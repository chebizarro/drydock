package profile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"fiatjaf.com/nostr"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeSigner struct{ sk nostr.SecretKey }

func newFakeSigner(t *testing.T) *fakeSigner {
	t.Helper()
	return &fakeSigner{sk: nostr.Generate()}
}

func (f *fakeSigner) GetPublicKey(context.Context) (nostr.PubKey, error) {
	return f.sk.Public(), nil
}

func (f *fakeSigner) SignEvent(_ context.Context, evt *nostr.Event) error {
	return evt.Sign(f.sk)
}

type fakeFetcher struct{ events []nostr.Event }

func (f *fakeFetcher) FetchMany(context.Context, []string, nostr.Filter, nostr.SubscriptionOptions) chan nostr.RelayEvent {
	ch := make(chan nostr.RelayEvent, len(f.events))
	for _, e := range f.events {
		ch <- nostr.RelayEvent{Event: e}
	}
	close(ch)
	return ch
}

type capturePublisher struct{ published []nostr.Event }

func (c *capturePublisher) Publish(_ context.Context, _ []string, event nostr.Event) error {
	c.published = append(c.published, event)
	return nil
}

func baseConfig() Config {
	return Config{
		Enabled:     true,
		Name:        "Drydock",
		About:       "Automated code review for git on Nostr.",
		ReadRelays:  []string{"wss://r"},
		WriteRelays: []string{"wss://w"},
	}
}

func TestEnsureProfilePublishesWhenMissing(t *testing.T) {
	signer := newFakeSigner(t)
	pub := &capturePublisher{}
	svc := New(baseConfig(), signer, &fakeFetcher{}, pub, testLogger())

	if err := svc.EnsureProfile(context.Background()); err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.published))
	}
	evt := pub.published[0]
	if evt.Kind != nostr.KindProfileMetadata {
		t.Fatalf("expected kind 0, got %d", int(evt.Kind))
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(evt.Content), &content); err != nil {
		t.Fatalf("content not JSON: %v", err)
	}
	if content["name"] != "Drydock" || content["about"] != "Automated code review for git on Nostr." {
		t.Fatalf("unexpected content: %v", content)
	}
}

func signedProfile(t *testing.T, signer *fakeSigner, content map[string]any) nostr.Event {
	t.Helper()
	raw, _ := json.Marshal(content)
	evt := nostr.Event{Kind: nostr.KindProfileMetadata, CreatedAt: nostr.Now(), Content: string(raw)}
	if err := signer.SignEvent(context.Background(), &evt); err != nil {
		t.Fatal(err)
	}
	return evt
}

func TestEnsureProfileSkipsWhenUnchanged(t *testing.T) {
	signer := newFakeSigner(t)
	existing := signedProfile(t, signer, map[string]any{
		"name":  "Drydock",
		"about": "Automated code review for git on Nostr.",
		"lud16": "keep@me.example", // unmanaged field
	})
	pub := &capturePublisher{}
	svc := New(baseConfig(), signer, &fakeFetcher{events: []nostr.Event{existing}}, pub, testLogger())

	if err := svc.EnsureProfile(context.Background()); err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	if len(pub.published) != 0 {
		t.Fatalf("expected no publish for unchanged profile, got %d", len(pub.published))
	}
}

func TestEnsureProfileRepublishesOnConfigChangePreservingUnmanagedFields(t *testing.T) {
	signer := newFakeSigner(t)
	existing := signedProfile(t, signer, map[string]any{
		"name":  "Drydock",
		"about": "old about",
		"lud16": "keep@me.example",
	})
	pub := &capturePublisher{}
	svc := New(baseConfig(), signer, &fakeFetcher{events: []nostr.Event{existing}}, pub, testLogger())

	if err := svc.EnsureProfile(context.Background()); err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected republish on changed about, got %d events", len(pub.published))
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(pub.published[0].Content), &content); err != nil {
		t.Fatal(err)
	}
	if content["about"] != "Automated code review for git on Nostr." {
		t.Fatalf("about not updated: %v", content["about"])
	}
	if content["lud16"] != "keep@me.example" {
		t.Fatalf("unmanaged field not preserved: %v", content)
	}
}

func TestEnsureProfileUploadsAssetsToBlossom(t *testing.T) {
	dir := t.TempDir()
	iconPath := filepath.Join(dir, "icon.png")
	bannerPath := filepath.Join(dir, "banner.png")
	if err := os.WriteFile(iconPath, []byte("icon-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bannerPath, []byte("banner-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	iconSHA := sha256.Sum256([]byte("icon-bytes"))
	iconHex := hex.EncodeToString(iconSHA[:])

	var uploads int
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			// Nothing stored yet.
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.Path == "/upload":
			uploads++
			if r.Header.Get("Authorization") != "" {
				sawAuth = true
			}
			body, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(body)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"url": "https://media.example/" + hex.EncodeToString(sum[:]) + ".png",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := baseConfig()
	cfg.IconPath = iconPath
	cfg.BannerPath = bannerPath
	cfg.BlossomServers = []string{srv.URL}

	signer := newFakeSigner(t)
	pub := &capturePublisher{}
	svc := New(cfg, signer, &fakeFetcher{}, pub, testLogger())

	if err := svc.EnsureProfile(context.Background()); err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	if uploads != 2 {
		t.Fatalf("expected 2 uploads (icon + banner), got %d", uploads)
	}
	if !sawAuth {
		t.Fatal("expected Nostr authorization header on uploads")
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(pub.published[0].Content), &content); err != nil {
		t.Fatal(err)
	}
	if content["picture"] != "https://media.example/"+iconHex+".png" {
		t.Fatalf("unexpected picture URL: %v", content["picture"])
	}
	if content["banner"] == nil || content["banner"] == "" {
		t.Fatalf("banner missing: %v", content)
	}
}

func TestEnsureProfileDisabledDoesNothing(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = false
	pub := &capturePublisher{}
	svc := New(cfg, newFakeSigner(t), &fakeFetcher{}, pub, testLogger())
	if err := svc.EnsureProfile(context.Background()); err != nil {
		t.Fatalf("ensure profile: %v", err)
	}
	if len(pub.published) != 0 {
		t.Fatal("disabled profile service must not publish")
	}
}
