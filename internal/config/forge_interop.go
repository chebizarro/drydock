package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type ForgeInteropConfig struct {
	LoomEnabled        bool
	LoomMaxRunsPerRepo int
	LoomMaxRuns        int
	LoomRunTTL         time.Duration
	ConcordEnabled     bool
	parseErrors        []string
}

func forgeInteropFromEnv() ForgeInteropConfig {
	c := ForgeInteropConfig{LoomMaxRunsPerRepo: 64, LoomMaxRuns: 1024, LoomRunTTL: 24 * time.Hour}
	for _, item := range []struct {
		name string
		dest *bool
	}{{"DRYDOCK_LOOM_ENABLED", &c.LoomEnabled}, {"DRYDOCK_CONCORD_ENABLED", &c.ConcordEnabled}} {
		if raw := strings.TrimSpace(os.Getenv(item.name)); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				c.parseErrors = append(c.parseErrors, item.name+" must be a boolean")
			} else {
				*item.dest = value
			}
		}
	}
	for _, item := range []struct {
		name string
		dest *int
	}{{"DRYDOCK_LOOM_MAX_RUNS_PER_REPO", &c.LoomMaxRunsPerRepo}, {"DRYDOCK_LOOM_MAX_RUNS", &c.LoomMaxRuns}} {
		if raw := strings.TrimSpace(os.Getenv(item.name)); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				c.parseErrors = append(c.parseErrors, item.name+" must be an integer")
			} else {
				*item.dest = value
			}
		}
	}
	if raw := strings.TrimSpace(os.Getenv("DRYDOCK_LOOM_RUN_TTL")); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil {
			c.parseErrors = append(c.parseErrors, "DRYDOCK_LOOM_RUN_TTL must be a duration")
		} else {
			c.LoomRunTTL = value
		}
	}
	return c
}
func (c *Config) validateForgeInterop(v *ValidationResult) {
	v.Errors = append(v.Errors, c.parseErrors...)
	if c.LoomEnabled {
		if c.LoomMaxRunsPerRepo < 1 || c.LoomMaxRunsPerRepo > 1024 {
			v.Errors = append(v.Errors, "DRYDOCK_LOOM_MAX_RUNS_PER_REPO must be in [1,1024]")
		}
		if c.LoomMaxRuns < 1 || c.LoomMaxRuns > 16384 || c.LoomMaxRuns < c.LoomMaxRunsPerRepo {
			v.Errors = append(v.Errors, "DRYDOCK_LOOM_MAX_RUNS must be in [per-repository cap,16384]")
		}
		if c.LoomRunTTL < time.Minute || c.LoomRunTTL > 7*24*time.Hour {
			v.Errors = append(v.Errors, "DRYDOCK_LOOM_RUN_TTL must be in [1m,168h]")
		}
	}
	if (c.LoomEnabled || c.ConcordEnabled) && c.SignerBunkerURL == "" && c.SignerNsec == "" {
		v.Errors = append(v.Errors, "forge interop requires a signer (Loom needs monitored-repository identity; Concord needs an author)")
	}
}
