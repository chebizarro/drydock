package promptrefine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"drydock/internal/db"
	"drydock/internal/reviewengine"
)

// fakeLLM records calls and returns canned responses.
type fakeLLM struct {
	responses []string
	calls     int
}

type failingPromptStore struct {
	promptStore
	activeErr error
	parentErr error
}

func (s *failingPromptStore) GetActivePromptVersion(ctx context.Context, name string) (db.PromptVersion, error) {
	if s.activeErr != nil {
		return db.PromptVersion{}, s.activeErr
	}
	return s.promptStore.GetActivePromptVersion(ctx, name)
}

func (s *failingPromptStore) GetPromptVersionByNumber(ctx context.Context, name string, version int) (db.PromptVersion, error) {
	if s.parentErr != nil {
		return db.PromptVersion{}, s.parentErr
	}
	return s.promptStore.GetPromptVersionByNumber(ctx, name, version)
}

func (f *fakeLLM) ChatCompletion(_ context.Context, _ reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	if f.calls >= len(f.responses) {
		return reviewengine.ChatResult{}, fmt.Errorf("no more responses")
	}
	resp := f.responses[f.calls]
	f.calls++
	return reviewengine.ChatResult{Content: resp}, nil
}

func setupStore(t *testing.T) *db.Store {
	t.Helper()
	ctx := context.Background()
	store, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestCheckAndRefineNotTriggeredBelowThreshold(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	llm := &fakeLLM{responses: []string{"refined prompt"}}

	svc := New(Config{Threshold: 5}, store, llm, logger)

	// Insert only 3 gaps — below threshold of 5.
	for i := 0; i < 3; i++ {
		if err := store.InsertPromptGap(ctx, "patch1", "repo1", fmt.Sprintf("gap %d", i)); err != nil {
			t.Fatalf("insert gap: %v", err)
		}
	}

	result, err := svc.CheckAndRefine(ctx)
	if err != nil {
		t.Fatalf("check and refine: %v", err)
	}
	if result.Triggered {
		t.Error("expected not triggered below threshold")
	}
	if llm.calls > 0 {
		t.Error("LLM should not have been called")
	}
}

func TestCheckAndRefineTriggersAtThreshold(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	refinedPrompt := "You are an improved code review agent.\nReturn JSON ONLY."
	llm := &fakeLLM{responses: []string{refinedPrompt}}

	svc := New(Config{Threshold: 5}, store, llm, logger)

	// Insert exactly 5 gaps — hits threshold.
	for i := 0; i < 5; i++ {
		if err := store.InsertPromptGap(ctx, "patch1", "repo1", fmt.Sprintf("gap %d", i)); err != nil {
			t.Fatalf("insert gap: %v", err)
		}
	}

	result, err := svc.CheckAndRefine(ctx)
	if err != nil {
		t.Fatalf("check and refine: %v", err)
	}
	if !result.Triggered {
		t.Fatal("expected triggered at threshold")
	}
	if result.GapsProcessed != 5 {
		t.Errorf("expected 5 gaps processed, got %d", result.GapsProcessed)
	}
	if !result.Activated {
		t.Error("expected version to be activated")
	}
	if llm.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", llm.calls)
	}

	// Verify the version is active in the store.
	active, err := store.GetActivePromptVersion(ctx, PromptNameReviewerSystem)
	if err != nil {
		t.Fatalf("get active version: %v", err)
	}
	if active.Content != refinedPrompt {
		t.Errorf("active content = %q, want %q", active.Content, refinedPrompt)
	}
	if active.Version != 1 {
		t.Errorf("version = %d, want 1", active.Version)
	}

	// Verify gaps are consumed.
	remaining, err := store.CountUnconsumedPromptGaps(ctx)
	if err != nil {
		t.Fatalf("count unconsumed: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 remaining gaps, got %d", remaining)
	}
}

func TestCheckAndRefinePropagatesActiveVersionLookupError(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	if err := store.InsertPromptGap(ctx, "patch1", "repo1", "gap"); err != nil {
		t.Fatalf("insert gap: %v", err)
	}

	svc := New(Config{Threshold: 1}, store, &fakeLLM{responses: []string{"refined"}}, slog.Default())
	svc.store = &failingPromptStore{promptStore: store, activeErr: fmt.Errorf("database unavailable")}

	if _, err := svc.CheckAndRefine(ctx); err == nil || !strings.Contains(err.Error(), "get active prompt version") {
		t.Fatalf("expected active version lookup error, got %v", err)
	}
}

func TestCheckAndRefineUsesActiveVersionAsBase(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Pre-seed an active version.
	v1Content := "Version 1 reviewer prompt."
	id, err := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, v1Content, 0, "")
	if err != nil {
		t.Fatalf("insert v1: %v", err)
	}
	if err := store.ActivatePromptVersion(ctx, id); err != nil {
		t.Fatalf("activate v1: %v", err)
	}

	var capturedUser string
	llm := &fakeLLM{responses: []string{"refined v2 prompt"}}
	// Wrap to capture the user prompt.
	wrappedLLM := &capturingLLM{inner: llm, captured: &capturedUser}

	svc := New(Config{Threshold: 2}, store, wrappedLLM, logger)

	for i := 0; i < 2; i++ {
		if err := store.InsertPromptGap(ctx, "patch2", "repo2", fmt.Sprintf("new gap %d", i)); err != nil {
			t.Fatalf("insert gap: %v", err)
		}
	}

	result, err := svc.CheckAndRefine(ctx)
	if err != nil {
		t.Fatalf("check and refine: %v", err)
	}
	if !result.Triggered {
		t.Fatal("expected triggered")
	}

	// The user prompt should contain the V1 content as the current prompt.
	if capturedUser == "" {
		t.Fatal("LLM user prompt was not captured")
	}
	if !contains(capturedUser, v1Content) {
		t.Errorf("user prompt should contain v1 content, got: %s", capturedUser)
	}

	// V2 should be active now.
	active, err := store.GetActivePromptVersion(ctx, PromptNameReviewerSystem)
	if err != nil {
		t.Fatalf("get active version: %v", err)
	}
	if active.Version != 2 {
		t.Errorf("expected version 2, got %d", active.Version)
	}
	if active.ParentVersion != 1 {
		t.Errorf("expected parent version 1, got %d", active.ParentVersion)
	}
}

func TestEvalAndMaybeRollbackPropagatesActiveVersionLookupError(t *testing.T) {
	store := setupStore(t)
	svc := New(Config{}, store, nil, slog.Default())
	svc.store = &failingPromptStore{promptStore: store, activeErr: fmt.Errorf("database unavailable")}

	if _, err := svc.EvalAndMaybeRollback(context.Background()); err == nil || !strings.Contains(err.Error(), "get active prompt version") {
		t.Fatalf("expected active version lookup error, got %v", err)
	}
}

func TestEvalAndMaybeRollbackNoRegression(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create v1 (parent) with a known eval score.
	id1, _ := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, "v1", 0, "")
	store.ActivatePromptVersion(ctx, id1)
	store.SetPromptVersionEvalScore(ctx, id1, 0.85)

	// Create and activate v2.
	id2, _ := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, "v2", 1, "1,2")
	store.ActivatePromptVersion(ctx, id2)

	// Insert an eval run with recall >= parent.
	store.InsertEvalRun(ctx, "ds1", 10, 10, 10, 9, 1, 1, 0.90, 0.1, 0.05, 0.95, "")

	svc := New(Config{EvalScoreTolerance: 0.05}, store, nil, logger)
	result, err := svc.EvalAndMaybeRollback(ctx)
	if err != nil {
		t.Fatalf("eval and rollback: %v", err)
	}
	if result.RolledBack {
		t.Error("should not have rolled back — score improved")
	}
}

func TestEvalAndMaybeRollbackPropagatesParentLookupError(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	id1, _ := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, "v1", 0, "")
	store.ActivatePromptVersion(ctx, id1)
	store.SetPromptVersionEvalScore(ctx, id1, 0.85)
	id2, _ := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, "v2", 1, "")
	store.ActivatePromptVersion(ctx, id2)
	store.InsertEvalRun(ctx, "ds1", 10, 10, 10, 5, 5, 5, 0.50, 0.5, 0.1, 0.6, "")

	svc := New(Config{}, store, nil, slog.Default())
	svc.store = &failingPromptStore{promptStore: store, parentErr: fmt.Errorf("database unavailable")}

	if _, err := svc.EvalAndMaybeRollback(ctx); err == nil || !strings.Contains(err.Error(), "get parent eval score") {
		t.Fatalf("expected parent lookup error, got %v", err)
	}
}

func TestEvalAndMaybeRollbackOnRegression(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create v1 with eval score.
	id1, _ := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, "v1", 0, "")
	store.ActivatePromptVersion(ctx, id1)
	store.SetPromptVersionEvalScore(ctx, id1, 0.85)

	// Create and activate v2.
	id2, _ := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, "v2", 1, "1,2")
	store.ActivatePromptVersion(ctx, id2)

	// Insert an eval run with recall well below parent.
	store.InsertEvalRun(ctx, "ds1", 10, 10, 10, 5, 5, 5, 0.50, 0.5, 0.1, 0.6, "")

	svc := New(Config{EvalScoreTolerance: 0.05}, store, nil, logger)
	result, err := svc.EvalAndMaybeRollback(ctx)
	if err != nil {
		t.Fatalf("eval and rollback: %v", err)
	}
	if !result.RolledBack {
		t.Error("should have rolled back — recall dropped from 0.85 to 0.50")
	}

	// V1 should be active again.
	active, err := store.GetActivePromptVersion(ctx, PromptNameReviewerSystem)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if active.Version != 1 {
		t.Errorf("expected v1 re-activated, got v%d", active.Version)
	}
}

func TestActiveReviewerPromptDefault(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	svc := New(Config{}, store, nil, logger)
	prompt := svc.ActiveReviewerPrompt(ctx)
	if prompt != "" {
		t.Errorf("expected empty string when no active version, got %q", prompt)
	}
}

func TestActiveReviewerPromptReturnsActive(t *testing.T) {
	ctx := context.Background()
	store := setupStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	content := "Custom reviewer prompt v1"
	id, _ := store.InsertPromptVersion(ctx, PromptNameReviewerSystem, content, 0, "")
	store.ActivatePromptVersion(ctx, id)

	svc := New(Config{}, store, nil, logger)
	prompt := svc.ActiveReviewerPrompt(ctx)
	if prompt != content {
		t.Errorf("expected %q, got %q", content, prompt)
	}
}

func TestInferGapCategory(t *testing.T) {
	tests := []struct {
		gap  string
		want string
	}{
		{"Missing SQL injection detection in user input", "security"},
		{"Fails to catch XSS in template rendering", "security"},
		{"Did not flag insecure TLS configuration", "security"},
		{"Missed nil pointer dereference after FindUser", "correctness"},
		{"Off-by-one error in pagination not detected", "correctness"},
		{"Should flag missing error handling on Write call", "correctness"},
		{"Race condition on shared map not flagged", "concurrency"},
		{"Missed potential deadlock in transfer logic", "concurrency"},
		{"Unbounded memory allocation from user input", "performance"},
		{"Cache miss rate too high for hot path", "performance"},
		{"Inconsistent naming convention not flagged", "style"},
		{"Missing documentation on exported function", "style"},
		{"Unclear gap that matches nothing specific", "other"},
	}
	for _, tt := range tests {
		got := inferGapCategory(tt.gap)
		if got != tt.want {
			t.Errorf("inferGapCategory(%q) = %q, want %q", tt.gap, got, tt.want)
		}
	}
}

func TestClusterGaps(t *testing.T) {
	gaps := []string{
		"Missing SQL injection detection",
		"Race condition on shared map not flagged",
		"Missed nil dereference after lookup",
		"XSS in template output not caught",
		"Deadlock in concurrent transfer",
	}
	clusters := clusterGaps(gaps)

	// Should be sorted alphabetically by category.
	if len(clusters) != 3 {
		t.Fatalf("expected 3 clusters, got %d", len(clusters))
	}

	// concurrency, correctness, security (alphabetical)
	if clusters[0].category != "concurrency" {
		t.Errorf("first cluster = %q, want concurrency", clusters[0].category)
	}
	if len(clusters[0].gaps) != 2 {
		t.Errorf("concurrency cluster has %d gaps, want 2", len(clusters[0].gaps))
	}
	if clusters[1].category != "correctness" {
		t.Errorf("second cluster = %q, want correctness", clusters[1].category)
	}
	if len(clusters[1].gaps) != 1 {
		t.Errorf("correctness cluster has %d gaps, want 1", len(clusters[1].gaps))
	}
	if clusters[2].category != "security" {
		t.Errorf("third cluster = %q, want security", clusters[2].category)
	}
	if len(clusters[2].gaps) != 2 {
		t.Errorf("security cluster has %d gaps, want 2", len(clusters[2].gaps))
	}
}

func TestClusterGapsEmpty(t *testing.T) {
	clusters := clusterGaps(nil)
	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters for nil input, got %d", len(clusters))
	}
}

func TestClusterGapsAllSameCategory(t *testing.T) {
	gaps := []string{
		"SQL injection in query builder",
		"Missing XSS escaping in template",
		"Hardcoded credentials in config",
	}
	clusters := clusterGaps(gaps)
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}
	if clusters[0].category != "security" {
		t.Errorf("category = %q, want security", clusters[0].category)
	}
	if len(clusters[0].gaps) != 3 {
		t.Errorf("cluster has %d gaps, want 3", len(clusters[0].gaps))
	}
}

func TestRefinementUserPromptClustered(t *testing.T) {
	gaps := []string{
		"Missing SQL injection check",
		"Race condition not detected",
		"Nil pointer after lookup",
	}
	result := refinementUserPrompt("test prompt", gaps)

	// Should contain category headers.
	if !contains(result, "### SECURITY") {
		t.Error("expected SECURITY cluster header in prompt")
	}
	if !contains(result, "### CONCURRENCY") {
		t.Error("expected CONCURRENCY cluster header in prompt")
	}
	if !contains(result, "### CORRECTNESS") {
		t.Error("expected CORRECTNESS cluster header in prompt")
	}
	if !contains(result, "clustered by category") {
		t.Error("expected 'clustered by category' header")
	}
}

func TestStripCodeFences(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"```\nhello\n```", "hello"},
		{"```text\nhello world\n```", "hello world"},
		{"no fences here", "no fences here"},
	}
	for _, tt := range tests {
		got := stripCodeFences(tt.input)
		if got != tt.want {
			t.Errorf("stripCodeFences(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// capturingLLM wraps another LLMClient and captures the user prompt.
type capturingLLM struct {
	inner    LLMClient
	captured *string
}

func (c *capturingLLM) ChatCompletion(ctx context.Context, req reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	*c.captured = req.User
	return c.inner.ChatCompletion(ctx, req)
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 ||
		findSubstring(haystack, needle))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
