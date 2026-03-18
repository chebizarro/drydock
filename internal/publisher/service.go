package publisher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"drydock/internal/db"
	"drydock/internal/reviewengine"

	"fiatjaf.com/nostr"
)

type Signer interface {
	GetPublicKey(ctx context.Context) (nostr.PubKey, error)
	SignEvent(ctx context.Context, evt *nostr.Event) error
}

type RelayPublisher interface {
	Publish(ctx context.Context, relays []string, event nostr.Event) error
}

type Config struct {
	DefaultRelays       []string
	DetailSeverityFloor string
	DefaultTTL          time.Duration
	SupersededTTL       time.Duration
}

type PublishInput struct {
	PatchEventID         string
	RepoID               string
	Summary              string
	Findings             []reviewengine.Finding
	Model                string
	ContextHash          string
	Confidence           float64
	ContextLayersUsed    []string
	ContextLayersDropped []string
	Superseded           bool
}

type Service struct {
	cfg      Config
	store    *db.Store
	signer   Signer
	publish  RelayPublisher
	logger   *slog.Logger
}

func New(cfg Config, store *db.Store, signer Signer, relayPublisher RelayPublisher, logger *slog.Logger) *Service {
	if cfg.DetailSeverityFloor == "" {
		cfg.DetailSeverityFloor = "high"
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 90 * 24 * time.Hour
	}
	if cfg.SupersededTTL == 0 {
		cfg.SupersededTTL = 7 * 24 * time.Hour
	}
	return &Service{cfg: cfg, store: store, signer: signer, publish: relayPublisher, logger: logger}
}

func (s *Service) PublishReview(ctx context.Context, in PublishInput) (string, error) {
	if strings.TrimSpace(in.PatchEventID) == "" {
		return "", errors.New("patch event id is required")
	}
	if strings.TrimSpace(in.RepoID) == "" {
		return "", errors.New("repo id is required")
	}
	patchAuthor, err := s.store.GetPatchAuthorPubKey(ctx, in.PatchEventID)
	if err != nil {
		return "", err
	}
	relays, err := s.resolveRelays(ctx, in.PatchEventID, in.RepoID)
	if err != nil {
		return "", err
	}

	ttl := s.cfg.DefaultTTL
	if in.Superseded {
		ttl = s.cfg.SupersededTTL
	}
	expiresAt := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)

	summaryEvent := nostr.Event{
		Kind:      1622,
		CreatedAt: nostr.Now(),
		Tags: buildCommonTags(
			in.PatchEventID,
			in.RepoID,
			patchAuthor,
			expiresAt,
		),
		Content: buildSummaryContent(in),
	}
	if err := s.signer.SignEvent(ctx, &summaryEvent); err != nil {
		return "", fmt.Errorf("sign summary review event: %w", err)
	}
	if summaryEvent.Kind == 1631 || summaryEvent.Kind == 1632 {
		return "", errors.New("publisher must not emit status events 1631/1632")
	}
	if err := s.publish.Publish(ctx, relays, summaryEvent); err != nil {
		return "", fmt.Errorf("publish summary review event: %w", err)
	}
	if err := s.store.InsertReviewEvent(ctx, summaryEvent, in.PatchEventID, in.RepoID); err != nil {
		return "", err
	}

	for _, finding := range in.Findings {
		if !isAtOrAboveSeverity(finding.Severity, s.cfg.DetailSeverityFloor) {
			continue
		}
		detail := nostr.Event{
			Kind:      1622,
			CreatedAt: nostr.Now(),
			Tags: buildCommonTags(
				in.PatchEventID,
				in.RepoID,
				patchAuthor,
				expiresAt,
			),
			Content: buildFindingContent(finding, in),
		}
		if err := s.signer.SignEvent(ctx, &detail); err != nil {
			s.logger.Error("failed to sign detail finding event", "patch_event_id", in.PatchEventID, "error", err)
			continue
		}
		if err := s.publish.Publish(ctx, relays, detail); err != nil {
			s.logger.Error("failed to publish detail finding event", "patch_event_id", in.PatchEventID, "error", err)
			continue
		}
		_ = s.store.InsertReviewEvent(ctx, detail, in.PatchEventID, in.RepoID)
	}

	if err := s.store.MarkReviewPublished(ctx, in.PatchEventID, in.RepoID, summaryEvent.ID.Hex()); err != nil {
		return "", err
	}
	return summaryEvent.ID.Hex(), nil
}

func (s *Service) resolveRelays(ctx context.Context, patchEventID, repoID string) ([]string, error) {
	relays, err := s.store.GetPublishRelays(ctx, patchEventID, repoID)
	if err != nil {
		return nil, err
	}
	if len(relays) == 0 {
		relays = append([]string(nil), s.cfg.DefaultRelays...)
	}
	relays = dedupeNonEmpty(relays)
	if len(relays) == 0 {
		return nil, errors.New("no relays available for publishing")
	}
	return relays, nil
}

func buildCommonTags(patchEventID, repoID, patchAuthorPubkey, expiration string) nostr.Tags {
	return nostr.Tags{
		{"e", patchEventID, "", "root"},
		{"e", patchEventID, "", "reply"},
		{"a", "30617:" + repoID},
		{"p", patchAuthorPubkey},
		{"expiration", expiration},
	}
}

func dedupeNonEmpty(items []string) []string {
	set := map[string]struct{}{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			set[item] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	slices.Sort(out)
	return out
}

func isAtOrAboveSeverity(severity, threshold string) bool {
	order := map[string]int{
		"info": 1, "low": 2, "medium": 3, "high": 4, "critical": 5,
	}
	return order[strings.ToLower(severity)] >= order[strings.ToLower(threshold)]
}

func footer(in PublishInput) string {
	used := strings.Join(in.ContextLayersUsed, ", ")
	dropped := strings.Join(in.ContextLayersDropped, ", ")
	// mandatory: keep field present even when empty
	return fmt.Sprintf(
		"\n\n---\nmodel: %s\ncontext-hash: %s\npatch-event-id: %s\nrepo-id: %s\nreview-mode: automated\nconfidence: %.2f\ncontext-layers-used: %s\ncontext-layers-dropped: %s\n",
		in.Model, in.ContextHash, in.PatchEventID, in.RepoID, in.Confidence, used, dropped,
	)
}

func buildSummaryContent(in PublishInput) string {
	var b strings.Builder
	b.WriteString("## Automated Review Summary\n\n")
	b.WriteString(strings.TrimSpace(in.Summary))
	if len(in.Findings) > 0 {
		b.WriteString("\n\n### Findings\n")
		for _, f := range in.Findings {
			b.WriteString(fmt.Sprintf("- **%s/%s** `%s:%d` — %s\n", f.Severity, f.Category, f.File, f.Line, strings.TrimSpace(f.Explanation)))
		}
	}
	b.WriteString(footer(in))
	return b.String()
}

func buildFindingContent(f reviewengine.Finding, in PublishInput) string {
	content := fmt.Sprintf(
		"## Automated Review Finding\n\n**Severity:** %s\n**Category:** %s\n**Location:** `%s:%d`\n\n**Evidence:** %s\n\n**Explanation:** %s\n\n**Suggestion:** %s",
		f.Severity, f.Category, f.File, f.Line, f.Evidence, f.Explanation, f.Suggestion,
	)
	return content + footer(in)
}

