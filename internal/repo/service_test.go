package repo

import (
	"testing"

	"fiatjaf.com/nostr"
)

func TestCloneURLsFromEvent(t *testing.T) {
	evt := nostr.Event{Tags: nostr.Tags{
		{"clone", "https://a.example/repo.git", "https://b.example/repo.git"},
		{"clone", "https://a.example/repo.git"},
	}}
	urls := cloneURLsFromEvent(evt)
	if len(urls) != 2 {
		t.Fatalf("expected 2 unique clone urls, got %d (%v)", len(urls), urls)
	}
}

func TestPRTipCommit(t *testing.T) {
	evt := nostr.Event{ID: nostr.MustIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Tags: nostr.Tags{{"c", "1111111111111111111111111111111111111111"}}}
	tip, err := prTipCommit(evt)
	if err != nil {
		t.Fatalf("expected tip commit, got error: %v", err)
	}
	if tip != "1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected tip commit %s", tip)
	}
}

func TestPRTipCommitMissing(t *testing.T) {
	evt := nostr.Event{ID: nostr.MustIDFromHex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}
	if _, err := prTipCommit(evt); err == nil {
		t.Fatalf("expected error for missing c tag")
	}
}
