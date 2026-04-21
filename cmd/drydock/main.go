package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"drydock/internal/config"
	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/ingest"
	"drydock/internal/listener"
	"drydock/internal/metareview"
	"drydock/internal/pipeline"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/reviewengine"
	"drydock/internal/signing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"

)

func main() {
	cfg := config.FromEnv()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- Database ---
	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		logger.Error("failed to migrate database", "error", err)
		os.Exit(1)
	}

	// Reset any entries stuck in "reviewing" from a prior crash
	if n, err := store.ResetStuckReviews(ctx); err != nil {
		logger.Warn("failed to reset stuck reviews", "error", err)
	} else if n > 0 {
		logger.Info("reset stuck reviews to pending", "count", n)
	}

	// --- Signer ---
	var signer publisher.Signer
	if cfg.SignerBunkerURL != "" {
		s, err := signing.NewBunkerSigner(ctx, signing.BunkerSignerConfig{
			BunkerURL: cfg.SignerBunkerURL,
			OnAuthURL: func(url string) {
				logger.Info("bunker auth required", "url", url)
			},
		})
		if err != nil {
			logger.Error("failed to create bunker signer", "error", err)
			os.Exit(1)
		}
		signer = s
		logger.Info("NIP-46 bunker signer ready")
	} else if cfg.SignerNsec != "" {
		s, err := signing.NewLocalSigner(cfg.SignerNsec)
		if err != nil {
			logger.Error("failed to create local signer", "error", err)
			os.Exit(1)
		}
		signer = s
		logger.Info("local nsec signer ready")
	} else {
		logger.Warn("no signer configured (set DRYDOCK_SIGNER_BUNKER_URL or DRYDOCK_SIGNER_NSEC) — review publishing disabled")
	}

	// --- Shared Nostr pool (with NIP-42 AUTH if signer available) ---
	poolOpts := nostr.PoolOptions{}
	if signer != nil {
		poolOpts.AuthRequiredHandler = func(authCtx context.Context, evt *nostr.Event) error {
			return signer.SignEvent(authCtx, evt)
		}
		logger.Info("NIP-42 relay auth handler enabled")
	}
	pool := nostr.NewPool(poolOpts)

	// --- NIP-11 relay capability probe (non-blocking, log-only) ---
	allRelays := make(map[string]struct{})
	for _, r := range cfg.Relays {
		allRelays[r] = struct{}{}
	}
	for _, r := range cfg.ReadRelays {
		allRelays[r] = struct{}{}
	}
	for _, r := range cfg.WriteRelays {
		allRelays[r] = struct{}{}
	}
	for relayURL := range allRelays {
		info, err := nip11.Fetch(ctx, relayURL)
		if err != nil {
			logger.Warn("NIP-11 probe failed", "relay", relayURL, "error", err)
			continue
		}
		logger.Info("relay probed",
			"relay", relayURL,
			"name", info.Name,
			"software", info.Software,
			"supported_nips", info.SupportedNIPs,
		)
		if info.Limitation != nil {
			if info.Limitation.AuthRequired && signer == nil {
				logger.Warn("relay requires auth but no signer configured",
					"relay", relayURL,
				)
			}
			if info.Limitation.PaymentRequired {
				logger.Warn("relay requires payment", "relay", relayURL)
			}
		}
	}

	// --- Relay lists (read/write separation with fallback to DRYDOCK_RELAYS) ---
	readRelays := cfg.ReadRelays
	if len(readRelays) == 0 {
		readRelays = cfg.Relays
	}
	writeRelays := cfg.WriteRelays
	if len(writeRelays) == 0 {
		writeRelays = cfg.Relays
	}

	// --- Ingest / Listener ---
	processor := ingest.NewProcessor(store, logger)
	svc := listener.New(listener.Config{
		Relays:          readRelays,
		LookbackMinutes: cfg.ListenerLookbackMin,
	}, processor, logger, listener.WithPool(pool), listener.WithStore(store))

	// --- Repo service ---
	repoManager := repo.NewManager(cfg.RepoCacheDir, logger,
		repo.WithMaxRepoCount(cfg.RepoCacheMaxCount),
		repo.WithMaxCacheSizeMB(cfg.RepoCacheMaxSizeMB),
	)
	repoSvc := repo.NewService(store, repoManager, logger)

	// --- Context builder ---
	ctxBuilder := contextbuilder.NewDefault()

	// --- Review engine (with retry for transient LLM failures) ---
	llmClient := reviewengine.NewRetryingClient(
		reviewengine.NewOpenAICompatClient(),
		reviewengine.RetryConfig{MaxAttempts: 3},
		logger,
	)
	engine := reviewengine.New(reviewengine.Config{
		Planner:      reviewengine.ModelEndpoint{BaseURL: cfg.PlannerBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.PlannerModel},
		Coder32B:     reviewengine.ModelEndpoint{BaseURL: cfg.Coder32BBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.Coder32BModel},
		LLM70B:       reviewengine.ModelEndpoint{BaseURL: cfg.LLM70BBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLM70BModel},
		Coder14B:     reviewengine.ModelEndpoint{BaseURL: cfg.Coder14BBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.Coder14BModel},
		PlannerTemp:  0.1,
		ReviewerTemp: 0.2,
	}, llmClient, logger)

	// --- Publisher ---
	var pubSvc *publisher.Service
	if signer != nil {
		relayPub := publisher.NewNostrRelayPublisher(pool, logger)
		pubSvc = publisher.New(publisher.Config{
			DefaultRelays:       writeRelays,
			DetailSeverityFloor: "high",
			DefaultTTL:          90 * 24 * time.Hour,
			SupersededTTL:       7 * 24 * time.Hour,
		}, store, signer, relayPub, logger)
	}

	// --- Meta-review (with retry) ---
	metaClient := reviewengine.NewRetryingClient(
		reviewengine.NewOpenAICompatClient(),
		reviewengine.RetryConfig{MaxAttempts: 3},
		logger,
	)
	metaSvc := metareview.New(metareview.Config{
		Endpoint:         reviewengine.ModelEndpoint{BaseURL: cfg.MetaBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.MetaModel},
		RandomSampleRate: 0.15,
		MinReuseJaccard:  0.85,
		FewShotCap:       500,
		MaxConcurrent:    2,
	}, store, metaClient, logger)

	// --- Pipeline runner ---
	var pipelineRunner *pipeline.Runner
	if pubSvc != nil {
		pipelineRunner = pipeline.New(
			pipeline.Config{Workers: 2},
			store,
			repoSvc,
			ctxBuilder,
			engine,
			pubSvc,
			metaSvc,
			processor.ReviewQueue,
			logger,
		)
	} else {
		logger.Warn("pipeline runner disabled (no signer configured)")
	}

	// --- Run ---
	errCh := make(chan error, 2)

	go func() {
		if err := svc.Run(ctx); err != nil {
			errCh <- err
		}
	}()

	if pipelineRunner != nil {
		go pipelineRunner.Run(ctx)
	}

	select {
	case err := <-errCh:
		logger.Error("service exited with error", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutting down")
	}
}


