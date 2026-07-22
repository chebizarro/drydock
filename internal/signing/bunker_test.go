package signing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateClientKeyPersistsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signet", "client.key")
	first, err := loadOrCreateClientKey(path)
	if err != nil {
		t.Fatalf("create client key: %v", err)
	}
	second, err := loadOrCreateClientKey(path)
	if err != nil {
		t.Fatalf("reload client key: %v", err)
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
