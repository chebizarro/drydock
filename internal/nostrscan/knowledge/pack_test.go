package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackCoverageAndSources(t *testing.T) {
	pack, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if pack.Version != "1.0.0" {
		t.Fatalf("version = %q", pack.Version)
	}
	if len(pack.AdversaryModels) != 2 || pack.AdversaryModels[0].ID != "MU" || pack.AdversaryModels[1].ID != "MS" {
		t.Fatalf("unexpected adversary models: %#v", pack.AdversaryModels)
	}
	wantV := []string{"NOSTR-V1", "NOSTR-V2", "NOSTR-V3", "NOSTR-V4", "NOSTR-V5", "NOSTR-V6", "NOSTR-V7", "NOSTR-R1", "NOSTR-R2"}
	if len(pack.Vulnerabilities) != len(wantV) {
		t.Fatalf("vulnerability count = %d", len(pack.Vulnerabilities))
	}
	for i, want := range wantV {
		entry := pack.Vulnerabilities[i]
		if entry.ID != want {
			t.Errorf("vulnerability[%d] = %q, want %q", i, entry.ID, want)
		}
		if !strings.Contains(entry.Source, "[NP25]") {
			t.Errorf("%s source lacks NP25 citation: %q", entry.ID, entry.Source)
		}
	}
	for _, want := range []string{"NIP-01", "NIP-04", "NIP-44", "NIP-46", "NIP-59", "event-kinds"} {
		found := false
		for _, entry := range pack.NIPCheatSheet {
			found = found || entry.ID == want
		}
		if !found {
			t.Errorf("missing cheat-sheet entry %s", want)
		}
	}
}

func TestContextGolden(t *testing.T) {
	got, err := Context()
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "nostr-protocol.golden.md", got+"\n")
}

func TestReviewerSystemPreambleGolden(t *testing.T) {
	got, err := ReviewerSystemPreamble()
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "reviewer-preamble.golden.txt", got+"\n")
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch for %s; run UPDATE_GOLDEN=1 go test ./internal/nostrscan/knowledge", name)
	}
}
