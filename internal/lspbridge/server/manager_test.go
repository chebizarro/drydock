package server

import (
	"reflect"
	"testing"

	"drydock/internal/lspbridge"
)

func TestManagerCommandConfigOverride(t *testing.T) {
	mgr := NewManager(nil, WithLSPCommandConfig(lspbridge.LangGo, lspbridge.LSPCommandConfig{
		Command: "/opt/drydock/bin/gopls",
		Args:    []string{"serve", "-remote=auto"},
	}))
	defer mgr.Shutdown()

	cfg, err := mgr.commandConfig(lspbridge.LangGo)
	if err != nil {
		t.Fatalf("commandConfig: %v", err)
	}
	if cfg.Command != "/opt/drydock/bin/gopls" {
		t.Fatalf("expected override command, got %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Args, []string{"serve", "-remote=auto"}) {
		t.Fatalf("expected override args, got %+v", cfg.Args)
	}
}

func TestManagerCommandConfigDisable(t *testing.T) {
	mgr := NewManager(nil, WithLSPCommandConfig(lspbridge.LangRust, lspbridge.LSPCommandConfig{Disabled: true}))
	defer mgr.Shutdown()

	if _, err := mgr.commandConfig(lspbridge.LangRust); err == nil {
		t.Fatal("expected disabled language to return an error")
	}
}

func TestConfiguredLSPCommandConfigsFromEnv(t *testing.T) {
	t.Setenv("DRYDOCK_LSP_GO_COMMAND", "/custom/gopls")
	t.Setenv("DRYDOCK_LSP_GO_ARGS", "serve,-remote=auto")
	t.Setenv("DRYDOCK_LSP_RUST_DISABLED", "true")

	configs := configuredLSPCommandConfigs()
	goCfg := configs[lspbridge.LangGo]
	if goCfg.Command != "/custom/gopls" || !reflect.DeepEqual(goCfg.Args, []string{"serve", "-remote=auto"}) {
		t.Fatalf("unexpected Go config from env: %+v", goCfg)
	}
	if !configs[lspbridge.LangRust].Disabled {
		t.Fatalf("expected Rust to be disabled from env, got %+v", configs[lspbridge.LangRust])
	}
}
