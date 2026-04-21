package pipeline

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/metareview"
	"drydock/internal/publisher"
	"drydock/internal/repo"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

// --- Integration test helpers ---

// testSigner signs events with a deterministic key for testing.
type testSigner struct {
	sk nostr.SecretKey
}

func (s testSigner) GetPublicKey(_ context.Context) (nostr.PubKey, error) {
	return nostr.GetPublicKey(s.sk), nil
}
func (s testSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(s.sk)
}

// collectingRelayPublisher captures published events instead of sending them.
type collectingRelayPublisher struct {
	events []nostr.Event
	relays [][]string
}

func (p *collectingRelayPublisher) Publish(_ context.Context, relays []string, event nostr.Event) error {
	p.events = append(p.events, event)
	p.relays = append(p.relays, relays)
	return nil
}

// gitRun runs a git command in the given directory.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// initRepoInCache creates a git repo with an initial commit containing main.go
// directly inside the repo cache directory that the Manager will use. This
// avoids needing a network-reachable clone URL in tests.
func initRepoInCache(t *testing.T, cacheDir, repoID string) string {
	t.Helper()
	// Replicate Manager.repoPath(): replace special chars and join with baseDir.
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "__", " ", "_").Replace(repoID)
	repoPath := filepath.Join(cacheDir, safe)

	os.MkdirAll(repoPath, 0o755)
	gitRun(t, repoPath, "init", "-b", "master")
	os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n"), 0o644)
	gitRun(t, repoPath, "add", "main.go")
	gitRun(t, repoPath, "commit", "-m", "initial")

	return repoPath
}

// makePatchDiff returns a valid unified diff that adds a comment to main.go.
func makePatchDiff() string {
	return "diff --git a/main.go b/main.go\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1 +1,2 @@\n" +
		" package main\n" +
		"+// reviewed\n"
}

// seedIntegrationDB creates DB entries for a repo announcement and patch event.
// Returns the patch event ID and repo ID.
func seedIntegrationDB(t *testing.T, ctx context.Context, store *db.Store) (patchEventID, repoID string) {
	t.Helper()

	repoSK := nostr.Generate()
	patchSK := nostr.Generate()

	// Use a dummy https URL — the repo is pre-cloned into the cache so
	// EnsureRepo will find the .git dir and just run fetch.
	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "integ-repo"},
			{"clone", "https://example.com/integ-repo.git"},
			{"relays", "wss://relay.test"},
		},
	}
	repoEvt.Sign(repoSK)
	if err := store.UpsertRepositoryAnnouncement(ctx, repoEvt); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	patchEvt := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":integ-repo"},
			{"e", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", "root"},
		},
		Content: makePatchDiff(),
	}
	patchEvt.Sign(patchSK)
	if err := store.InsertPatchEvent(ctx, patchEvt); err != nil {
		t.Fatalf("seed patch: %v", err)
	}
	if err := store.RecordPatchEventRelay(ctx, patchEvt.ID.Hex(), "wss://relay.test"); err != nil {
		t.Fatalf("seed relay: %v", err)
	}

	rID := db.RepoIDFromPatch(patchEvt)
	if _, err := store.BeginReview(ctx, patchEvt.ID.Hex(), rID); err != nil {
		t.Fatalf("begin review: %v", err)
	}
	return patchEvt.ID.Hex(), rID
}

// --- Integration tests ---

func TestIntegrationFullPipelineProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// 1. Real DB
	dbPath := filepath.Join(t.TempDir(), "integ.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	patchID, repoID := seedIntegrationDB(t, ctx, store)

	// 2. Pre-clone repo into cache so EnsureRepo finds it (avoids clone URL issues)
	cacheDir := filepath.Join(t.TempDir(), "repos")
	initRepoInCache(t, cacheDir, repoID)

	// 3. Real repo service
	repoMgr := repo.NewManager(cacheDir, logger)
	repoSvc := repo.NewService(store, repoMgr, logger)

	// 4. Real context builder (no optional services)
	ctxBuilder := contextbuilder.NewDefault()

	// 5. Fake LLM that returns planner + reviewer responses
	fakeLLM := &reviewengine.FakeLLMForTest{
		Responses: []string{
			`{"change_type":"feature","risk_areas":["correctness"],"needed_context":[],"review_focus":"logic","model_route":"coder32b"}`,
			`{"summary":"Code looks clean.","findings":[{"severity":"info","category":"style","file":"main.go","line":2,"evidence":"comment","explanation":"trivial comment added","suggestion":"consider docstring","confidence":0.75}],"needs_more_context":[]}`,
		},
	}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "planner"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder32b"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "llm70b"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "coder14b"},
	}, fakeLLM, logger)

	// 6. Real publisher with fake signer and collecting relay publisher
	signerKey := nostr.Generate()
	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays:       []string{"wss://relay.test"},
		DetailSeverityFloor: "high",
		DefaultTTL:          90 * 24 * time.Hour,
		SupersededTTL:       7 * 24 * time.Hour,
	}, store, testSigner{sk: signerKey}, relayPub, logger)

	// 7. Real meta-review service (with fake LLM — won't trigger for this test)
	metaLLM := &reviewengine.FakeLLMForTest{
		Responses: []string{
			`{"missed_findings":[],"false_positives":[],"reasoning_quality":0.9,"context_utilization":0.8,"prompt_gaps":[],"suggested_few_shot":false}`,
		},
	}
	metaSvc := metareview.New(metareview.Config{
		Endpoint:         reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "meta"},
		RandomSampleRate: 0, // disable random triggering
		MaxConcurrent:    1,
	}, store, metaLLM, logger)

	// 8. Build and run pipeline
	queue := make(chan db.ReviewTask, 1)
	runner := New(
		Config{Workers: 1},
		store, repoSvc, ctxBuilder, engine, pubSvc, metaSvc,
		queue, logger,
	)

	// Process the review task directly
	err = runner.process(ctx, db.ReviewTask{
		PatchEventID: patchID,
		RepoID:       repoID,
	})
	if err != nil {
		t.Fatalf("process failed: %v", err)
	}

	// --- Assertions ---

	// Should have published at least the summary event
	if len(relayPub.events) == 0 {
		t.Fatal("expected at least one published event")
	}

	summaryEvt := relayPub.events[0]

	// Kind should be 1111 (NIP-22 comment)
	if summaryEvt.Kind != nostr.KindComment {
		t.Fatalf("expected kind %d, got %d", nostr.KindComment, summaryEvt.Kind)
	}

	// Content should contain the summary
	if !strings.Contains(summaryEvt.Content, "Code looks clean") {
		t.Fatalf("expected summary in content, got: %s", summaryEvt.Content)
	}

	// Footer should contain metadata fields
	if !strings.Contains(summaryEvt.Content, "model:") {
		t.Fatal("missing model footer field")
	}
	if !strings.Contains(summaryEvt.Content, "context-hash:") {
		t.Fatal("missing context-hash footer field")
	}
	if !strings.Contains(summaryEvt.Content, "excluded-files:") {
		t.Fatal("missing excluded-files footer field")
	}

	// Should have correct tags (root scope)
	hasTag := func(name string) bool {
		for _, tag := range summaryEvt.Tags {
			if len(tag) > 0 && tag[0] == name {
				return true
			}
		}
		return false
	}
	if !hasTag("E") {
		t.Fatal("missing E root tag")
	}
	if !hasTag("K") {
		t.Fatal("missing K root kind tag")
	}

	// Relays should include the relay from the repo/patch
	if len(relayPub.relays) == 0 {
		t.Fatal("no relay lists recorded")
	}
	relayFound := false
	for _, r := range relayPub.relays[0] {
		if r == "wss://relay.test" {
			relayFound = true
		}
	}
	if !relayFound {
		t.Fatalf("expected wss://relay.test in relay list, got: %v", relayPub.relays[0])
	}

	// LLM should have received exactly 2 calls (planner + reviewer)
	if len(fakeLLM.Requests) != 2 {
		t.Fatalf("expected 2 LLM calls (planner + reviewer), got %d", len(fakeLLM.Requests))
	}

	// Review should be marked as published in the DB
	status, err := store.GetReviewStatus(ctx, patchID, repoID)
	if err != nil {
		t.Fatalf("get review status: %v", err)
	}
	if status != "published" {
		t.Fatalf("expected review status 'published', got %q", status)
	}
}

func TestIntegrationApplyFailurePublishesHint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	// 1. Real DB
	dbPath := filepath.Join(t.TempDir(), "integ-fail.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Seed with a patch that won't apply (references non-existent context lines)
	repoSK := nostr.Generate()
	patchSK := nostr.Generate()

	repoEvt := nostr.Event{
		Kind:      30617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"d", "fail-repo"},
			{"clone", "https://example.com/fail-repo.git"},
			{"relays", "wss://relay.test"},
		},
	}
	repoEvt.Sign(repoSK)
	store.UpsertRepositoryAnnouncement(ctx, repoEvt)

	badDiff := "diff --git a/nonexistent.go b/nonexistent.go\n" +
		"--- a/nonexistent.go\n" +
		"+++ b/nonexistent.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package old\n" +
		" func Existing() {}\n" +
		" func Other() {}\n" +
		"+func New() {}\n"

	patchEvt := nostr.Event{
		Kind:      1617,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"a", "30617:" + nostr.GetPublicKey(repoSK).Hex() + ":fail-repo"},
			{"e", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "", "root"},
		},
		Content: badDiff,
	}
	patchEvt.Sign(patchSK)
	store.InsertPatchEvent(ctx, patchEvt)
	store.RecordPatchEventRelay(ctx, patchEvt.ID.Hex(), "wss://relay.test")

	rID := db.RepoIDFromPatch(patchEvt)
	store.BeginReview(ctx, patchEvt.ID.Hex(), rID)

	// 2. Pre-clone repo into cache with only main.go — the bad diff won't apply
	cacheDir := filepath.Join(t.TempDir(), "repos")
	initRepoInCache(t, cacheDir, rID)

	// 3. Build pipeline
	repoMgr := repo.NewManager(cacheDir, logger)
	repoSvc := repo.NewService(store, repoMgr, logger)
	ctxBuilder := contextbuilder.NewDefault()

	fakeLLM := &reviewengine.FakeLLMForTest{}
	engine := reviewengine.New(reviewengine.Config{
		Planner:  reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "p"},
		Coder32B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "c"},
		LLM70B:   reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "l"},
		Coder14B: reviewengine.ModelEndpoint{BaseURL: "http://test", Model: "s"},
	}, fakeLLM, logger)

	signerKey := nostr.Generate()
	relayPub := &collectingRelayPublisher{}
	pubSvc := publisher.New(publisher.Config{
		DefaultRelays:       []string{"wss://relay.test"},
		DetailSeverityFloor: "high",
		DefaultTTL:          90 * 24 * time.Hour,
		SupersededTTL:       7 * 24 * time.Hour,
	}, store, testSigner{sk: signerKey}, relayPub, logger)

	queue := make(chan db.ReviewTask, 1)
	runner := New(
		Config{Workers: 1},
		store, repoSvc, ctxBuilder, engine, pubSvc, nil,
		queue, logger,
	)

	// process should return an error (patch doesn't apply)
	err = runner.process(ctx, db.ReviewTask{
		PatchEventID: patchEvt.ID.Hex(),
		RepoID:       rID,
	})
	if err == nil {
		t.Fatal("expected process to fail for unapplyable patch")
	}

	// But it should have published an apply-failure review comment
	if len(relayPub.events) == 0 {
		t.Fatal("expected apply-failure review to be published")
	}
	if !strings.Contains(relayPub.events[0].Content, "does not apply cleanly") {
		t.Fatalf("expected apply-failure hint in content, got: %s", relayPub.events[0].Content)
	}

	// LLM should NOT have been called (patch didn't apply)
	if len(fakeLLM.Requests) != 0 {
		t.Fatalf("expected 0 LLM calls for apply failure, got %d", len(fakeLLM.Requests))
	}
}
