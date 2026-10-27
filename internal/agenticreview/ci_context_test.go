package agenticreview

import (
	"context"
	"strings"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/agenttools"
	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

func TestSuccessfulDiscoveryIncludesCIClaimsWithinExistingGate(t *testing.T) {
	patch := testPatch()
	snapshot := discoverySnapshot(t, patch, map[string]string{"changed.go": "package p\nconst Value = 2\n"})
	client := &scriptedClient{results: []reviewengine.CompletionResult{serviceCompletion(serviceToolCall(t, "finalize", agenttools.ToolSelectionFinalize, map[string]any{}))}}
	discovery, err := NewDiscovery(DiscoveryConfig{Client: client, Registry: agenttools.NewRegistry(), Counter: testCounter{}, TokenBudget: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	claims := "CI attestations — third-party claims, not authorization.\n- compute job: success; worker=fixture; event_id=fixture\n"
	result, err := discovery.Run(context.Background(), DiscoveryInput{Snapshot: snapshot, Patch: patch, BuildInput: contextbuilder.BuildInput{CIAttestations: claims}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SessionCapable || !strings.Contains(result.Bundle.Content, claims) || !strings.Contains(result.Bundle.Content, patch) {
		t.Fatalf("successful discovery lost claims or patch: %s", result.Bundle.Content)
	}
	if result.Bundle.TokenCount != len(result.Bundle.Content) {
		t.Fatal("new content bypassed authoritative token recount")
	}
	found := false
	for _, layer := range result.Bundle.LayersUsed {
		if layer == contextbuilder.LayerCIAttestations {
			found = true
		}
	}
	if !found {
		t.Fatal("CI layer not recorded")
	}
}
func TestOptionalCIClaimsCannotBlockReviewOnBudget(t *testing.T) {
	base := contextbuilder.ContextBundle{Content: "authoritative patch bytes", TokenCount: 25, TokenBudget: 100}
	got, err := withCIClaims(base, strings.Repeat("claim", 100), testCounter{}, 100, .2)
	if err != nil || got.Content != base.Content || got.TokenCount != base.TokenCount {
		t.Fatalf("optional claim blocked/changed review: %#v %v", got, err)
	}
	if len(got.LayersDropped) != 1 || got.LayersDropped[0] != contextbuilder.LayerCIAttestations {
		t.Fatal("budget drop not surfaced")
	}
	disabled, err := withCIClaims(base, "", testCounter{}, 100, .2)
	if err != nil || disabled.Content != base.Content || len(disabled.LayersUsed) != 0 || len(disabled.LayersDropped) != 0 {
		t.Fatal("disabled integration altered bundle")
	}
}
