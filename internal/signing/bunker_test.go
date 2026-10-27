package signing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateClientKeyPersistsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signet", "client.key")
	first, resumed, err := LoadOrCreateClientKey(path)
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}
	if resumed {
		t.Fatal("new client key reported as resumed")
	}
	second, resumed, err := LoadOrCreateClientKey(path)
	if err != nil {
		t.Fatalf("reload client key: %v", err)
	}
	if !resumed {
		t.Fatal("persisted client key was not reported as resumed")
	}
	if first.Hex() != second.Hex() {
		t.Fatal("client identity changed after reload")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat client key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("client key mode = %o, want 600", got)
	}
}

func TestLoadOrCreateClientKeyEmptyPath(t *testing.T) {
	// An empty path must generate a fresh key every call.
	first, resumed, err := LoadOrCreateClientKey("")
	if err != nil {
		t.Fatalf("first call with empty path: %v", err)
	}
	if resumed {
		t.Fatal("empty path reported as resumed")
	}
	second, resumed, err := LoadOrCreateClientKey("")
	if err != nil {
		t.Fatalf("second call with empty path: %v", err)
	}
	if resumed {
		t.Fatal("second empty path reported as resumed")
	}
	if first.Hex() == second.Hex() {
		t.Fatal("two empty-path calls returned the same key")
	}
}

func TestStripBunkerConnectSecret(t *testing.T) {
	got, err := StripBunkerConnectSecret("bunker://abc?relay=wss%3A%2F%2Frelay.example&secret=one-time")
	if err != nil {
		t.Fatalf("strip connect secret: %v", err)
	}
	if got != "bunker://abc?relay=wss%3A%2F%2Frelay.example" {
		t.Fatalf("stripped URL = %q", got)
	}
}
