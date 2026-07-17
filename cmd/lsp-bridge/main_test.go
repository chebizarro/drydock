package main

import "testing"

func TestParseConfigRefusesUnauthenticatedStartup(t *testing.T) {
	getenv := func(string) string { return "" }
	if _, err := parseConfig(nil, getenv); err == nil {
		t.Fatal("expected startup configuration without tokens or dev mode to fail")
	}
}

func TestParseConfigSecurityDefaultsAndExplicitDev(t *testing.T) {
	getenv := func(string) string { return "" }
	cfg, err := parseConfig([]string{"-dev"}, getenv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.addr != "127.0.0.1:8082" {
		t.Fatalf("expected loopback default, got %q", cfg.addr)
	}
	if !cfg.dev {
		t.Fatal("expected explicit dev mode")
	}
}

func TestParseConfigAcceptsAuthTokenEnv(t *testing.T) {
	getenv := func(key string) string {
		if key == "LSP_BRIDGE_AUTH_TOKEN" {
			return "secret"
		}
		return ""
	}
	cfg, err := parseConfig(nil, getenv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.authTokens) != 1 || cfg.authTokens[0] != "secret" {
		t.Fatalf("unexpected auth tokens: %#v", cfg.authTokens)
	}
}
