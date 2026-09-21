package reviewsession

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

type byteCounter struct{}

func (byteCounter) Count(text string) int { return len(text) }

func newTestStore(t *testing.T) (*SQLStore, *db.Store, *time.Time) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	store, err := NewSQLiteStore(database.DB(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return store, database, &now
}

func createSession(t *testing.T, store *SQLStore, now time.Time) Reservation {
	t.Helper()
	reservation, err := store.Create(context.Background(), CreateParams{
		ChatID: "0123456789abcdef0123456789abcdef",
		Owner:  Owner{Kind: "nostr", ID: "owner"},
		Mode:   ModePatch,
		Snapshot: Snapshot{
			ID: "snapshot", Kind: "mutable_copy", StoragePath: "snapshot",
			ManifestHash: "manifest", DiffHash: "diff", ExpiresAt: now.Add(time.Hour),
		},
		TargetEnvelope: json.RawMessage(`{"target":"bound"}`),
		BundleHash:     "bundle", LeaseID: "lease", RequestID: "start",
		Artifacts: []Artifact{{Kind: "patch", Hash: "diff", Mandatory: true}},
		ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

func TestStoreReserveTurnCASAndRequestReplay(t *testing.T) {
	store, _, now := newTestStore(t)
	createSession(t, store, *now)
	if err := store.CompleteTurn(context.Background(), "0123456789abcdef0123456789abcdef", "start",
		json.RawMessage(`{"Review":{"summary":"initial","findings":[]}}`)); err != nil {
		t.Fatal(err)
	}

	params := ReserveTurnParams{
		ChatID: "0123456789abcdef0123456789abcdef",
		Owner:  Owner{Kind: "nostr", ID: "owner"}, RequestText: "follow up",
		ExpectedVersion: 0, ExpiresAt: now.Add(time.Hour),
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"request-a", "request-b"} {
		wg.Add(1)
		go func(requestID string) {
			defer wg.Done()
			p := params
			p.RequestID = requestID
			_, err := store.ReserveTurn(context.Background(), p)
			errs <- err
		}(id)
	}
	wg.Wait()
	close(errs)
	var success int
	for err := range errs {
		if err == nil {
			success++
			continue
		}
		if !errors.Is(err, ErrVersionConflict) && !errors.Is(err, ErrActiveTurn) {
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("successful reservations = %d, want 1", success)
	}

	loaded, err := store.LoadForContinuation(context.Background(), params.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	active := loaded.Session.ActiveRequest
	if err := store.CompleteTurn(context.Background(), params.ChatID, active,
		json.RawMessage(`{"Review":{"summary":"done","findings":[]}}`)); err != nil {
		t.Fatal(err)
	}
	replayParams := params
	replayParams.RequestID = active
	replay, err := store.ReserveTurn(context.Background(), replayParams)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay || replay.Turn.Status != TurnComplete {
		t.Fatalf("replay = %+v", replay)
	}
	replayParams.RequestText = "different"
	if _, err := store.ReserveTurn(context.Background(), replayParams); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different payload error = %v", err)
	}
}

func TestStorePersistsOrderedMessagesAndExpiresBeforeDereference(t *testing.T) {
	store, database, now := newTestStore(t)
	createSession(t, store, *now)
	chatID := "0123456789abcdef0123456789abcdef"
	if err := store.AppendMessages(context.Background(), chatID, "start", []Message{
		{Role: reviewengine.MessageRoleAssistant, Content: "inspect", ToolCalls: []reviewengine.ToolCall{{
			ID: "call-1", Type: "function", Function: reviewengine.ToolCallFunction{Name: "code.read", Arguments: `{"path":"a.go"}`},
		}}},
		{Role: reviewengine.MessageRoleTool, ToolCallID: "call-1", Name: "code.read", Content: "content"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteTurn(context.Background(), chatID, "start",
		json.RawMessage(`{"Review":{"summary":"ok","findings":[]}}`)); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadForContinuation(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[0].Seq != 0 || loaded.Messages[1].Seq != 1 {
		t.Fatalf("messages = %+v", loaded.Messages)
	}
	lease, err := store.Expire(context.Background(), chatID)
	if err != nil {
		t.Fatal(err)
	}
	if lease != "lease" {
		t.Fatalf("lease = %q", lease)
	}
	var state string
	var refs int
	if err := database.DB().QueryRowContext(context.Background(), `SELECT state FROM review_sessions WHERE chat_id=?`, chatID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := database.DB().QueryRowContext(context.Background(), `SELECT ref_count FROM review_snapshots WHERE snapshot_id='snapshot'`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if state != "expired" || refs != 0 {
		t.Fatalf("state=%s refs=%d", state, refs)
	}
}

func TestCompactHistoryKeepsNewestTurnRawAndCompactsOlderResults(t *testing.T) {
	loaded := Loaded{
		Turns: []Turn{
			{TurnNo: 0, RequestText: "first question", Status: TurnComplete,
				Result: json.RawMessage(`{"Review":{"summary":"first summary","findings":[{"file":"a.go"}]}}`)},
			{TurnNo: 1, RequestText: "second question", Status: TurnComplete,
				Result: json.RawMessage(`{"Review":{"summary":"second summary","findings":[]}}`)},
		},
		Messages: []Message{
			{TurnNo: 0, Seq: 0, Role: reviewengine.MessageRoleUser, Content: "first question"},
			{TurnNo: 0, Seq: 1, Role: reviewengine.MessageRoleAssistant, Content: strings.Repeat("raw transcript ", 100)},
			{TurnNo: 1, Seq: 0, Role: reviewengine.MessageRoleUser, Content: "second question"},
			{TurnNo: 1, Seq: 1, Role: reviewengine.MessageRoleAssistant, Content: "newest raw"},
		},
	}
	history, err := CompactHistory(loaded, byteCounter{}, 350)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || history[1].Content[:20] != "Prior review result:" ||
		history[3].Content != "newest raw" {
		t.Fatalf("compacted history = %+v", history)
	}
	if _, err := CompactHistory(loaded, byteCounter{}, 10); !errors.Is(err, ErrHistoryTooLarge) {
		t.Fatalf("tiny budget error = %v", err)
	}
}

// TestCompactedTurnResultDecodesMarshaledRunOutput exercises the exact shape
// production writes — json.Marshal(reviewengine.RunOutput) — which the old
// capitalised fixtures never covered.
func TestCompactedTurnResultDecodesMarshaledRunOutput(t *testing.T) {
	raw, err := json.Marshal(reviewengine.RunOutput{
		Review: reviewengine.ReviewerOutput{
			Summary:  "real summary",
			Findings: []reviewengine.Finding{{File: "a.go", Severity: "high"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := compactedTurnResult(Turn{TurnNo: 1, Result: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"summary":"real summary"`) || !strings.Contains(got, `"file":"a.go"`) {
		t.Fatalf("compacted result = %s", got)
	}
}

// TestCompactedTurnResultReadsFixedReviewPath proves summary/findings are read
// from the fixed .Review path and never from a sibling object that happens to
// carry the same keys. The pre-fix BFS chose between them by randomized Go map
// iteration, so this failed nondeterministically.
func TestCompactedTurnResultReadsFixedReviewPath(t *testing.T) {
	result := json.RawMessage(`{"Planner":{"summary":"WRONG","findings":[{"file":"wrong.go"}]},"Review":{"summary":"RIGHT","findings":[]}}`)
	for i := 0; i < 100; i++ {
		got, err := compactedTurnResult(Turn{TurnNo: 3, Result: result})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `"summary":"RIGHT"`) || strings.Contains(got, "WRONG") {
			t.Fatalf("compacted result read the wrong object: %s", got)
		}
	}
}
