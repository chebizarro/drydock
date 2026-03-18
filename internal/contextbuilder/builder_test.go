package contextbuilder

import (
	"context"
	"testing"
)

type fakeProvider struct {
	name     string
	priority int
	content  string
}

func (f fakeProvider) LayerName() string { return f.name }
func (f fakeProvider) Priority() int     { return f.priority }
func (f fakeProvider) Build(context.Context, BuildInput) (string, error) {
	return f.content, nil
}

type byteCounter struct{}

func (byteCounter) Count(text string) int { return len(text) }

func TestBuilderHardStopsAndDropsLowerPriorityLayers(t *testing.T) {
	b := &Builder{
		TokenBudget: 10,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: LayerPatchDiff, priority: 1, content: "12345"},
			fakeProvider{name: LayerFileContext, priority: 2, content: "123456"},
			fakeProvider{name: LayerTests, priority: 4, content: "xx"},
		},
	}

	out, err := b.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if out.TokenCount != 5 {
		t.Fatalf("expected token count 5, got %d", out.TokenCount)
	}
	if len(out.LayersUsed) != 1 || out.LayersUsed[0] != LayerPatchDiff {
		t.Fatalf("unexpected layers used: %#v", out.LayersUsed)
	}
	if len(out.LayersDropped) != 2 || out.LayersDropped[0] != LayerFileContext || out.LayersDropped[1] != LayerTests {
		t.Fatalf("unexpected dropped order: %#v", out.LayersDropped)
	}
}

func TestBuilderDeterministicOrderByPriorityThenName(t *testing.T) {
	b := &Builder{
		TokenBudget: 100,
		Counter:     byteCounter{},
		Providers: []Provider{
			fakeProvider{name: "zeta", priority: 3, content: "1"},
			fakeProvider{name: "alpha", priority: 3, content: "1"},
			fakeProvider{name: "beta", priority: 1, content: "1"},
		},
	}
	out, err := b.Build(context.Background(), BuildInput{})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	want := []string{"beta", "alpha", "zeta"}
	if len(out.LayersUsed) != len(want) {
		t.Fatalf("expected %d used layers, got %d", len(want), len(out.LayersUsed))
	}
	for i := range want {
		if out.LayersUsed[i] != want[i] {
			t.Fatalf("unexpected order at %d: want %s got %s", i, want[i], out.LayersUsed[i])
		}
	}
}

