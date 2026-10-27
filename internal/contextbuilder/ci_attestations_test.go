package contextbuilder

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/db"
)

func TestCIAttestationsContextMatchesExactTarget(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "context.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	commit, patch, worker := strings.Repeat("b", 40), strings.Repeat("c", 64), strings.Repeat("d", 64)
	for _, a := range []db.CIAttestation{
		{EventID: strings.Repeat("1", 64), RepoID: "repo", CommitRef: commit, Worker: worker, Signer: worker, ClaimType: "compute job", Status: "success", CreatedAt: 1700000000},
		{EventID: strings.Repeat("2", 64), RepoID: "repo", PatchRef: patch, Worker: worker, Signer: worker, ClaimType: "workflow", Status: "failed", CreatedAt: 1700000001},
		{EventID: strings.Repeat("3", 64), RepoID: "other", CommitRef: commit, Status: "wrong-repo"},
		{EventID: strings.Repeat("4", 64), RepoID: "repo", CommitRef: strings.Repeat("a", 40), Status: "wrong-commit"},
	} {
		if err := store.CompleteLoomRun(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	p := CIAttestationsProvider{Store: store}
	input := BuildInput{RepoID: "repo", CommitRef: commit, PatchEventID: patch, PatchEventContent: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"}
	builder, err := NewBuilder(DefaultTokenBudget, ApproxTokenCounter{}, []Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := builder.Build(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CI attestations", "third-party claims", "not authorization", "compute job: success", "workflow: failed", worker, "2023-11-14T22:13:20Z", "event_id=" + strings.Repeat("1", 64), "event_id=" + strings.Repeat("2", 64)} {
		if !strings.Contains(bundle.Content, want) {
			t.Fatalf("missing %q in %s", want, bundle.Content)
		}
	}
	for _, bad := range []string{"wrong-repo", "wrong-commit"} {
		if strings.Contains(bundle.Content, bad) {
			t.Fatal("unrelated claim leaked")
		}
	}
	snapshot := "CI attestations — pre-rendered third-party claims"
	rendered, err := (CIAttestationsProvider{}).Build(ctx, BuildInput{CIAttestations: snapshot})
	if err != nil || rendered != snapshot {
		t.Fatal("pre-rendered claim snapshot was not reused")
	}
	emptySnapshot, err := (CIAttestationsProvider{}).Build(ctx, BuildInput{RepoID: "repo", CommitRef: commit, CIAttestationsLoaded: true})
	if err != nil || emptySnapshot != "" {
		t.Fatal("empty preloaded snapshot was not preserved")
	}
	missing, err := p.Build(ctx, BuildInput{RepoID: "repo", CommitRef: "missing"})
	if err != nil || missing != "" {
		t.Fatalf("unmatched context: %q %v", missing, err)
	}
	// Merely persisting claims cannot enable the layer: wiring is operator-gated.
	disabled, _ := NewBuilder(DefaultTokenBudget, ApproxTokenCounter{}, nil)
	bundle, err = disabled.Build(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bundle.Content, "CI attestations") {
		t.Fatal("disabled provider surfaced claims")
	}
}
