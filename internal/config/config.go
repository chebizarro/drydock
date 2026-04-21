package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
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
