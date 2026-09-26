package main

import "testing"

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseConfigRequiresToken(t *testing.T) {
	_, err := parseConfig(nil, envFrom(nil))
	if err == nil {
		t.Fatal("expected error when no token and not dev mode")
	}
}

func TestParseConfigDevModeAllowsNoToken(t *testing.T) {
	cfg, err := parseConfig([]string{"-dev"}, envFrom(nil))
	if err != nil {
		t.Fatalf("dev mode error = %v", err)
	}
	if !cfg.dev || len(cfg.authTokens) != 0 {
		t.Fatalf("unexpected cfg %#v", cfg)
	}
}

func TestParseConfigTokenFromEnv(t *testing.T) {
	cfg, err := parseConfig(nil, envFrom(map[string]string{
		"DEP_RUNNER_AUTH_TOKEN":    "secret",
		"DEP_RUNNER_GOPROXY":       "https://proxy.example",
		"DEP_RUNNER_ALLOW_SCRIPTS": "true",
	}))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(cfg.authTokens) != 1 || cfg.authTokens[0] != "secret" {
		t.Fatalf("tokens = %v", cfg.authTokens)
	}
	if cfg.opts.GoProxy != "https://proxy.example" {
		t.Fatalf("goproxy = %q", cfg.opts.GoProxy)
	}
	if !cfg.opts.AllowScripts {
		t.Fatal("allow scripts should be true")
	}
}

func TestParseConfigFlagOverridesAllowScripts(t *testing.T) {
	cfg, err := parseConfig([]string{"-auth-token", "t", "-allow-scripts"}, envFrom(nil))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !cfg.opts.AllowScripts {
		t.Fatal("flag should enable allow scripts")
	}
}

func TestParseConfigBadTimeout(t *testing.T) {
	_, err := parseConfig(nil, envFrom(map[string]string{
		"DEP_RUNNER_AUTH_TOKEN": "t",
		"DEP_RUNNER_TIMEOUT":    "not-a-duration",
	}))
	if err == nil {
		t.Fatal("expected timeout parse error")
	}
}
