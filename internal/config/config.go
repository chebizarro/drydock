package config

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"drydock/internal/vectorstore"

	_ "modernc.org/sqlite" // Register sqlite driver for validation
)

const (
	defaultRelays          = "wss://relay.damus.io,wss://nos.lol,wss://relay.primal.net"
	defaultPlannerBaseURL  = "http://127.0.0.1:11434/v1"
	defaultPlannerModel    = "qwen2.5-coder-14b-instruct-q4_k_m"
	defaultCoder32BBaseURL = "http://127.0.0.1:11434/v1"
	defaultCoder32BModel   = "qwen2.5-coder-32b-instruct-q4_k_m"
	defaultLLM70BBaseURL   = "http://127.0.0.1:11435/v1"
	defaultLLM70BModel     = "llama-3.3-70b-instruct-q4_k_m"
	defaultCoder14BBaseURL = "http://127.0.0.1:11434/v1"
	defaultCoder14BModel   = "qwen2.5-coder-14b-instruct-q4_k_m"
	defaultMetaBaseURL     = "http://127.0.0.1:11436/v1"
	defaultMetaModel       = "llama-3.3-70b-instruct-q4_k_m"
	defaultEmbedModel      = "nomic-embed-text"
)

var defaultPublicRelaySet = map[string]struct{}{
	"wss://relay.damus.io":   {},
	"wss://nos.lol":          {},
	"wss://relay.primal.net": {},
}

type Config struct {
	Environment string
	Production  bool
	ExplicitEnv map[string]bool

	DatabaseURL           string
	RepoCacheDir          string
	RepoCacheMaxCount     int
	RepoCacheMaxSizeMB    int
	Relays                []string
	ReadRelays            []string
	WriteRelays           []string
	LogLevel              slog.Level
	ListenerLookbackMin   int
	ListenerHWMOverlap    time.Duration
	ListenerMaxFutureSkew time.Duration
	ListenerMaxEventAge   time.Duration

	PlannerBaseURL             string
	PlannerModel               string
	Coder32BBaseURL            string
	Coder32BModel              string
	LLM70BBaseURL              string
	LLM70BModel                string
	Coder14BBaseURL            string
	Coder14BModel              string
	LLMAPIKey                  string
	PlannerAPIKey              string
	Coder32BAPIKey             string
	LLM70BAPIKey               string
	Coder14BAPIKey             string
	MetaAPIKey                 string
	SignerBunkerURL            string
	SignerNsec                 string
	SignerNsecFile             string
	DevMode                    bool
	ChartroomURL               string
	ChartroomToken             string
	ChartroomCorpusIDs         []string
	ChartroomSourceIDs         []string
	QdrantURL                  string
	QdrantAPIKey               string
	QdrantCollections          vectorstore.CollectionNames
	QdrantResultsPerCollection int
	EmbedBaseURL               string
	EmbedModel                 string
	EmbedAPIKey                string
	EmbedDimension             int
	PaymentNWCURI              string
	PaymentTrustedMints        []string
	LSPBridgeURL               string
	MetaBaseURL                string
	MetaModel                  string
	EvalDatasetPath            string
	HealthAddr                 string
	DashboardBearerToken       string
	PipelineWorkers            int
	CodeChatLimit              int
	CodeChatWindow             time.Duration
	FeedbackLimit              int
	FeedbackWindow             time.Duration
}

func FromEnv() Config {
	environment := strings.ToLower(strings.TrimSpace(os.Getenv("DRYDOCK_ENV")))
	production := isProductionMode(environment, os.Getenv("DRYDOCK_PRODUCTION"))
	defaultCollections := vectorstore.DefaultCollectionNames()

	signerNsec := envOrDefault("DRYDOCK_SIGNER_NSEC", "")
	signerNsecFile := envOrDefault("DRYDOCK_SIGNER_NSEC_FILE", "")
	if signerNsecFile != "" {
		nsecFromFile, err := os.ReadFile(signerNsecFile)
		if err != nil {
			slog.Warn("failed to read signer nsec file", "path", signerNsecFile, "error", err)
		} else {
			signerNsec = strings.TrimSpace(string(nsecFromFile))
		}
	}

	return Config{
		Environment:        environment,
		Production:         production,
		ExplicitEnv:        configuredEnv(),
		DatabaseURL:        envOrDefault("DRYDOCK_DATABASE_URL", "file:drydock.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)"),
		RepoCacheDir:       envOrDefault("DRYDOCK_REPO_CACHE_DIR", "repos"),
		RepoCacheMaxCount:  parseIntOrDefault(envOrDefault("DRYDOCK_REPO_CACHE_MAX_COUNT", "50"), 50),
		RepoCacheMaxSizeMB: parseIntOrDefault(envOrDefault("DRYDOCK_REPO_CACHE_MAX_SIZE_MB", "10240"), 10240),
		Relays: splitCSV(
			envOrDefault(
				"DRYDOCK_RELAYS",
				devDefault(production, defaultRelays),
			),
		),
		ReadRelays:            splitCSV(envOrDefault("DRYDOCK_READ_RELAYS", "")),
		WriteRelays:           splitCSV(envOrDefault("DRYDOCK_WRITE_RELAYS", "")),
		LogLevel:              parseLogLevel(envOrDefault("DRYDOCK_LOG_LEVEL", "info")),
		ListenerLookbackMin:   parseIntOrDefault(envOrDefault("DRYDOCK_LISTENER_LOOKBACK_MIN", "5"), 5),
		ListenerHWMOverlap:    parseDurationOrDefault(envOrDefault("DRYDOCK_LISTENER_HWM_OVERLAP", "30s"), 30*time.Second),
		ListenerMaxFutureSkew: parseDurationOrDefault(envOrDefault("DRYDOCK_LISTENER_MAX_FUTURE_SKEW", "10m"), 10*time.Minute),
		ListenerMaxEventAge:   parseDurationOrDefault(envOrDefault("DRYDOCK_LISTENER_MAX_EVENT_AGE", "8760h"), 365*24*time.Hour),
		PlannerBaseURL:        envOrDefault("DRYDOCK_PLANNER_BASE_URL", devDefault(production, defaultPlannerBaseURL)),
		PlannerModel:          envOrDefault("DRYDOCK_PLANNER_MODEL", devDefault(production, defaultPlannerModel)),
		Coder32BBaseURL:       envOrDefault("DRYDOCK_CODER32B_BASE_URL", devDefault(production, defaultCoder32BBaseURL)),
		Coder32BModel:         envOrDefault("DRYDOCK_CODER32B_MODEL", devDefault(production, defaultCoder32BModel)),
		LLM70BBaseURL:         envOrDefault("DRYDOCK_LLM70B_BASE_URL", devDefault(production, defaultLLM70BBaseURL)),
		LLM70BModel:           envOrDefault("DRYDOCK_LLM70B_MODEL", devDefault(production, defaultLLM70BModel)),
		Coder14BBaseURL:       envOrDefault("DRYDOCK_CODER14B_BASE_URL", devDefault(production, defaultCoder14BBaseURL)),
		Coder14BModel:         envOrDefault("DRYDOCK_CODER14B_MODEL", devDefault(production, defaultCoder14BModel)),
		LLMAPIKey:             envOrDefault("DRYDOCK_LLM_API_KEY", ""),
		PlannerAPIKey:         envOrDefault("DRYDOCK_PLANNER_API_KEY", ""),
		Coder32BAPIKey:        envOrDefault("DRYDOCK_CODER32B_API_KEY", ""),
		LLM70BAPIKey:          envOrDefault("DRYDOCK_LLM70B_API_KEY", ""),
		Coder14BAPIKey:        envOrDefault("DRYDOCK_CODER14B_API_KEY", ""),
		MetaAPIKey:            envOrDefault("DRYDOCK_META_API_KEY", ""),
		SignerBunkerURL:       envOrDefault("DRYDOCK_SIGNER_BUNKER_URL", ""),
		SignerNsec:            signerNsec,
		SignerNsecFile:        signerNsecFile,
		DevMode:               parseBoolOrDefault(envOrDefault("DEV_MODE", envOrDefault("DRYDOCK_DEV_MODE", "")), false),
		ChartroomURL:          envOrDefault("DRYDOCK_CHARTROOM_URL", ""),
		ChartroomToken:        envOrDefault("DRYDOCK_CHARTROOM_TOKEN", envOrDefault("CHARTROOM_HTTP_BEARER_TOKEN", "")),
		ChartroomCorpusIDs:    splitCSV(envOrDefault("DRYDOCK_CHARTROOM_CORPUS_IDS", "")),
		ChartroomSourceIDs:    splitCSV(envOrDefault("DRYDOCK_CHARTROOM_SOURCE_IDS", envOrDefault("DRYDOCK_CHARTROOM_SOURCES", ""))),
		QdrantURL:             envOrDefault("DRYDOCK_QDRANT_URL", ""),
		QdrantAPIKey:          envOrDefault("DRYDOCK_QDRANT_API_KEY", ""),
		QdrantCollections: vectorstore.CollectionNames{
			NIPSpecs:    envOrDefault("DRYDOCK_QDRANT_COLLECTION_NIP_SPECS", defaultCollections.NIPSpecs),
			ProjectDocs: envOrDefault("DRYDOCK_QDRANT_COLLECTION_PROJECT_DOCS", defaultCollections.ProjectDocs),
			FewShot:     envOrDefault("DRYDOCK_QDRANT_COLLECTION_FEW_SHOT", defaultCollections.FewShot),
			CodeChunks:  envOrDefault("DRYDOCK_QDRANT_COLLECTION_CODE_CHUNKS", defaultCollections.CodeChunks),
		},
		QdrantResultsPerCollection: parseIntOrDefault(envOrDefault("DRYDOCK_QDRANT_RESULTS_PER_COLLECTION", "3"), 3),
		EmbedBaseURL:               envOrDefault("DRYDOCK_EMBED_BASE_URL", ""),
		EmbedModel:                 envOrDefault("DRYDOCK_EMBED_MODEL", devDefault(production, defaultEmbedModel)),
		EmbedAPIKey:                envOrDefault("DRYDOCK_EMBED_API_KEY", ""),
		EmbedDimension:             parseIntOrDefault(envOrDefault("DRYDOCK_EMBED_DIMENSION", "768"), 768),
		PaymentNWCURI:              envOrDefault("DRYDOCK_NWC_CONNECTION_STRING", ""),
		PaymentTrustedMints:        paymentTrustedMints(),
		LSPBridgeURL:               envOrDefault("DRYDOCK_LSP_BRIDGE_URL", ""),
		MetaBaseURL:                envOrDefault("DRYDOCK_META_BASE_URL", devDefault(production, defaultMetaBaseURL)),
		MetaModel:                  envOrDefault("DRYDOCK_META_MODEL", devDefault(production, defaultMetaModel)),
		EvalDatasetPath:            envOrDefault("DRYDOCK_EVAL_DATASET_PATH", "eval/heldout-sample.json"),
		HealthAddr:                 envOrDefault("DRYDOCK_HEALTH_ADDR", "127.0.0.1:8081"),
		DashboardBearerToken:       envOrDefault("DRYDOCK_DASHBOARD_BEARER_TOKEN", ""),
		PipelineWorkers:            parseIntOrDefault(envOrDefault("DRYDOCK_PIPELINE_WORKERS", "2"), 2),
		CodeChatLimit:              parseIntOrDefault(envOrDefault("DRYDOCK_CODECHAT_RATE_LIMIT_REQUESTS", "20"), 20),
		CodeChatWindow:             parseDurationOrDefault(envOrDefault("DRYDOCK_CODECHAT_RATE_LIMIT_WINDOW", "1h"), time.Hour),
		FeedbackLimit:              parseIntOrDefault(envOrDefault("DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_REQUESTS", "100"), 100),
		FeedbackWindow:             parseDurationOrDefault(envOrDefault("DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_WINDOW", "24h"), 24*time.Hour),
	}
}

func devDefault(production bool, value string) string {
	if production {
		return ""
	}
	return value
}

func isProductionMode(environment, productionFlag string) bool {
	if parseBoolOrDefault(productionFlag, false) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "prod", "production":
		return true
	default:
		return false
	}
}

func configuredEnv() map[string]bool {
	keys := []string{
		"DRYDOCK_RELAYS",
		"DRYDOCK_READ_RELAYS",
		"DRYDOCK_WRITE_RELAYS",
		"DRYDOCK_LLM_API_KEY",
		"DRYDOCK_PLANNER_BASE_URL",
		"DRYDOCK_PLANNER_MODEL",
		"DRYDOCK_PLANNER_API_KEY",
		"DRYDOCK_CODER32B_BASE_URL",
		"DRYDOCK_CODER32B_MODEL",
		"DRYDOCK_CODER32B_API_KEY",
		"DRYDOCK_LLM70B_BASE_URL",
		"DRYDOCK_LLM70B_MODEL",
		"DRYDOCK_LLM70B_API_KEY",
		"DRYDOCK_CODER14B_BASE_URL",
		"DRYDOCK_CODER14B_MODEL",
		"DRYDOCK_CODER14B_API_KEY",
		"DRYDOCK_META_BASE_URL",
		"DRYDOCK_META_MODEL",
		"DRYDOCK_META_API_KEY",
		"DRYDOCK_QDRANT_URL",
		"DRYDOCK_QDRANT_API_KEY",
		"DRYDOCK_EMBED_BASE_URL",
		"DRYDOCK_EMBED_MODEL",
		"DRYDOCK_EMBED_API_KEY",
	}
	configured := make(map[string]bool, len(keys))
	for _, key := range keys {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			configured[key] = true
		}
	}
	return configured
}

func validCollectionName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
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

func paymentTrustedMints() []string {
	if mints := splitCSV(envOrDefault("DRYDOCK_CASHU_TRUSTED_MINTS", "")); len(mints) > 0 {
		return mints
	}
	return splitCSV(envOrDefault("DRYDOCK_CASHU_MINT_URL", ""))
}

func parseLogLevel(v string) slog.Level {
	normalized := strings.ToLower(strings.TrimSpace(v))
	switch normalized {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	default:
		slog.Warn("invalid log level, defaulting to info", "value", v)
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

func parseDurationOrDefault(v string, fallback time.Duration) time.Duration {
	parsed, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return parsed
}

func parseBoolOrDefault(v string, fallback bool) bool {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(trimmed)
	if err != nil {
		return fallback
	}
	return parsed
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

	if c.IsProduction() {
		c.validateProductionConfig(&result)
	}

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
	hasSignerConfig := c.SignerBunkerURL != "" || c.SignerNsec != "" || c.SignerNsecFile != ""
	if !hasSignerConfig {
		result.Warnings = append(result.Warnings, "no signer configured: review publishing will be disabled (set DRYDOCK_SIGNER_BUNKER_URL or DRYDOCK_SIGNER_NSEC)")
	}

	// --- Database connectivity ---
	if err := c.validateDatabase(ctx); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("database validation failed: %v", err))
	}

	// --- LLM endpoint checks (warnings only, as they may come online later) ---
	llmEndpoints := map[string]struct {
		baseURL string
		apiKey  string
	}{
		"planner":  {baseURL: c.PlannerBaseURL, apiKey: c.EffectiveLLMAPIKey(c.PlannerAPIKey)},
		"coder32b": {baseURL: c.Coder32BBaseURL, apiKey: c.EffectiveLLMAPIKey(c.Coder32BAPIKey)},
		"70b":      {baseURL: c.LLM70BBaseURL, apiKey: c.EffectiveLLMAPIKey(c.LLM70BAPIKey)},
		"coder14b": {baseURL: c.Coder14BBaseURL, apiKey: c.EffectiveLLMAPIKey(c.Coder14BAPIKey)},
		"meta":     {baseURL: c.MetaBaseURL, apiKey: c.EffectiveLLMAPIKey(c.MetaAPIKey)},
	}
	for name, endpoint := range llmEndpoints {
		baseURL := endpoint.baseURL
		if baseURL == "" {
			continue
		}
		if err := c.checkLLMEndpoint(ctx, baseURL, endpoint.apiKey); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("LLM endpoint %s (%s) not reachable: %v", name, baseURL, err))
		}
	}

	// --- Optional service warnings ---
	if c.ChartroomURL != "" && c.ChartroomToken == "" {
		result.Warnings = append(result.Warnings, "DRYDOCK_CHARTROOM_URL set but no bearer token configured (set DRYDOCK_CHARTROOM_TOKEN or CHARTROOM_HTTP_BEARER_TOKEN)")
	}
	if c.QdrantURL != "" && c.EmbedBaseURL == "" {
		result.Warnings = append(result.Warnings, "DRYDOCK_QDRANT_URL set but DRYDOCK_EMBED_BASE_URL not set: RAG features disabled")
	}
	if c.EmbedBaseURL != "" && c.QdrantURL == "" {
		result.Warnings = append(result.Warnings, "DRYDOCK_EMBED_BASE_URL set but DRYDOCK_QDRANT_URL not set: RAG features disabled")
	}
	if c.EmbedDimension <= 0 {
		result.Errors = append(result.Errors, "DRYDOCK_EMBED_DIMENSION must be at least 1")
	}
	for key, name := range map[string]string{
		"DRYDOCK_QDRANT_COLLECTION_NIP_SPECS":    c.QdrantCollections.NIPSpecs,
		"DRYDOCK_QDRANT_COLLECTION_PROJECT_DOCS": c.QdrantCollections.ProjectDocs,
		"DRYDOCK_QDRANT_COLLECTION_FEW_SHOT":     c.QdrantCollections.FewShot,
		"DRYDOCK_QDRANT_COLLECTION_CODE_CHUNKS":  c.QdrantCollections.CodeChunks,
	} {
		if !validCollectionName(name) {
			result.Errors = append(result.Errors, fmt.Sprintf("%s must contain only letters, numbers, underscore, or hyphen", key))
		}
	}
	if c.QdrantResultsPerCollection < 1 {
		result.Errors = append(result.Errors, "DRYDOCK_QDRANT_RESULTS_PER_COLLECTION must be at least 1")
	}
	if c.ListenerLookbackMin < 1 {
		result.Errors = append(result.Errors, "DRYDOCK_LISTENER_LOOKBACK_MIN must be at least 1")
	}
	if c.ListenerHWMOverlap <= 0 || c.ListenerMaxFutureSkew <= 0 || c.ListenerMaxEventAge <= 0 {
		result.Errors = append(result.Errors, "listener overlap, future skew, and max event age must be greater than 0")
	}
	if c.PaymentNWCURI != "" && len(c.PaymentTrustedMints) == 0 {
		result.Warnings = append(result.Warnings, "DRYDOCK_NWC_CONNECTION_STRING set but no trusted Cashu mints configured (set DRYDOCK_CASHU_TRUSTED_MINTS or DRYDOCK_CASHU_MINT_URL)")
	}
	if c.PaymentNWCURI == "" && len(c.PaymentTrustedMints) > 0 {
		result.Warnings = append(result.Warnings, "trusted Cashu mints configured but DRYDOCK_NWC_CONNECTION_STRING is empty: paid reviews cannot be authorized")
	}

	// --- Rate limits ---
	if c.CodeChatLimit < 1 {
		result.Errors = append(result.Errors, "DRYDOCK_CODECHAT_RATE_LIMIT_REQUESTS must be at least 1")
	}
	if c.CodeChatWindow <= 0 {
		result.Errors = append(result.Errors, "DRYDOCK_CODECHAT_RATE_LIMIT_WINDOW must be greater than 0")
	}
	if c.FeedbackLimit < 1 {
		result.Errors = append(result.Errors, "DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_REQUESTS must be at least 1")
	}
	if c.FeedbackWindow <= 0 {
		result.Errors = append(result.Errors, "DRYDOCK_MARKETPLACE_FEEDBACK_RATE_LIMIT_WINDOW must be greater than 0")
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

// IsProduction reports whether production-mode startup guards should be enforced.
func (c *Config) IsProduction() bool {
	if c.Production {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(c.Environment)) {
	case "prod", "production":
		return true
	default:
		return false
	}
}

func (c *Config) validateProductionConfig(result *ValidationResult) {
	if result == nil {
		return
	}

	if !c.hasExplicitProductionRelays() {
		result.Errors = append(result.Errors, "production mode requires DRYDOCK_RELAYS or both DRYDOCK_READ_RELAYS and DRYDOCK_WRITE_RELAYS to be explicitly configured")
	}
	for _, relay := range append(append([]string{}, c.Relays...), append(c.ReadRelays, c.WriteRelays...)...) {
		if isDefaultPublicRelay(relay) {
			result.Errors = append(result.Errors, fmt.Sprintf("production mode must not use built-in public relay default %q; configure private/approved relays explicitly", relay))
		}
	}

	c.requireExplicit(result, "DRYDOCK_PLANNER_BASE_URL", c.PlannerBaseURL)
	c.requireExplicit(result, "DRYDOCK_PLANNER_MODEL", c.PlannerModel)
	c.requireLLMAPIKey(result, "planner", "DRYDOCK_PLANNER_API_KEY", c.EffectiveLLMAPIKey(c.PlannerAPIKey))
	c.requireExplicit(result, "DRYDOCK_CODER32B_BASE_URL", c.Coder32BBaseURL)
	c.requireExplicit(result, "DRYDOCK_CODER32B_MODEL", c.Coder32BModel)
	c.requireLLMAPIKey(result, "coder32b", "DRYDOCK_CODER32B_API_KEY", c.EffectiveLLMAPIKey(c.Coder32BAPIKey))
	c.requireExplicit(result, "DRYDOCK_LLM70B_BASE_URL", c.LLM70BBaseURL)
	c.requireExplicit(result, "DRYDOCK_LLM70B_MODEL", c.LLM70BModel)
	c.requireLLMAPIKey(result, "70b", "DRYDOCK_LLM70B_API_KEY", c.EffectiveLLMAPIKey(c.LLM70BAPIKey))
	c.requireExplicit(result, "DRYDOCK_CODER14B_BASE_URL", c.Coder14BBaseURL)
	c.requireExplicit(result, "DRYDOCK_CODER14B_MODEL", c.Coder14BModel)
	c.requireLLMAPIKey(result, "coder14b", "DRYDOCK_CODER14B_API_KEY", c.EffectiveLLMAPIKey(c.Coder14BAPIKey))
	c.requireExplicit(result, "DRYDOCK_META_BASE_URL", c.MetaBaseURL)
	c.requireExplicit(result, "DRYDOCK_META_MODEL", c.MetaModel)
	c.requireLLMAPIKey(result, "meta", "DRYDOCK_META_API_KEY", c.EffectiveLLMAPIKey(c.MetaAPIKey))

	for name, rawURL := range map[string]string{
		"DRYDOCK_PLANNER_BASE_URL":  c.PlannerBaseURL,
		"DRYDOCK_CODER32B_BASE_URL": c.Coder32BBaseURL,
		"DRYDOCK_LLM70B_BASE_URL":   c.LLM70BBaseURL,
		"DRYDOCK_CODER14B_BASE_URL": c.Coder14BBaseURL,
		"DRYDOCK_META_BASE_URL":     c.MetaBaseURL,
		"DRYDOCK_QDRANT_URL":        c.QdrantURL,
		"DRYDOCK_EMBED_BASE_URL":    c.EmbedBaseURL,
	} {
		if isLoopbackURL(rawURL) {
			result.Errors = append(result.Errors, fmt.Sprintf("production mode must not use localhost/loopback URL for %s", name))
		}
		if strings.TrimSpace(rawURL) != "" && !isHTTPSURL(rawURL) {
			result.Errors = append(result.Errors, fmt.Sprintf("production mode requires %s to use https://", name))
		}
	}

	c.requireExplicit(result, "DRYDOCK_QDRANT_URL", c.QdrantURL)
	c.requireExplicit(result, "DRYDOCK_QDRANT_API_KEY", c.QdrantAPIKey)
	c.requireExplicit(result, "DRYDOCK_EMBED_BASE_URL", c.EmbedBaseURL)
	c.requireExplicit(result, "DRYDOCK_EMBED_MODEL", c.EmbedModel)
	c.requireExplicit(result, "DRYDOCK_EMBED_API_KEY", c.EmbedAPIKey)
}

func (c *Config) hasExplicitProductionRelays() bool {
	hasSharedRelays := len(c.Relays) > 0 && c.isExplicit("DRYDOCK_RELAYS")
	hasSplitRelays := len(c.ReadRelays) > 0 && c.isExplicit("DRYDOCK_READ_RELAYS") && len(c.WriteRelays) > 0 && c.isExplicit("DRYDOCK_WRITE_RELAYS")
	return hasSharedRelays || hasSplitRelays
}

func (c *Config) requireExplicit(result *ValidationResult, key, value string) {
	if strings.TrimSpace(value) == "" || !c.isExplicit(key) {
		result.Errors = append(result.Errors, fmt.Sprintf("production mode requires %s to be explicitly configured", key))
	}
}

func (c *Config) requireLLMAPIKey(result *ValidationResult, endpointName, endpointKeyEnv, value string) {
	if strings.TrimSpace(value) == "" || (!c.isExplicit("DRYDOCK_LLM_API_KEY") && !c.isExplicit(endpointKeyEnv)) {
		result.Errors = append(result.Errors, fmt.Sprintf("production mode requires an API key for %s LLM endpoint via DRYDOCK_LLM_API_KEY or %s", endpointName, endpointKeyEnv))
	}
}

func (c *Config) isExplicit(key string) bool {
	if c.ExplicitEnv == nil {
		return true
	}
	return c.ExplicitEnv[key]
}

func isDefaultPublicRelay(relay string) bool {
	_, ok := defaultPublicRelaySet[normalizeURLForCompare(relay)]
	return ok
}

func normalizeURLForCompare(raw string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isHTTPSURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && strings.EqualFold(u.Scheme, "https") && u.Hostname() != ""
}

// validateDatabase checks that the database can be opened and is writable.
func (c *Config) validateDatabase(ctx context.Context) error {
	// Parse the DSN to ensure it's valid
	if c.DatabaseURL == "" {
		return fmt.Errorf("database URL is empty")
	}

	// Try to open and ping
	db, err := sql.Open("sqlite", c.DatabaseURL)
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

// EffectiveLLMAPIKey returns an endpoint-specific API key when configured,
// falling back to the global DRYDOCK_LLM_API_KEY.
func (c *Config) EffectiveLLMAPIKey(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return c.LLMAPIKey
}

// checkLLMEndpoint verifies an LLM endpoint is reachable.
func (c *Config) checkLLMEndpoint(ctx context.Context, baseURL, apiKey string) error {
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

		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
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
