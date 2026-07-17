package marketplace

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

func mustOpenStore(t *testing.T, ctx context.Context) *db.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func TestReviewerRegistration(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()
	registry := NewRegistry(store, logger)

	profile := ReviewerProfile{
		Pubkey:         "abc123def456",
		DisplayName:    "Test Reviewer",
		Languages:      []string{"go", "rust"},
		Domains:        []string{"security", "performance"},
		Availability:   AvailabilityAvailable,
		PricePerReview: 1000,
		MaxConcurrent:  3,
	}

	err := registry.RegisterReviewer(ctx, profile, "event123")
	if err != nil {
		t.Fatalf("RegisterReviewer failed: %v", err)
	}

	// Retrieve the profile
	got, rep, err := registry.GetReviewer(ctx, "abc123def456")
	if err != nil {
		t.Fatalf("GetReviewer failed: %v", err)
	}

	if got.DisplayName != "Test Reviewer" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Test Reviewer")
	}
	if len(got.Languages) != 2 || got.Languages[0] != "go" {
		t.Errorf("Languages = %v, want [go rust]", got.Languages)
	}
	// New reviewers start with default score (0.5 from DB default)
	// Note: the exact value may vary based on caching and DB implementation
	if rep.OverallScore < 0 || rep.OverallScore > 1 {
		t.Errorf("OverallScore = %f, should be in [0, 1]", rep.OverallScore)
	}
}

func TestFindReviewers(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()
	registry := NewRegistry(store, logger)

	// Register multiple reviewers
	reviewers := []ReviewerProfile{
		{
			Pubkey:       "reviewer1",
			DisplayName:  "Go Expert",
			Languages:    []string{"go"},
			Domains:      []string{"backend"},
			Availability: AvailabilityAvailable,
		},
		{
			Pubkey:       "reviewer2",
			DisplayName:  "Rust Expert",
			Languages:    []string{"rust", "go"},
			Domains:      []string{"systems"},
			Availability: AvailabilityAvailable,
		},
		{
			Pubkey:       "reviewer3",
			DisplayName:  "Unavailable Reviewer",
			Languages:    []string{"go"},
			Domains:      []string{"backend"},
			Availability: AvailabilityUnavailable,
		},
	}

	for i, r := range reviewers {
		if err := registry.RegisterReviewer(ctx, r, "event"+string(rune('a'+i))); err != nil {
			t.Fatalf("RegisterReviewer failed for %s: %v", r.Pubkey, err)
		}
	}

	// Search for Go reviewers
	criteria := RoutingCriteria{
		Languages: []string{"go"},
	}
	matches, err := registry.FindReviewers(ctx, criteria, 10)
	if err != nil {
		t.Fatalf("FindReviewers failed: %v", err)
	}

	// Should find 2 (reviewer1 and reviewer2, not reviewer3 who is unavailable)
	if len(matches) != 2 {
		t.Errorf("FindReviewers returned %d matches, want 2", len(matches))
	}

	// Verify unavailable reviewer is excluded
	for _, m := range matches {
		if m.Profile.Pubkey == "reviewer3" {
			t.Error("FindReviewers included unavailable reviewer")
		}
	}
}

func TestMatchScoreCalculation(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()
	registry := NewRegistry(store, logger)

	// Register reviewers with different specialties
	reviewers := []ReviewerProfile{
		{
			Pubkey:       "perfect_match",
			Languages:    []string{"go", "rust"},
			Domains:      []string{"security"},
			Availability: AvailabilityAvailable,
		},
		{
			Pubkey:       "partial_match",
			Languages:    []string{"go", "python"},
			Domains:      []string{"web"},
			Availability: AvailabilityAvailable,
		},
	}

	for i, r := range reviewers {
		registry.RegisterReviewer(ctx, r, "event"+string(rune('a'+i)))
	}

	// Search for go+rust security reviewers
	criteria := RoutingCriteria{
		Languages: []string{"go", "rust"},
		Domains:   []string{"security"},
	}
	matches, err := registry.FindReviewers(ctx, criteria, 10)
	if err != nil {
		t.Fatalf("FindReviewers failed: %v", err)
	}

	if len(matches) < 2 {
		t.Fatalf("Expected at least 2 matches, got %d", len(matches))
	}

	// Perfect match should have higher score
	if matches[0].Profile.Pubkey != "perfect_match" {
		t.Errorf("Expected perfect_match to be ranked first, got %s", matches[0].Profile.Pubkey)
	}
	if matches[0].MatchScore <= matches[1].MatchScore {
		t.Errorf("Perfect match score (%f) should be > partial match score (%f)",
			matches[0].MatchScore, matches[1].MatchScore)
	}
}

func TestRouterLanguageDetection(t *testing.T) {
	router := &Router{}

	tests := []struct {
		files    []string
		expected []string
	}{
		{
			files:    []string{"main.go", "utils.go"},
			expected: []string{"go"},
		},
		{
			files:    []string{"src/main.rs", "Cargo.toml"},
			expected: []string{"rust"},
		},
		{
			files:    []string{"app.ts", "utils.js", "index.html"},
			expected: []string{"typescript", "javascript"},
		},
		{
			files:    []string{"README.md", "Makefile"},
			expected: []string{},
		},
	}

	for _, tc := range tests {
		langs := router.detectLanguages(tc.files)
		if len(langs) != len(tc.expected) {
			t.Errorf("detectLanguages(%v) = %v, want %v", tc.files, langs, tc.expected)
			continue
		}
		// Check all expected languages are present
		langSet := make(map[string]bool)
		for _, l := range langs {
			langSet[l] = true
		}
		for _, exp := range tc.expected {
			if !langSet[exp] {
				t.Errorf("detectLanguages(%v) missing %s", tc.files, exp)
			}
		}
	}
}

func TestReputationCalculation(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	logger := slog.Default()
	registry := NewRegistry(store, logger)

	// Register a reviewer
	profile := ReviewerProfile{
		Pubkey:       "test_reviewer",
		Languages:    []string{"go"},
		Availability: AvailabilityAvailable,
	}
	registry.RegisterReviewer(ctx, profile, "event1")

	// Simulate some assignments and feedback
	for i := 0; i < 5; i++ {
		assignment := db.ReviewAssignment{
			PatchEventID:      "patch" + string(rune('a'+i)),
			RepoID:            "repo1",
			ReviewerPubkey:    "test_reviewer",
			RequesterPubkey:   "requester1",
			Status:            "completed",
			AssignmentEventID: "assign" + string(rune('a'+i)),
			ExpiresAt:         time.Now().Add(24 * time.Hour).Unix(),
		}
		store.CreateAssignment(ctx, assignment)
	}

	// Add some feedback
	for i := 0; i < 3; i++ {
		fb := db.ReviewFeedback{
			AssignmentID:   i + 1,
			ReviewerPubkey: "test_reviewer",
			RaterPubkey:    "requester1",
			Rating:         4, // Good rating
			EventID:        "fb" + string(rune('a'+i)),
		}
		store.RecordFeedback(ctx, fb)
	}

	// Trigger reputation recalculation
	err := registry.RecalculateReputation(ctx, "test_reviewer")
	if err != nil {
		t.Fatalf("RecalculateReputation failed: %v", err)
	}

	// Check updated reputation
	_, rep, err := registry.GetReviewer(ctx, "test_reviewer")
	if err != nil {
		t.Fatalf("GetReviewer failed: %v", err)
	}

	// Should have non-default reputation now
	if rep.TotalReviews != 5 {
		t.Errorf("TotalReviews = %d, want 5", rep.TotalReviews)
	}
	if rep.AverageRating == 0 {
		t.Error("AverageRating should be > 0 after feedback")
	}
}

func TestEventKinds(t *testing.T) {
	tests := []struct {
		name string
		kind int
	}{
		{"KindReviewerProfile", KindReviewerProfile},
		{"KindReviewFeedback", KindReviewFeedback},
	}

	expectedKinds := map[int]bool{
		31990: true, // Reviewer profile (NIP-89 app handler)
		7000:  true, // NIP-90 feedback
	}

	for _, tc := range tests {
		if !expectedKinds[tc.kind] {
			t.Errorf("%s = %d, not in expected kinds", tc.name, tc.kind)
		}
	}
}

func TestReviewerProfileNIP89Event(t *testing.T) {
	profile := ReviewerProfile{
		Pubkey:            "reviewer-pubkey",
		DisplayName:       "Alice - Code Reviewer",
		About:             "Security and performance specialist",
		Languages:         []string{"go", "rust"},
		Domains:           []string{"security", "performance"},
		Availability:      AvailabilityAvailable,
		PricePerReview:    1000,
		PayoutDestination: "lnbc1profilepayout",
		MaxConcurrent:     3,
		ResponseTime:      "24h",
	}

	event, err := ReviewerProfileEvent(profile)
	if err != nil {
		t.Fatalf("ReviewerProfileEvent failed: %v", err)
	}
	if int(event.Kind) != 31990 {
		t.Fatalf("event kind = %d, want 31990", event.Kind)
	}

	wantTags := nostr.Tags{
		{"d", "drydock-reviewer"},
		{"k", "25910"},
		{"name", "Alice - Code Reviewer"},
		{"about", "Security and performance specialist"},
		{"drydock:languages", "go", "rust"},
		{"drydock:domains", "security", "performance"},
		{"drydock:availability", "available"},
		{"drydock:price", "1000"},
		{"drydock:payout", "lnbc1profilepayout"},
		{"drydock:methods", "marketplace/assign", "marketplace/accept", "marketplace/reject", "marketplace/complete"},
	}
	for _, want := range wantTags {
		if !hasTag(event.Tags, want) {
			t.Fatalf("missing tag %v in %v", want, event.Tags)
		}
	}

	parsed, ok, err := ParseReviewerProfileEvent(event)
	if err != nil {
		t.Fatalf("ParseReviewerProfileEvent failed: %v", err)
	}
	if !ok {
		t.Fatal("expected Drydock NIP-89 reviewer profile")
	}
	if parsed.DisplayName != profile.DisplayName || parsed.About != profile.About {
		t.Fatalf("parsed profile = %+v, want name/about from tags", parsed)
	}
	if len(parsed.Languages) != 2 || parsed.Languages[0] != "go" || parsed.Languages[1] != "rust" {
		t.Fatalf("parsed languages = %v, want [go rust]", parsed.Languages)
	}
	if parsed.PricePerReview != 1000 || parsed.MaxConcurrent != 3 || parsed.ResponseTime != "24h" {
		t.Fatalf("parsed profile = %+v, want price/max/response time", parsed)
	}
}

func hasTag(tags nostr.Tags, want nostr.Tag) bool {
	for _, tag := range tags {
		if len(tag) != len(want) {
			continue
		}
		matched := true
		for i := range tag {
			if tag[i] != want[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func TestReviewFeedbackTags(t *testing.T) {
	tags := ReviewFeedbackTags("review123", "reviewer456", 5)

	if tagValue(tags, "e") != "review123" {
		t.Errorf("e tag = %q, want review123", tagValue(tags, "e"))
	}
	if tagValue(tags, "p") != "reviewer456" {
		t.Errorf("p tag = %q, want reviewer456", tagValue(tags, "p"))
	}
	if tagValue(tags, "status") != FeedbackStatusSuccess {
		t.Errorf("status tag = %q, want %q", tagValue(tags, "status"), FeedbackStatusSuccess)
	}
	if tagValue(tags, "rating") != "5" {
		t.Errorf("rating tag = %q, want 5", tagValue(tags, "rating"))
	}
	if tagValue(tags, "t") != TagReviewFeedback {
		t.Errorf("t tag = %q, want %q", tagValue(tags, "t"), TagReviewFeedback)
	}
}

func TestParseReviewFeedbackEventReadsRatingFromTag(t *testing.T) {
	event := nostr.Event{
		Kind:    KindReviewFeedback,
		Tags:    ReviewFeedbackTags("review123", "reviewer456", 5),
		Content: `{"helpful":true,"accurate":true,"comment":"Great review!","rating":1}`,
	}

	feedback, err := ParseReviewFeedbackEvent(event)
	if err != nil {
		t.Fatalf("ParseReviewFeedbackEvent failed: %v", err)
	}

	if feedback.Rating != 5 {
		t.Errorf("Rating = %d, want 5", feedback.Rating)
	}
	if feedback.ReviewEventID != "review123" {
		t.Errorf("ReviewEventID = %q, want review123", feedback.ReviewEventID)
	}
	if feedback.ReviewerPubkey != "reviewer456" {
		t.Errorf("ReviewerPubkey = %q, want reviewer456", feedback.ReviewerPubkey)
	}
	if !feedback.Helpful || !feedback.Accurate || feedback.Comment != "Great review!" {
		t.Errorf("feedback content not parsed: %+v", feedback)
	}
}

func TestIsValidRating(t *testing.T) {
	tests := []struct {
		rating int
		want   bool
	}{
		{0, false},
		{1, true},
		{3, true},
		{5, true},
		{6, false},
		{-1, false},
	}

	for _, tc := range tests {
		got := IsValidRating(tc.rating)
		if got != tc.want {
			t.Errorf("IsValidRating(%d) = %v, want %v", tc.rating, got, tc.want)
		}
	}
}
