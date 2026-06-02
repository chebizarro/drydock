package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"drydock/internal/circuitbreaker"
	"drydock/internal/codechat"
	"drydock/internal/codeindex"
	"drydock/internal/config"
	"drydock/internal/contextbuilder"
	"drydock/internal/contextvm"
	"drydock/internal/conversation"
	"drydock/internal/dashboard"
	"drydock/internal/db"
	"drydock/internal/driftguard"
	"drydock/internal/embedding"
	"drydock/internal/health"
	"drydock/internal/idegateway"
	"drydock/internal/ingest"
	"drydock/internal/listener"
	"drydock/internal/lspbridge"
	"drydock/internal/marketplace"
	"drydock/internal/metareview"
	"drydock/internal/metrics"
	"drydock/internal/nipingest"
	"drydock/internal/payment"
	"drydock/internal/pipeline"
	"drydock/internal/promptrefine"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/reviewengine"
	"drydock/internal/securityscan"
	"drydock/internal/signing"
	"drydock/internal/vectorstore"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
)

func main() {
	cfg := config.FromEnv()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// --- NIP ingest mode: run and exit ---
	if mode := os.Getenv("DRYDOCK_MODE"); mode == "nip-ingest" {
		runNIPIngest(cfg, logger)
		return
	}

	// --- Drift guard mode: export/flag/list and exit ---
	if mode := os.Getenv("DRYDOCK_MODE"); mode == "drift-guard" {
		runDriftGuard(cfg, logger)
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- Configuration validation (fail fast) ---
	logger.Info("validating configuration...")
	validationResult := cfg.Validate(ctx)
	validationResult.Log(logger)
	if validationResult.HasErrors() {
		logger.Error("configuration validation failed, exiting")
		os.Exit(1)
	}
	logger.Info("configuration validation passed")

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

	// --- Signer (chain: bunker → socket → DBus → nsec) ---
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
	}
	if signer == nil && (cfg.SignerSocketPath != "" || socketSignerAvailable()) {
		s, err := signing.NewSocketSigner(ctx, signing.SocketSignerConfig{
			SocketPath: cfg.SignerSocketPath,
		})
		if err != nil {
			logger.Warn("NIP-5F socket signer not available", "error", err)
		} else {
			signer = s
			logger.Info("NIP-5F socket signer ready")
		}
	}
	if signer == nil && cfg.SignerDBus {
		s, err := signing.NewDBusSigner(ctx, signing.DBusSignerConfig{})
		if err != nil {
			logger.Warn("NIP-55L DBus signer not available", "error", err)
		} else {
			signer = s
			logger.Info("NIP-55L DBus signer ready")
		}
	}
	if signer == nil && cfg.SignerNsec != "" {
		s, err := signing.NewLocalSigner(cfg.SignerNsec)
		if err != nil {
			logger.Error("failed to create local signer", "error", err)
			os.Exit(1)
		}
		signer = s
		logger.Info("local nsec signer ready")
	}
	if signer == nil {
		logger.Warn("no signer configured — review publishing disabled")
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
	relayPub := publisher.NewNostrRelayPublisher(pool, logger)

	// --- ContextVM transport (MCP-over-Nostr JSON-RPC foundation) ---
	contextVMTransport := contextvm.NewTransport(pool, signer, readRelays, writeRelays, logger)
	contextVMRouter := contextvm.NewRouter()
	logger.Info("contextvm transport initialized",
		"kind", int(contextvm.KindContextVM),
		"gift_wrap_kind", int(contextvm.KindGiftWrap),
		"enabled", contextVMTransport != nil && contextVMRouter != nil && signer != nil,
	)

	// --- Health check server ---
	healthAddr := cfg.HealthAddr
	healthSrv := health.New(store, logger)

	// --- Conversation handler ---
	var convHandler *conversation.Handler
	if signer != nil {
		convClient := reviewengine.NewCircuitBreakingClient(
			reviewengine.NewRetryingClient(
				reviewengine.NewOpenAICompatClient(),
				reviewengine.RetryConfig{MaxAttempts: 2},
				logger,
			),
			circuitbreaker.DefaultConfig(),
			logger,
		)
		convHandler = conversation.New(conversation.Config{
			Endpoint:      reviewengine.ModelEndpoint{BaseURL: cfg.PlannerBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.PlannerAPIKey), Model: cfg.PlannerModel},
			Temperature:   0.3,
			DefaultRelays: writeRelays,
			ResponseTTL:   30 * 24 * time.Hour,
		}, store, convClient, signer, relayPub, logger)
	}

	// --- Ingest / Listener ---
	var processorOpts []func(*ingest.Processor)
	if convHandler != nil {
		processorOpts = append(processorOpts, ingest.WithConversation(convHandler))
	}
	// Loop suppression: resolve our own signer pubkey so the processor can
	// skip auto-fix patches we publish (preventing recursive self-review).
	if signer != nil {
		if signerPubKey, err := signer.GetPublicKey(ctx); err == nil {
			processorOpts = append(processorOpts, ingest.WithLocalAutofixAuthor(signerPubKey.Hex()))
			logger.Info("autofix loop suppression enabled", "signer_pubkey", signerPubKey.Hex())
		} else {
			logger.Warn("failed to resolve signer pubkey for autofix loop suppression", "error", err)
		}
	}
	// --- Repo service ---
	repoManager := repo.NewManager(cfg.RepoCacheDir, logger,
		repo.WithMaxRepoCount(cfg.RepoCacheMaxCount),
		repo.WithMaxCacheSizeMB(cfg.RepoCacheMaxSizeMB),
	)
	repoSvc := repo.NewService(store, repoManager, logger)

	// --- Payment service (used when a repo enables payments in .drydock.yaml) ---
	var invoiceProvider payment.InvoiceProvider
	if cfg.PaymentNWCURI != "" {
		var err error
		invoiceProvider, err = payment.NewNWCInvoiceProvider(payment.NWCConfig{URI: cfg.PaymentNWCURI})
		if err != nil {
			logger.Error("failed to configure NWC payment provider", "error", err)
			os.Exit(1)
		}
	} else if len(cfg.PaymentTrustedMints) > 0 {
		logger.Warn("trusted Cashu mints configured but NWC connection is missing; Cashu token payments cannot be authorized")
	}
	mintClient := payment.NewCashuMintClient(10 * time.Second)
	paymentSvc := payment.New(payment.Config{
		TrustedMints: cfg.PaymentTrustedMints,
	}, store, invoiceProvider, mintClient, logger)
	logger.Info("payment service configured", "nwc_configured", invoiceProvider != nil, "trusted_mints", len(cfg.PaymentTrustedMints))

	// --- Optional service clients ---
	var builderOpts []func(*contextbuilder.BuilderOptions)

	// Qdrant + embedding
	var qdrantClient *vectorstore.Client
	var embedClient *embedding.Client
	var codeIndexer *codeindex.Indexer
	if cfg.QdrantURL != "" && cfg.EmbedBaseURL != "" {
		qdrantClient = vectorstore.NewClient(cfg.QdrantURL, cfg.QdrantAPIKey)
		embedClient = embedding.NewClient(cfg.EmbedBaseURL, cfg.EmbedAPIKey, cfg.EmbedModel)

		// Ensure collections exist (non-fatal).
		vectorDim := cfg.EmbedDimension
		for _, col := range []string{vectorstore.CollectionNIPSpecs, vectorstore.CollectionProjectDocs, vectorstore.CollectionFewShot, vectorstore.CollectionCodeChunks} {
			if err := qdrantClient.EnsureCollection(ctx, col, vectorDim); err != nil {
				logger.Warn("failed to ensure Qdrant collection", "collection", col, "error", err)
			}
		}

		builderOpts = append(builderOpts, contextbuilder.WithQdrant(qdrantClient, embedClient))

		// Code index provider + indexer.
		codeProvider := codeindex.NewProvider(qdrantClient, embedClient, logger)
		if codeProvider != nil {
			builderOpts = append(builderOpts, contextbuilder.WithExtraProviders(codeProvider))
		}
		codeIndexer = codeindex.New(qdrantClient, embedClient, logger, vectorDim)

		logger.Info("Qdrant + embedding configured", "qdrant", cfg.QdrantURL, "embed_model", cfg.EmbedModel, "embed_dimension", vectorDim)
	} else if cfg.QdrantURL != "" || cfg.EmbedBaseURL != "" {
		logger.Warn("both DRYDOCK_QDRANT_URL and DRYDOCK_EMBED_BASE_URL must be set for RAG features")
	}

	// LSP bridge
	var lspClient *lspbridge.Client
	if cfg.LSPBridgeURL != "" {
		lspClient = lspbridge.NewClient(cfg.LSPBridgeURL)
		if err := lspClient.Ping(ctx); err != nil {
			logger.Warn("LSP bridge not reachable, falling back to git grep", "url", cfg.LSPBridgeURL, "error", err)
			lspClient = nil
		} else {
			builderOpts = append(builderOpts, contextbuilder.WithLSPBridge(lspClient))
			logger.Info("LSP bridge connected", "url", cfg.LSPBridgeURL)
		}
	}

	// --- Security scanner ---
	secScanner := securityscan.New()
	secProvider := securityscan.NewProvider(secScanner)
	builderOpts = append(builderOpts, contextbuilder.WithExtraProviders(secProvider))
	logger.Info("security scanner enabled", "rules", len(securityscan.BuiltinRules()))

	// --- Context builder ---
	ctxBuilder := contextbuilder.NewWithOptions(contextbuilder.NewBuilderOptions(builderOpts...))

	// --- Review engine (with retry + circuit breaker for transient LLM failures) ---
	llmClient := reviewengine.NewCircuitBreakingClient(
		reviewengine.NewRetryingClient(
			reviewengine.NewOpenAICompatClient(),
			reviewengine.RetryConfig{MaxAttempts: 3},
			logger,
		),
		circuitbreaker.DefaultConfig(),
		logger,
	)
	engine := reviewengine.New(reviewengine.Config{
		Planner:      reviewengine.ModelEndpoint{BaseURL: cfg.PlannerBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.PlannerAPIKey), Model: cfg.PlannerModel},
		Coder32B:     reviewengine.ModelEndpoint{BaseURL: cfg.Coder32BBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.Coder32BAPIKey), Model: cfg.Coder32BModel},
		LLM70B:       reviewengine.ModelEndpoint{BaseURL: cfg.LLM70BBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.LLM70BAPIKey), Model: cfg.LLM70BModel},
		Coder14B:     reviewengine.ModelEndpoint{BaseURL: cfg.Coder14BBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.Coder14BAPIKey), Model: cfg.Coder14BModel},
		PlannerTemp:  0.1,
		ReviewerTemp: 0.2,
	}, llmClient, logger)

	// --- Publisher ---
	var pubSvc *publisher.Service
	if signer != nil {
		pubSvc = publisher.New(publisher.Config{
			DefaultRelays:       writeRelays,
			DetailSeverityFloor: "high",
			DefaultTTL:          90 * 24 * time.Hour,
			SupersededTTL:       7 * 24 * time.Hour,
		}, store, signer, relayPub, logger)
	}

	// --- Meta-review (with retry + circuit breaker) ---
	metaClient := reviewengine.NewCircuitBreakingClient(
		reviewengine.NewRetryingClient(
			reviewengine.NewOpenAICompatClient(),
			reviewengine.RetryConfig{MaxAttempts: 3},
			logger,
		),
		circuitbreaker.DefaultConfig(),
		logger,
	)
	var metaOpts []func(*metareview.Service)
	if qdrantClient != nil && embedClient != nil {
		metaOpts = append(metaOpts, metareview.WithQdrant(qdrantClient, embedClient))
	}
	metaSvc := metareview.New(metareview.Config{
		Endpoint:         reviewengine.ModelEndpoint{BaseURL: cfg.MetaBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.MetaAPIKey), Model: cfg.MetaModel},
		RandomSampleRate: 0.15,
		MinReuseJaccard:  0.85,
		FewShotCap:       500,
		MaxConcurrent:    2,
	}, store, metaClient, logger, metaOpts...)

	// --- Prompt refinement (reuses the meta-review LLM endpoint) ---
	prSvc := promptrefine.New(promptrefine.Config{
		Threshold:          promptrefine.DefaultThreshold,
		Endpoint:           reviewengine.ModelEndpoint{BaseURL: cfg.MetaBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.MetaAPIKey), Model: cfg.MetaModel},
		EvalScoreTolerance: 0.05,
	}, store, metaClient, logger)

	// --- Event handlers registered before subscribing ---
	if signer != nil && qdrantClient != nil && embedClient != nil {
		if keyer, ok := signer.(codechat.Keyer); ok {
			codeChatHandler := codechat.New(codechat.Config{
				Endpoint:      reviewengine.ModelEndpoint{BaseURL: cfg.PlannerBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.PlannerAPIKey), Model: cfg.PlannerModel},
				Temperature:   0.4,
				DefaultRelays: writeRelays,
			}, store, qdrantClient, embedClient, llmClient, keyer, relayPub, logger)
			processorOpts = append(processorOpts, ingest.WithCodeChat(codeChatHandler))
			logger.Info("codechat handler registered")
		} else {
			logger.Warn("codechat handler disabled", "requires", "signer with encryption/decryption support")
		}
	} else {
		logger.Warn("codechat handler disabled", "requires", "signer and Qdrant+embedding")
	}

	if signer != nil {
		ideHandler := idegateway.New(idegateway.Config{DefaultRelays: writeRelays}, store, ctxBuilder, engine, signer, relayPub, logger)
		processorOpts = append(processorOpts, ingest.WithIDEGateway(ideHandler))
		if err := contextvm.RegisterIDEMethods(contextVMRouter, ideHandler); err != nil {
			logger.Error("failed to register IDE ContextVM handlers", "error", err)
			os.Exit(1)
		}
		processorOpts = append(processorOpts, ingest.WithContextVM(contextVMRouter, contextVMTransport))
		logger.Info("IDE gateway handler registered")

		marketRegistry := marketplace.NewRegistry(store, logger)
		marketRouter := marketplace.NewRouter(marketplace.RouterConfig{DefaultRelays: writeRelays}, marketRegistry, store, signer, relayPub, contextVMTransport, logger)
		marketHandler := marketplace.NewHandler(marketRegistry, marketRouter, store, logger)
		if err := contextvm.RegisterMarketplaceMethods(contextVMRouter, marketHandler); err != nil {
			logger.Error("failed to register marketplace contextvm methods", "error", err)
			os.Exit(1)
		}
		processorOpts = append(processorOpts, ingest.WithMarketplace(marketHandler))
		logger.Info("marketplace handler registered")
	} else {
		logger.Warn("IDE gateway and marketplace handlers disabled", "requires", "signer")
	}

	processor := ingest.NewProcessor(store, logger, processorOpts...)
	svc := listener.New(listener.Config{
		Relays:          readRelays,
		LookbackMinutes: cfg.ListenerLookbackMin,
	}, processor, logger, listener.WithPool(pool), listener.WithStore(store))

	// --- Pipeline runner ---
	var pipelineRunner *pipeline.Runner
	if pubSvc != nil {
		var pipelineOpts []func(*pipeline.Runner)
		pipelineOpts = append(pipelineOpts, pipeline.WithPromptRefiner(prSvc))
		pipelineOpts = append(pipelineOpts, pipeline.WithSecurityScanner(secScanner))
		pipelineOpts = append(pipelineOpts, pipeline.WithActivityHeartbeat(healthSrv.RecordActivity))
		if paymentSvc != nil {
			pipelineOpts = append(pipelineOpts, pipeline.WithPaymentAuthorizer(paymentSvc))
		}
		if codeIndexer != nil {
			pipelineOpts = append(pipelineOpts, pipeline.WithCodeIndexer(codeIndexer))
		}
		if qdrantClient != nil && embedClient != nil {
			pipelineOpts = append(pipelineOpts,
				pipeline.WithFewShotRetriever(
					pipeline.NewQdrantRetriever(qdrantClient, embedClient, store, logger)))
		}
		pipelineRunner = pipeline.New(
			pipeline.Config{Workers: cfg.PipelineWorkers},
			store,
			repoSvc,
			ctxBuilder,
			engine,
			pubSvc,
			metaSvc,
			processor.ReviewQueue,
			logger,
			pipelineOpts...,
		)
	} else {
		logger.Warn("pipeline runner disabled (no signer configured)")
	}

	// --- Analytics dashboard ---
	dash := dashboard.New(store, logger)
	dash.Register(healthSrv.Mux())
	logger.Info("analytics dashboard enabled", "path", "/dashboard/")

	go func() {
		if err := healthSrv.ListenAndServe(healthAddr); err != nil {
			logger.Error("health server error", "error", err)
		}
	}()

	// --- Run ---
	errCh := make(chan error, 2)

	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		if err := svc.Run(ctx); err != nil {
			errCh <- err
		}
	}()

	pipelineDone := make(chan struct{})
	if pipelineRunner != nil {
		go func() {
			defer close(pipelineDone)
			pipelineRunner.Run(ctx)
		}()
	} else {
		close(pipelineDone)
	}

	// --- Background prompt refinement loop (checks every 5 minutes) ---
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := prSvc.CheckAndRefine(ctx)
				if err != nil {
					logger.Warn("prompt refinement check failed", "error", err)
				} else if result.Triggered {
					logger.Info("prompt refinement triggered",
						"gaps_processed", result.GapsProcessed,
						"new_version_id", result.NewVersionID,
					)
				}
			}
		}
	}()

	// --- Background failed-review requeue sweep (every 10 minutes) ---
	// Recovers tasks that failed due to transient issues (queue overflow,
	// temporary LLM failures) by moving them back to pending after a cooldown.
	if pipelineRunner != nil {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Requeue tasks that have been in "failed" state for at least 5 minutes.
					tasks, err := store.RequeueFailedReviews(ctx, 300, 20)
					if err != nil {
						logger.Warn("failed review requeue sweep error", "error", err)
					} else if len(tasks) > 0 {
						metrics.ReviewsRequeued.Add(int64(len(tasks)))
						logger.Info("requeued failed reviews", "count", len(tasks))
						for _, task := range tasks {
							select {
							case processor.ReviewQueue <- task:
							default:
								logger.Warn("review queue still full during requeue",
									"patch_event_id", task.PatchEventID)
							}
						}
					}
				}
			}
		}()
	}

	// --- Background marketplace assignment expiry (checks every 5 minutes) ---
	expirySvc := marketplace.NewExpiryService(marketplace.DefaultExpiryConfig(), store, logger)
	go expirySvc.Run(ctx)

	healthSrv.SetReady(true)

	select {
	case err := <-errCh:
		logger.Error("service exited with error", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutting down, waiting for in-flight work to drain")
	}

	// Mark unhealthy so load balancer stops sending traffic during drain.
	healthSrv.SetReady(false)

	// Graceful drain: wait for pipeline and listener to finish, with a deadline.
	const drainTimeout = 60 * time.Second
	drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer drainCancel()

	allDone := make(chan struct{})
	go func() {
		<-pipelineDone
		<-listenerDone
		close(allDone)
	}()

	select {
	case <-allDone:
		logger.Info("graceful shutdown complete")
	case <-drainCtx.Done():
		logger.Warn("graceful shutdown timed out, exiting", "timeout", drainTimeout)
	}

	// Shut down the health server after draining work.
	if err := healthSrv.Shutdown(drainCtx); err != nil {
		logger.Warn("health server shutdown error", "error", err)
	}
}

// runDriftGuard runs the convention drift guard CLI and exits.
func runDriftGuard(cfg config.Config, logger *slog.Logger) {
	ctx := context.Background()

	store, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	svc := driftguard.NewService(store, logger)

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"export"}
	}

	switch args[0] {
	case "export":
		n := 20
		if len(args) > 1 {
			if _, err := fmt.Sscanf(args[1], "%d", &n); err != nil {
				logger.Error("invalid sample size", "arg", args[1])
				os.Exit(1)
			}
		}
		count, err := svc.ExportSample(ctx, os.Stdout, n)
		if err != nil {
			logger.Error("export failed", "error", err)
			os.Exit(1)
		}
		logger.Info("drift guard export complete", "count", count)

	case "flag":
		if len(args) < 2 {
			logger.Error("usage: drydock flag <meta-review-id> [notes]")
			os.Exit(1)
		}
		var id int64
		if _, err := fmt.Sscanf(args[1], "%d", &id); err != nil {
			logger.Error("invalid meta-review ID", "arg", args[1])
			os.Exit(1)
		}
		notes := ""
		if len(args) > 2 {
			notes = strings.Join(args[2:], " ")
		}
		if err := svc.FlagReview(ctx, id, notes); err != nil {
			logger.Error("flag failed", "error", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "Flagged meta-review %d as convention drift.\n", id)

	case "list":
		count, err := svc.ListFlagged(ctx, os.Stdout)
		if err != nil {
			logger.Error("list failed", "error", err)
			os.Exit(1)
		}
		logger.Info("drift guard list complete", "count", count)

	default:
		logger.Error("unknown drift-guard subcommand", "cmd", args[0], "valid", "export, flag, list")
		os.Exit(1)
	}
}

// runNIPIngest runs the NIP spec ingest pipeline and exits.
func runNIPIngest(cfg config.Config, logger *slog.Logger) {
	if cfg.NIPsDir == "" {
		logger.Error("DRYDOCK_NIPS_DIR must be set for nip-ingest mode")
		os.Exit(1)
	}
	if cfg.QdrantURL == "" || cfg.EmbedBaseURL == "" {
		logger.Error("DRYDOCK_QDRANT_URL and DRYDOCK_EMBED_BASE_URL must be set for nip-ingest mode")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	qdrantClient := vectorstore.NewClient(cfg.QdrantURL, cfg.QdrantAPIKey)
	embedClient := embedding.NewClient(cfg.EmbedBaseURL, cfg.EmbedAPIKey, cfg.EmbedModel)

	ingester := nipingest.NewIngester(qdrantClient, embedClient, logger)
	n, err := ingester.Run(ctx, nipingest.Config{
		NIPsDir:   cfg.NIPsDir,
		VectorDim: cfg.EmbedDimension,
	})
	if err != nil {
		logger.Error("NIP ingest failed", "error", err)
		os.Exit(1)
	}
	logger.Info("NIP ingest complete", "chunks_upserted", n)
}

// socketSignerAvailable checks if the default NIP-5F socket path exists.
func socketSignerAvailable() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(home + "/.local/share/nostr/signer.sock")
	return err == nil
}
