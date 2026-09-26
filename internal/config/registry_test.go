package config

import (
	"strings"
	"testing"
)

func TestProductionRegistryEndpointsRequireTLS(t *testing.T) {
	for _, tc := range []struct {
		key string
		set func(*Config)
	}{
		{"DRYDOCK_GO_REGISTRY_URL", func(c *Config) { c.GoRegistryURL = "http://mirror.example" }},
		{"DRYDOCK_NPM_REGISTRY_URL", func(c *Config) { c.NPMRegistryURL = "http://mirror.example" }},
		{"DRYDOCK_CARGO_REGISTRY_URL", func(c *Config) { c.CargoRegistryURL = "http://mirror.example" }},
		{"DRYDOCK_PYPI_REGISTRY_URL", func(c *Config) { c.PyPIRegistryURL = "http://mirror.example" }},
	} {
		t.Run(tc.key, func(t *testing.T) {
			cfg := Config{}
			tc.set(&cfg)
			result := ValidationResult{}
			cfg.validateProductionConfig(&result)
			found := false
			for _, msg := range result.Errors {
				if strings.Contains(msg, tc.key) && strings.Contains(msg, "https://") {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing TLS rejection for %s: %v", tc.key, result.Errors)
			}
		})
	}
}
