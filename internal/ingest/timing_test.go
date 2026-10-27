package ingest

import (
	"testing"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/eventkind"

	"fiatjaf.com/nostr"
)

func TestEventTimestampPlausibilityExemptsControlPlaneKinds(t *testing.T) {
	old := nostr.Timestamp(time.Now().Add(-3 * 365 * 24 * time.Hour).Unix())
	maxAge := 365 * 24 * time.Hour
	for _, kind := range []nostr.Kind{eventkind.MonitoredRepositories, eventkind.Deletion, eventkind.RepositoryAnnouncement} {
		if !eventTimestampPlausibleForKind(kind, old, 10*time.Minute, maxAge) {
			t.Errorf("kind %d older than max age must still be accepted (replacement ordering decides)", kind)
		}
	}
	if eventTimestampPlausibleForKind(eventkind.Patch, old, 10*time.Minute, maxAge) {
		t.Error("old patch must be rejected by the age bound")
	}
	future := nostr.Timestamp(time.Now().Add(time.Hour).Unix())
	if eventTimestampPlausibleForKind(eventkind.RepositoryAnnouncement, future, 10*time.Minute, maxAge) {
		t.Error("future-dated announcement must still be rejected")
	}
}
