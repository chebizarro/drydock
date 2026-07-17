package db

import (
	"context"
	"testing"
	"time"
)

func storeChat(t *testing.T, store *Store, ctx context.Context, eventID, sender, repo, question, response string) {
	t.Helper()
	turn := CodeChatTurn{
		EventID:      eventID,
		SenderPubKey: sender,
		RepoID:       repo,
		Question:     question,
		CreatedAt:    time.Now().Unix(),
	}
	_, err := store.BeginCodeChatTurn(ctx, turn, 100)
	if err != nil {
		t.Fatalf("BeginCodeChatTurn: %v", err)
	}
	if err := store.SetCodeChatResponse(ctx, eventID, response); err != nil {
		t.Fatalf("SetCodeChatResponse: %v", err)
	}
	if err := store.MarkCodeChatPublished(ctx, eventID); err != nil {
		t.Fatalf("MarkCodeChatPublished: %v", err)
	}
}

func TestBeginCodeChatTurn(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	turn := CodeChatTurn{
		EventID:      "event-1",
		SenderPubKey: "sender-1",
		RepoID:       "repo-1",
		Question:     "What does main do?",
		CreatedAt:    time.Now().Unix(),
	}

	// First turn should succeed
	turnNum, err := store.BeginCodeChatTurn(ctx, turn, 10)
	if err != nil {
		t.Fatalf("BeginCodeChatTurn: %v", err)
	}
	if turnNum != 1 {
		t.Errorf("expected turn 1, got %d", turnNum)
	}

	// Duplicate event should return 0
	turnNum, err = store.BeginCodeChatTurn(ctx, turn, 10)
	if err != nil {
		t.Fatalf("BeginCodeChatTurn (duplicate): %v", err)
	}
	if turnNum != 0 {
		t.Errorf("expected 0 for duplicate, got %d", turnNum)
	}
}

func TestBeginCodeChatTurn_RateLimit(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create max turns
	for i := 0; i < 3; i++ {
		turn := CodeChatTurn{
			EventID:      "event-" + string(rune('a'+i)),
			SenderPubKey: "sender-1",
			RepoID:       "repo-1",
			Question:     "Q",
			CreatedAt:    time.Now().Unix(),
		}
		_, err := store.BeginCodeChatTurn(ctx, turn, 3)
		if err != nil {
			t.Fatalf("BeginCodeChatTurn %d: %v", i, err)
		}
	}

	// Next turn should be rate limited
	turn := CodeChatTurn{
		EventID:      "event-exceed",
		SenderPubKey: "sender-1",
		RepoID:       "repo-1",
		Question:     "Q",
		CreatedAt:    time.Now().Unix(),
	}
	_, err := store.BeginCodeChatTurn(ctx, turn, 3)
	if err != ErrCodeChatRateLimited {
		t.Errorf("expected ErrCodeChatRateLimited, got %v", err)
	}
}

func TestGetCodeChatHistory(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Store some chat messages
	storeChat(t, store, ctx, "event-1", "sender-1", "repo-1", "What does main do?", "It starts the app.")
	storeChat(t, store, ctx, "event-2", "sender-1", "repo-1", "How about init?", "It initializes things.")
	storeChat(t, store, ctx, "event-3", "sender-2", "repo-1", "Other user question", "Different answer.")

	// Get history for sender-1
	history, err := store.GetCodeChatHistory(ctx, "sender-1", "repo-1", 10)
	if err != nil {
		t.Fatalf("GetCodeChatHistory: %v", err)
	}

	if len(history) != 2 {
		t.Errorf("expected 2 messages for sender-1, got %d", len(history))
	}
}

func TestGetCodeChatHistory_Limit(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Store 5 messages
	for i := 0; i < 5; i++ {
		storeChat(t, store, ctx, "event-"+string(rune('a'+i)), "sender-1", "repo-1", "Q", "A")
	}

	// Get only 3
	history, err := store.GetCodeChatHistory(ctx, "sender-1", "repo-1", 3)
	if err != nil {
		t.Fatalf("GetCodeChatHistory: %v", err)
	}

	if len(history) != 3 {
		t.Errorf("expected 3 messages (limit), got %d", len(history))
	}
}

func TestMarkCodeChatFailed(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// Create a turn first
	turn := CodeChatTurn{
		EventID:      "event-1",
		SenderPubKey: "sender-1",
		RepoID:       "repo-1",
		Question:     "Q",
		CreatedAt:    time.Now().Unix(),
	}
	store.BeginCodeChatTurn(ctx, turn, 10)

	// Mark as failed (should not error)
	if err := store.MarkCodeChatFailed(ctx, "event-1"); err != nil {
		t.Fatalf("MarkCodeChatFailed: %v", err)
	}
	turnNumber, err := store.BeginCodeChatTurn(ctx, turn, 10)
	if err != nil {
		t.Fatalf("BeginCodeChatTurn retry: %v", err)
	}
	if turnNumber == 0 {
		t.Fatal("failed codechat turn was not admitted for retry")
	}

	// Mark non-existent (should not error - no-op)
	if err := store.MarkCodeChatFailed(ctx, "nonexistent"); err != nil {
		t.Fatalf("MarkCodeChatFailed (nonexistent): %v", err)
	}
}

func TestGetLastCodeChatRepo(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	// No history - should return empty
	repo, err := store.GetLastCodeChatRepo(ctx, "sender-1")
	if err != nil {
		t.Fatalf("GetLastCodeChatRepo: %v", err)
	}
	if repo != "" {
		t.Errorf("expected empty repo for new user, got %q", repo)
	}

	// Store a message
	storeChat(t, store, ctx, "event-1", "sender-1", "repo-1", "Q1", "A1")

	// Should return the repo
	repo, err = store.GetLastCodeChatRepo(ctx, "sender-1")
	if err != nil {
		t.Fatalf("GetLastCodeChatRepo: %v", err)
	}
	if repo != "repo-1" {
		t.Errorf("expected 'repo-1', got %q", repo)
	}
}

func TestGetCodeChatHistory_Empty(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	history, err := store.GetCodeChatHistory(ctx, "nonexistent", "repo-1", 10)
	if err != nil {
		t.Fatalf("GetCodeChatHistory: %v", err)
	}

	if len(history) != 0 {
		t.Errorf("expected empty history, got %d messages", len(history))
	}
}
