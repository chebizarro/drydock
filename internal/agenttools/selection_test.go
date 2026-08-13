package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"drydock/internal/contextbuilder"
	"drydock/internal/workspacesnapshot"
)

type byteCounter struct{}

func (byteCounter) Count(text string) int { return len(text) }

func TestSelectionCoalescesRangesAndProtectsMandatoryArtifacts(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{
		"changed.go": "package p\nfunc Changed() {}\n",
		"extra.go":   "one\ntwo\nthree\nfour\nfive\nsix\nseven\n",
	})
	selection, err := NewSelection(SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: byteCounter{}, TokenBudget: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.Add(context.Background(),
		SelectionArtifact{Kind: ArtifactLineRange, Path: "extra.go", StartLine: 2, EndLine: 4},
		SelectionArtifact{Kind: ArtifactLineRange, Path: "extra.go", StartLine: 4, EndLine: 6},
	); err != nil {
		t.Fatal(err)
	}
	status := selection.Status()
	var ranges []SelectionArtifact
	for _, artifact := range status.Artifacts {
		if artifact.Kind == ArtifactLineRange {
			ranges = append(ranges, artifact)
		}
	}
	if len(ranges) != 1 || ranges[0].StartLine != 2 || ranges[0].EndLine != 6 {
		t.Fatalf("coalesced ranges = %#v", ranges)
	}
	if err := selection.Remove(SelectionArtifact{Kind: ArtifactPatch}); !errors.Is(err, ErrMandatoryArtifact) {
		t.Fatalf("remove patch error = %v", err)
	}
	if err := selection.Remove(SelectionArtifact{Kind: ArtifactFile, Path: "changed.go"}); !errors.Is(err, ErrMandatoryArtifact) {
		t.Fatalf("remove changed file error = %v", err)
	}
	if err := selection.Remove(SelectionArtifact{Kind: ArtifactLineRange, Path: "extra.go", StartLine: 3, EndLine: 4}); err != nil {
		t.Fatal(err)
	}
	status = selection.Status()
	var got []LineRange
	for _, artifact := range status.Artifacts {
		if artifact.Kind == ArtifactLineRange {
			got = append(got, LineRange{StartLine: artifact.StartLine, EndLine: artifact.EndLine})
		}
	}
	want := []LineRange{{StartLine: 2, EndLine: 2}, {StartLine: 5, EndLine: 6}}
	if !slices.Equal(got, want) {
		t.Fatalf("subtracted ranges = %#v, want %#v", got, want)
	}
}

func TestSelectionKeepsDeletedChangedFileAsMandatoryMetadata(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"remaining.go": "package p"})
	selection, err := NewSelection(SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"deleted.go"},
		Counter: byteCounter{}, TokenBudget: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := selection.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bundle.Content, "## file: deleted.go\n[deleted in snapshot]") {
		t.Fatalf("deleted file metadata missing:\n%s", bundle.Content)
	}
	status := selection.Status()
	found := false
	for _, artifact := range status.Artifacts {
		if artifact.Kind == ArtifactFile && artifact.Path == "deleted.go" && artifact.Mandatory {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleted mandatory artifact missing: %#v", status.Artifacts)
	}
}

func TestSelectionFinalizeRendersExactImmutableBundle(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{
		"changed.go": "package p\nfunc Changed() {}\n",
		"extra.go":   "package p\ntype Extra struct{}\nfunc Build() Extra { return Extra{} }\n",
	})
	selection, err := NewSelection(SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: byteCounter{}, TokenBudget: 20_000, Headroom: 0.10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.Add(context.Background(),
		SelectionArtifact{Kind: ArtifactCodemap, Path: "extra.go"},
	); err != nil {
		t.Fatal(err)
	}
	bundle, err := selection.Finalize(context.Background())
	if err != nil {
		if strings.Contains(err.Error(), "tree-sitter is unavailable") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if bundle.TokenCount != len(bundle.Content) {
		t.Fatalf("token count = %d, serialized bytes = %d", bundle.TokenCount, len(bundle.Content))
	}
	if !slices.Equal(bundle.ChangedFiles, []string{"changed.go"}) {
		t.Fatalf("changed files = %v", bundle.ChangedFiles)
	}
	for _, marker := range []string{"## patch", "## changed-files", "## file: changed.go", "## codemap: extra.go"} {
		if !strings.Contains(bundle.Content, marker) {
			t.Fatalf("bundle missing %q:\n%s", marker, bundle.Content)
		}
	}
	if err := selection.Add(context.Background(), SelectionArtifact{Kind: ArtifactFile, Path: "extra.go"}); !errors.Is(err, ErrSelectionFinalized) {
		t.Fatalf("add after finalize error = %v", err)
	}
	again, err := selection.Finalize(context.Background())
	if err != nil || again.Content != bundle.Content {
		t.Fatalf("repeat finalize = %#v, %v", again, err)
	}
	stored, ok := selection.Bundle()
	if !ok || stored.Content != bundle.Content {
		t.Fatalf("stored bundle = %#v, ok=%v", stored, ok)
	}
}

func TestSelectionRejectsApproximateCounter(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"changed.go": "content"})
	_, err := NewSelection(SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: contextbuilder.ApproxTokenCounter{}, TokenBudget: 1_000,
	})
	if !errors.Is(err, ErrAuthoritativeTokenCounterRequired) {
		t.Fatalf("approximate counter error = %v", err)
	}
}

func TestExactGateAppliesHeadroom(t *testing.T) {
	bundle := contextbuilder.ContextBundle{Content: strings.Repeat("x", 90)}
	got, err := GateBundle(bundle, byteCounter{}, 100, 0.10)
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenCount != 90 || got.TokenBudget != 100 {
		t.Fatalf("gated bundle = %#v", got)
	}
	bundle.Content += "x"
	if _, err := GateBundle(bundle, byteCounter{}, 100, 0.10); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over-budget error = %v", err)
	}
}

func TestFinalizationDetectsSnapshotTampering(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"changed.go": "before"})
	selection, err := NewSelection(SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: byteCounter{}, TokenBudget: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := snapshot.Resolve("changed.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolved, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := selection.Finalize(context.Background()); !errors.Is(err, workspacesnapshot.ErrHashMismatch) {
		t.Fatalf("tampered finalize error = %v", err)
	}
}

func TestSelectionFinalizeToolIsRegistryGate(t *testing.T) {
	snapshot := mutableSnapshot(t, map[string]string{"changed.go": "content"})
	selection, err := NewSelection(SelectionConfig{
		Snapshot: snapshot, ChangedFiles: []string{"changed.go"},
		Counter: byteCounter{}, TokenBudget: 1_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	scope := NewScope("selection-run", snapshot, RoleContextDiscovery)
	scope.Selection = selection
	result, err := registry.Dispatch(context.Background(), Invocation{
		ToolCallID: "finalize-1", Name: ToolSelectionFinalize,
		Arguments: json.RawMessage(`{}`), Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"context_hash"`) {
		t.Fatalf("finalize result = %s", result.Content)
	}
	if _, ok := selection.Bundle(); !ok {
		t.Fatal("selection.finalize did not freeze a bundle")
	}
}
