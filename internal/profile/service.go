// Package profile maintains drydock's NIP-01 kind 0 profile metadata.
//
// On startup the service compares the profile currently visible on the
// configured relays with the desired profile derived from configuration. When
// no kind 0 exists, or the configuration has changed since the last publish,
// a fresh kind 0 is signed and published. Fields managed by drydock (name,
// about, website, picture, banner) are overwritten; any other fields present
// in the existing profile (e.g. lud16) are preserved.
//
// The icon and banner images are pushed to a Blossom media server (BUD-01/
// BUD-02) when one is configured, and the resulting content-addressed URLs
// are used as the profile picture and banner. Explicitly configured URLs
// take precedence over uploads.
package profile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fiatjaf.com/nostr"
)

// blossomAuthKind is the Blossom authorization event kind (BUD-01).
const blossomAuthKind = nostr.Kind(24242)

type Signer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
}

type RelayPublisher interface {
	Publish(ctx context.Context, relays []string, event nostr.Event) error
}

// Fetcher retrieves events from relays until EOSE. *nostr.Pool satisfies it.
type Fetcher interface {
	FetchMany(ctx context.Context, urls []string, filter nostr.Filter, opts nostr.SubscriptionOptions) chan nostr.RelayEvent
}

type Config struct {
	Enabled bool
	Name    string
	About   string
	Website string
	// PictureURL / BannerURL, when set, are used verbatim and no upload is
	// attempted for that image.
	PictureURL string
	BannerURL  string
	// IconPath / BannerPath are local files pushed to a Blossom server when
	// no explicit URL is configured.
	IconPath   string
	BannerPath string
	// BlossomServers lists media servers to try, in order, for image uploads.
	BlossomServers []string
	ReadRelays     []string
	WriteRelays    []string
}

type Service struct {
	cfg      Config
	signer   Signer
	fetcher  Fetcher
	relayPub RelayPublisher
	http     *http.Client
	logger   *slog.Logger
}

func New(cfg Config, signer Signer, fetcher Fetcher, relayPub RelayPublisher, logger *slog.Logger) *Service {
	return &Service{
		cfg:      cfg,
		signer:   signer,
		fetcher:  fetcher,
		relayPub: relayPub,
		http:     &http.Client{Timeout: 30 * time.Second},
		logger:   logger,
	}
}

// EnsureProfile publishes or refreshes the kind 0 profile. It is safe to call
// on every startup: nothing is published when the visible profile already
// matches the configured metadata.
func (s *Service) EnsureProfile(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	pub, err := s.signer.GetPublicKey(ctx)
	if err != nil {
		return fmt.Errorf("get signer public key: %w", err)
	}

	current := s.fetchCurrentProfile(ctx, pub)

	merged, changed := s.mergeDesired(ctx, current)
	if !changed {
		s.logger.Info("kind 0 profile is up to date")
		return nil
	}

	content, err := canonicalJSON(merged)
	if err != nil {
		return fmt.Errorf("encode profile content: %w", err)
	}
	evt := nostr.Event{
		Kind:      nostr.KindProfileMetadata,
		CreatedAt: nostr.Now(),
		Content:   content,
	}
	if err := s.signer.SignEvent(ctx, &evt); err != nil {
		return fmt.Errorf("sign kind 0 profile: %w", err)
	}
	if err := s.relayPub.Publish(ctx, s.cfg.WriteRelays, evt); err != nil {
		return fmt.Errorf("publish kind 0 profile: %w", err)
	}
	s.logger.Info("published kind 0 profile",
		"event_id", evt.ID.Hex(),
		"name", s.cfg.Name,
		"had_previous", current != nil)
	return nil
}

// fetchCurrentProfile returns the newest kind 0 content for the pubkey
// visible on the read relays, or nil when none is found (or fetch fails).
func (s *Service) fetchCurrentProfile(ctx context.Context, pub nostr.PubKey) map[string]any {
	if s.fetcher == nil || len(s.cfg.ReadRelays) == 0 {
		return nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var newest *nostr.Event
	events := s.fetcher.FetchMany(fetchCtx, s.cfg.ReadRelays, nostr.Filter{
		Kinds:   []nostr.Kind{nostr.KindProfileMetadata},
		Authors: []nostr.PubKey{pub},
		Limit:   1,
	}, nostr.SubscriptionOptions{})
	for re := range events {
		evt := re.Event
		if newest == nil || evt.CreatedAt > newest.CreatedAt {
			e := evt
			newest = &e
		}
	}
	if newest == nil {
		return nil
	}
	var content map[string]any
	if err := json.Unmarshal([]byte(newest.Content), &content); err != nil {
		s.logger.Warn("existing kind 0 content is not valid JSON, replacing it", "error", err)
		return map[string]any{}
	}
	return content
}

// mergeDesired overlays the managed fields onto the current profile content
// and reports whether the result differs from what is currently published.
// A nil current profile always reports changed.
func (s *Service) mergeDesired(ctx context.Context, current map[string]any) (map[string]any, bool) {
	merged := map[string]any{}
	for k, v := range current {
		merged[k] = v
	}

	setOrDelete := func(key, val string) {
		if strings.TrimSpace(val) == "" {
			delete(merged, key)
			return
		}
		merged[key] = val
	}
	setOrDelete("name", s.cfg.Name)
	setOrDelete("about", s.cfg.About)
	setOrDelete("website", s.cfg.Website)
	setOrDelete("picture", s.resolveImageURL(ctx, s.cfg.PictureURL, s.cfg.IconPath, "icon"))
	setOrDelete("banner", s.resolveImageURL(ctx, s.cfg.BannerURL, s.cfg.BannerPath, "banner"))

	if current == nil {
		return merged, true
	}
	a, errA := canonicalJSON(merged)
	b, errB := canonicalJSON(current)
	if errA != nil || errB != nil {
		return merged, true
	}
	return merged, a != b
}

// resolveImageURL returns the explicit URL when configured, otherwise pushes
// the local asset to the first working Blossom server and returns its URL.
// Returns "" when no image can be resolved (field is then omitted).
func (s *Service) resolveImageURL(ctx context.Context, explicitURL, assetPath, label string) string {
	if strings.TrimSpace(explicitURL) != "" {
		return explicitURL
	}
	if strings.TrimSpace(assetPath) == "" {
		return ""
	}
	data, err := os.ReadFile(assetPath)
	if err != nil {
		s.logger.Warn("profile image asset unavailable, omitting", "label", label, "path", assetPath, "error", err)
		return ""
	}
	if len(s.cfg.BlossomServers) == 0 {
		s.logger.Warn("no Blossom server configured, omitting profile image", "label", label, "path", assetPath)
		return ""
	}
	for _, server := range s.cfg.BlossomServers {
		url, err := s.pushBlossom(ctx, server, data, filepath.Base(assetPath))
		if err != nil {
			s.logger.Warn("Blossom upload failed", "label", label, "server", server, "error", err)
			continue
		}
		return url
	}
	return ""
}

// pushBlossom stores the blob on a Blossom server (BUD-02), returning its
// content-addressed URL. Skips the upload when the server already has the
// blob (BUD-01 HEAD).
func (s *Service) pushBlossom(ctx context.Context, server string, data []byte, name string) (string, error) {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	sum := sha256.Sum256(data)
	shaHex := hex.EncodeToString(sum[:])
	ext := strings.ToLower(filepath.Ext(name))
	blobURL := server + "/" + shaHex + ext

	// Already stored?
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, server+"/"+shaHex, nil)
	if err == nil {
		if res, headErr := s.http.Do(headReq); headErr == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return blobURL, nil
			}
		}
	}

	auth, err := s.blossomAuthHeader(ctx, shaHex, name)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, server+"/upload", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", contentTypeForExt(ext))
	res, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("blossom upload: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var descriptor struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&descriptor); err == nil && strings.TrimSpace(descriptor.URL) != "" {
		return descriptor.URL, nil
	}
	return blobURL, nil
}

// blossomAuthHeader builds the BUD-01 authorization header: a signed kind
// 24242 event carried as base64 JSON.
func (s *Service) blossomAuthHeader(ctx context.Context, shaHex, name string) (string, error) {
	evt := nostr.Event{
		Kind:      blossomAuthKind,
		CreatedAt: nostr.Now(),
		Content:   "Upload " + name,
		Tags: nostr.Tags{
			{"t", "upload"},
			{"x", shaHex},
			{"expiration", fmt.Sprintf("%d", time.Now().Add(10*time.Minute).Unix())},
		},
	}
	if err := s.signer.SignEvent(ctx, &evt); err != nil {
		return "", fmt.Errorf("sign blossom auth event: %w", err)
	}
	return "Nostr " + base64.StdEncoding.EncodeToString([]byte(evt.String())), nil
}

func contentTypeForExt(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

// canonicalJSON encodes a map with sorted keys so content comparison is
// deterministic.
func canonicalJSON(m map[string]any) (string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		vb, err := json.Marshal(m[k])
		if err != nil {
			return "", err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.String(), nil
}
