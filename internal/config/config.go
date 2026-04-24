package config

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL         string
	RepoCacheDir        string
	RepoCacheMaxCount   int
	RepoCacheMaxSizeMB  int
	Relays              []string
	ReadRelays          []string
	WriteRelays         []string
	LogLevel            slog.Level
	ListenerLookbackMin int

	PlannerBaseURL  string
	PlannerModel    string
	Coder32BBaseURL string
	Coder32BModel   string
	LLM70BBaseURL   string
	LLM70BModel     string
	Coder14BBaseURL string
	Coder14BModel   string
	LLMAPIKey       string
	SignerBunkerURL    string
	SignerNsec         string
	SignerSocketPath   string
	SignerDBus         bool
	QdrantURL       string
	QdrantAPIKey    string
	EmbedBaseURL    string
	EmbedModel      string
	EmbedAPIKey     string
	LSPBridgeURL    string
	MetaBaseURL     string
	MetaModel       string
	EvalDatasetPath string
	NIPsDir         string
	HealthAddr      string
	PipelineWorkers int
}

func FromEnv() Config {
	return Config{
		DatabaseURL:         envOrDefault("DRYDOCK_DATABASE_URL", "file:drydock.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)"),
		RepoCacheDir:        envOrDefault("DRYDOCK_REPO_CACHE_DIR", "repos"),
		RepoCacheMaxCount:   parseIntOrDefault(envOrDefault("DRYDOCK_REPO_CACHE_MAX_COUNT", "50"), 50),
		RepoCacheMaxSizeMB:  parseIntOrDefault(envOrDefault("DRYDOCK_REPO_CACHE_MAX_SIZE_MB", "10240"), 10240),
		Relays: splitCSV(
			envOrDefault(
				"DRYDOCK_RELAYS",
				"wss://relay.damus.io,wss://nos.lol,wss://relay.primal.net",
			),
		),
		ReadRelays:  splitCSV(envOrDefault("DRYDOCK_READ_RELAYS", "")),
		WriteRelays: splitCSV(envOrDefault("DRYDOCK_WRITE_RELAYS", "")),
		LogLevel:            parseLogLevel(envOrDefault("DRYDOCK_LOG_LEVEL", "info")),
		ListenerLookbackMin: parseIntOrDefault(envOrDefault("DRYDOCK_LISTENER_LOOKBACK_MIN", "5"), 5),
		PlannerBaseURL:      envOrDefault("DRYDOCK_PLANNER_BASE_URL", "http://127.0.0.1:11434/v1"),
		PlannerModel:        envOrDefault("DRYDOCK_PLANNER_MODEL", "qwen2.5-coder-14b-instruct-q4_k_m"),
		Coder32BBaseURL:     envOrDefault("DRYDOCK_CODER32B_BASE_URL", "http://127.0.0.1:11434/v1"),
		Coder32BModel:       envOrDefault("DRYDOCK_CODER32B_MODEL", "qwen2.5-coder-32b-instruct-q4_k_m"),
		LLM70BBaseURL:       envOrDefault("DRYDOCK_LLM70B_BASE_URL", "http://127.0.0.1:11435/v1"),
		LLM70BModel:         envOrDefault("DRYDOCK_LLM70B_MODEL", "llama-3.3-70b-instruct-q4_k_m"),
		Coder14BBaseURL:     envOrDefault("DRYDOCK_CODER14B_BASE_URL", "http://127.0.0.1:11434/v1"),
		Coder14BModel:       envOrDefault("DRYDOCK_CODER14B_MODEL", "qwen2.5-coder-14b-instruct-q4_k_m"),
		LLMAPIKey:           envOrDefault("DRYDOCK_LLM_API_KEY", ""),
		SignerBunkerURL:     envOrDefault("DRYDOCK_SIGNER_BUNKER_URL", ""),
		SignerNsec:          envOrDefault("DRYDOCK_SIGNER_NSEC", ""),
		SignerSocketPath:    envOrDefault("DRYDOCK_SIGNER_SOCKET_PATH", ""),
		SignerDBus:          envOrDefault("DRYDOCK_SIGNER_DBUS", "") == "true",
		QdrantURL:           envOrDefault("DRYDOCK_QDRANT_URL", ""),
		QdrantAPIKey:        envOrDefault("DRYDOCK_QDRANT_API_KEY", ""),
		EmbedBaseURL:        envOrDefault("DRYDOCK_EMBED_BASE_URL", ""),
		EmbedModel:          envOrDefault("DRYDOCK_EMBED_MODEL", "nomic-embed-text"),
		EmbedAPIKey:         envOrDefault("DRYDOCK_EMBED_API_KEY", ""),
		LSPBridgeURL:        envOrDefault("DRYDOCK_LSP_BRIDGE_URL", ""),
		MetaBaseURL:         envOrDefault("DRYDOCK_META_BASE_URL", "http://127.0.0.1:11436/v1"),
		MetaModel:           envOrDefault("DRYDOCK_META_MODEL", "llama-3.3-70b-instruct-q4_k_m"),
		EvalDatasetPath:     envOrDefault("DRYDOCK_EVAL_DATASET_PATH", "eval/heldout-sample.json"),
		NIPsDir:             envOrDefault("DRYDOCK_NIPS_DIR", ""),
		HealthAddr:          envOrDefault("DRYDOCK_HEALTH_ADDR", ":8081"),
		PipelineWorkers:     parseIntOrDefault(envOrDefault("DRYDOCK_PIPELINE_WORKERS", "2"), 2),
	}
}

func splitCSV(v string) []string {
	raw := strings.Split(v, ",")
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		trimmed := strings.TrimSpace(item)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseLogLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func parseIntOrDefault(v string, fallback int) int {
	var result int
	if _, err := fmt.Sscanf(v, "%d", &result); err != nil {
		return fallback
	}
	return result
}

// ValidationResult contains the outcome of configuration validation.
type ValidationResult struct {
	Errors   []string // Fatal errors that prevent startup
	Warnings []string // Non-fatal issues that may cause degraded operation
}

// HasErrors returns true if there are any fatal validation errors.
func (v ValidationResult) HasErrors() bool {
	return len(v.Errors) > 0
}

// Log outputs all errors and warnings to the provided logger.
func (v ValidationResult) Log(logger *slog.Logger) {
	for _, err := range v.Errors {
		logger.Error("configuration error", "message", err)
	}
	for _, warn := range v.Warnings {
		logger.Warn("configuration warning", "message", warn)
	}
}

// Validate checks the configuration for errors and warnings.
// It performs connectivity checks where possible to fail fast.
func (c *Config) Validate(ctx context.Context) ValidationResult {
	result := ValidationResult{}

	// --- Required: At least one relay ---
	if len(c.Relays) == 0 && len(c.ReadRelays) == 0 {
		result.Errors = append(result.Errors, "no relays configured: set DRYDOCK_RELAYS or DRYDOCK_READ_RELAYS")
	}

	// --- Validate relay URLs ---
	allRelays := append(append([]string{}, c.Relays...), c.ReadRelays...)
	allRelays = append(allRelays, c.WriteRelays...)
	for _, relay := range allRelays {
		if relay == "" {
			continue
		}
		u, err := url.Parse(relay)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("invalid relay URL %q: %v", relay, err))
			continue
		}
		if u.Scheme != "wss" && u.Scheme != "ws" {
			result.Errors = append(result.Errors, fmt.Sprintf("relay URL must use ws:// or wss:// scheme: %q", relay))
		}
	}

	// --- Signer configuration ---
	hasSignerConfig := c.SignerBunkerURL != "" || c.SignerNsec != "" || c.SignerSocketPath != "" || c.SignerDBus
	if !hasSignerConfig {
		result.Warnings = append(result.Warnings, "no signer configured: review publishing will be disabled (set DRYDOCK_SIGNER_BUNKER_URL, DRYDOCK_SIGNER_NSEC, or enable DRYDOCK_SIGNER_DBUS)")
	}

	// --- Database connectivity ---
	if err := c.validateDatabase(ctx); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("database validation failed: %v", err))
	}

	// --- LLM endpoint checks (warnings only, as they may come online later) ---
	llmEndpoints := map[string]string{
		"planner": c.PlannerBaseURL,
		"70b":     c.LLM70BBaseURL,
	}
	for name, baseURL := range llmEndpoints {
		if baseURL == "" {
			continue
		}
		if err := c.checkLLMEndpoint(ctx, baseURL); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("LLM endpoint %s (%s) not reachable: %v", name, baseURL, err))
		}
	}

	// --- Optional service warnings ---
	if c.QdrantURL != "" && c.EmbedBaseURL == "" {
		result.Warnings = append(result.Warnings, "DRYDOCK_QDRANT_URL set but DRYDOCK_EMBED_BASE_URL not set: RAG features disabled")
	}
	if c.EmbedBaseURL != "" && c.QdrantURL == "" {
		result.Warnings = append(result.Warnings, "DRYDOCK_EMBED_BASE_URL set but DRYDOCK_QDRANT_URL not set: RAG features disabled")
	}

	// --- Pipeline workers ---
	if c.PipelineWorkers < 1 {
		result.Errors = append(result.Errors, "DRYDOCK_PIPELINE_WORKERS must be at least 1")
	}
	if c.PipelineWorkers > 32 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("DRYDOCK_PIPELINE_WORKERS=%d is very high; consider reducing for stability", c.PipelineWorkers))
	}

	// --- Health address ---
	if c.HealthAddr != "" {
		if _, err := url.Parse("http://" + c.HealthAddr); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("invalid DRYDOCK_HEALTH_ADDR %q: %v", c.HealthAddr, err))
		}
	}

	return result
}

// validateDatabase checks that the database can be opened and is writable.
func (c *Config) validateDatabase(ctx context.Context) error {
	// Parse the DSN to ensure it's valid
	if c.DatabaseURL == "" {
		return fmt.Errorf("database URL is empty")
	}

	// Try to open and ping
	db, err := sql.Open("sqlite3", c.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to open: %w", err)
	}
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("failed to ping: %w", err)
	}

	// Try a write operation
	_, err = db.ExecContext(pingCtx, "CREATE TABLE IF NOT EXISTS _config_validation_test (id INTEGER); DROP TABLE IF EXISTS _config_validation_test;")
	if err != nil {
		return fmt.Errorf("database not writable: %w", err)
	}

	return nil
}

// checkLLMEndpoint verifies an LLM endpoint is reachable.
func (c *Config) checkLLMEndpoint(ctx context.Context, baseURL string) error {
	// Try to hit the models endpoint (common for OpenAI-compatible APIs)
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Second}
	
	// Try /v1/models or just /models
	endpoints := []string{
		strings.TrimSuffix(baseURL, "/") + "/models",
		strings.TrimSuffix(baseURL, "/v1") + "/v1/models",
	}

	var lastErr error
	for _, endpoint := range endpoints {
		req, err := http.NewRequestWithContext(checkCtx, "GET", endpoint, nil)
		if err != nil {
			lastErr = err
			continue
		}
		
		if c.LLMAPIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.LLMAPIKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		// Any response (even 401/403) means the endpoint is reachable
		if resp.StatusCode < 500 {
			return nil
		}
		lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
	}

	return lastErr
}
