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

	"drydock/internal/db"
	"drydock/internal/reviewengine"
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
		json.RawMessage(`{"Review":{"Summary":"initial","Findings":[]}}`)); err != nil {
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
		json.RawMessage(`{"Review":{"Summary":"done","Findings":[]}}`)); err != nil {
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
		json.RawMessage(`{"Review":{"Summary":"ok","Findings":[]}}`)); err != nil {
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
				Result: json.RawMessage(`{"Review":{"Summary":"first summary","Findings":[{"File":"a.go"}]}}`)},
			{TurnNo: 1, RequestText: "second question", Status: TurnComplete,
				Result: json.RawMessage(`{"Review":{"Summary":"second summary","Findings":[]}}`)},
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
	again, err := CompactHistory(loaded, byteCounter{}, 350)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(history)
	right, _ := json.Marshal(again)
	if string(left) != string(right) {
		t.Fatalf("compaction is not deterministic")
	}
	if _, err := CompactHistory(loaded, byteCounter{}, 10); !errors.Is(err, ErrHistoryTooLarge) {
		t.Fatalf("tiny budget error = %v", err)
	}
}
