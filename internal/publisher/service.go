package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"drydock/internal/db"
	"drydock/internal/metrics"
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
	ExcludedFiles        []string
	Superseded           bool
	// DetailSeverityFloor overrides the service-level detail severity floor
	// for this specific review. Empty means use the service default.
	DetailSeverityFloor  string
	// Walkthrough is an optional change description prepended to the summary.
	Walkthrough          reviewengine.WalkthroughOutput
}

type Service struct {
	cfg     Config
	store   *db.Store
	signer  Signer
	publish RelayPublisher
	logger  *slog.Logger
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

	// Idempotency check: if a prior run already published a review event for
	// this patch/repo but crashed before marking it published in the DB,
	// skip re-publishing and just mark it published now.
	if priorEventID, err := s.store.GetReviewEventID(ctx, in.PatchEventID, in.RepoID); err == nil && priorEventID != "" {
		s.logger.Info("review already published by prior run, marking published",
			"patch_event_id", in.PatchEventID,
			"repo_id", in.RepoID,
			"review_event_id", priorEventID,
		)
		if err := s.store.MarkReviewPublished(ctx, in.PatchEventID, in.RepoID, priorEventID); err != nil {
			// If it's already published, that's fine.
			if !errors.Is(err, db.ErrReviewNotFound) {
				return priorEventID, nil
			}
		}
		return priorEventID, nil
	}

	patchRec, err := s.store.GetPatchEvent(ctx, in.PatchEventID)
	if err != nil {
		return "", err
	}
	var patchEvent nostr.Event
	if err := json.Unmarshal([]byte(patchRec.RawEvent), &patchEvent); err != nil {
		return "", fmt.Errorf("decode patch event: %w", err)
	}
	scope, err := deriveCommentScope(patchEvent)
	if err != nil {
		return "", err
	}
	if scope.RootID != patchEvent.ID.Hex() {
		if rootAuthor, err := s.store.GetPatchAuthorPubKey(ctx, scope.RootID); err == nil && strings.TrimSpace(rootAuthor) != "" {
			scope.RootPubKey = rootAuthor
		}
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
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Now(),
		Tags:      buildCommonTags(scope, in.RepoID, expiresAt),
		Content:   buildSummaryContent(in),
	}
	if err := s.signer.SignEvent(ctx, &summaryEvent); err != nil {
		return "", fmt.Errorf("sign summary review event: %w", err)
	}
	if summaryEvent.Kind == 1631 || summaryEvent.Kind == 1632 {
		return "", errors.New("publisher must not emit status events 1631/1632")
	}

	// Record the review event ID in review_log *before* publishing to relays.
	// This creates a crash-recovery breadcrumb: if we crash after relay
	// publish but before MarkReviewPublished, the idempotency check at the
	// top of PublishReview will detect the prior event and skip re-publishing.
	if err := s.store.SetReviewEventID(ctx, in.PatchEventID, in.RepoID, summaryEvent.ID.Hex()); err != nil {
		s.logger.Warn("failed to pre-record review event ID (continuing)",
			"patch_event_id", in.PatchEventID, "error", err)
	}

	metrics.PublishAttempts.Inc()
	if err := s.publish.Publish(ctx, relays, summaryEvent); err != nil {
		metrics.PublishFailures.Inc()
		return "", fmt.Errorf("publish summary review event: %w", err)
	}
	if err := s.store.InsertReviewEvent(ctx, summaryEvent, in.PatchEventID, in.RepoID); err != nil {
		return "", err
	}

	detailFloor := s.cfg.DetailSeverityFloor
	if in.DetailSeverityFloor != "" {
		detailFloor = in.DetailSeverityFloor
	}

	var detailEligible, detailPublished int
	for _, finding := range in.Findings {
		if !reviewengine.IsAtOrAboveSeverity(finding.Severity, detailFloor) {
			continue
		}
		detailEligible++
		detail := nostr.Event{
			Kind:      nostr.KindComment,
			CreatedAt: nostr.Now(),
			Tags:      buildCommonTags(scope, in.RepoID, expiresAt),
			Content:   buildFindingContent(finding, in),
		}
		if err := s.signer.SignEvent(ctx, &detail); err != nil {
			s.logger.Error("failed to sign detail finding event",
				"patch_event_id", in.PatchEventID, "finding_file", finding.File,
				"finding_line", finding.Line, "error", err)
			continue
		}
		if err := s.publish.Publish(ctx, relays, detail); err != nil {
			s.logger.Error("failed to publish detail finding event",
				"patch_event_id", in.PatchEventID, "finding_file", finding.File,
				"finding_line", finding.Line, "error", err)
			continue
		}
		if err := s.store.InsertReviewEvent(ctx, detail, in.PatchEventID, in.RepoID); err != nil {
			s.logger.Error("failed to store detail finding event",
				"patch_event_id", in.PatchEventID, "detail_event_id", detail.ID.Hex(),
				"error", err)
		}
		detailPublished++
	}
	if detailEligible > 0 && detailPublished < detailEligible {
		s.logger.Warn("some detail findings failed to publish",
			"patch_event_id", in.PatchEventID,
			"eligible", detailEligible,
			"published", detailPublished,
			"failed", detailEligible-detailPublished)
	}

	if err := s.store.MarkReviewPublished(ctx, in.PatchEventID, in.RepoID, summaryEvent.ID.Hex()); err != nil {
		return "", err
	}
	metrics.PublishSuccesses.Inc()
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

type commentScope struct {
	RootID       string
	RootKind     nostr.Kind
	RootPubKey   string
	ParentID     string
	ParentKind   nostr.Kind
	ParentPubKey string
}

func buildCommonTags(scope commentScope, repoID, expiration string) nostr.Tags {
	tags := nostr.Tags{
		{"E", scope.RootID, "", scope.RootPubKey},
		{"K", strconv.Itoa(int(scope.RootKind))},
		{"e", scope.ParentID, "", scope.ParentPubKey},
		{"k", strconv.Itoa(int(scope.ParentKind))},
		{"A", "30617:" + repoID},
		{"expiration", expiration},
	}
	if scope.RootPubKey != "" {
		tags = append(tags, nostr.Tag{"P", scope.RootPubKey})
	}
	if scope.ParentPubKey != "" {
		tags = append(tags, nostr.Tag{"p", scope.ParentPubKey})
	}
	return tags
}

func deriveCommentScope(target nostr.Event) (commentScope, error) {
	scope := commentScope{
		RootID:       target.ID.Hex(),
		RootKind:     target.Kind,
		RootPubKey:   target.PubKey.Hex(),
		ParentID:     target.ID.Hex(),
		ParentKind:   target.Kind,
		ParentPubKey: target.PubKey.Hex(),
	}

	if target.Kind == 1619 {
		rootIDTag := target.Tags.Find("E")
		if rootIDTag == nil || len(rootIDTag) < 2 || strings.TrimSpace(rootIDTag[1]) == "" {
			return commentScope{}, errors.New("PR update event missing required E tag")
		}
		scope.RootID = rootIDTag[1]
		scope.RootKind = 1618

		if rootPKTag := target.Tags.Find("P"); rootPKTag != nil && len(rootPKTag) >= 2 {
			scope.RootPubKey = rootPKTag[1]
		}
		return scope, nil
	}

	for _, tag := range target.Tags {
		if len(tag) < 2 || tag[0] != "e" {
			continue
		}
		if len(tag) >= 4 && tag[3] == "root" {
			scope.RootID = tag[1]
			scope.RootKind = target.Kind
			break
		}
	}

	return scope, nil
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

// maxSuggestionBytes is the maximum size for suggested_diff/suggested_code
// blocks to prevent oversized relay events.
const maxSuggestionBytes = 4096

func truncateSuggestion(s string) string {
	if len(s) <= maxSuggestionBytes {
		return s
	}
	return s[:maxSuggestionBytes] + "\n[truncated]"
}

// escapeFenceContent ensures triple backticks inside model output don't
// break fenced code blocks.
func escapeFenceContent(s string) string {
	return strings.ReplaceAll(s, "```", "` ` `")
}

func footer(in PublishInput) string {
	used := strings.Join(in.ContextLayersUsed, ", ")
	dropped := strings.Join(in.ContextLayersDropped, ", ")
	excluded := strings.Join(in.ExcludedFiles, ", ")
	// mandatory: keep field present even when empty
	return fmt.Sprintf(
		"\n\n---\nmodel: %s\ncontext-hash: %s\npatch-event-id: %s\nrepo-id: %s\nreview-mode: automated\nconfidence: %.2f\ncontext-layers-used: %s\ncontext-layers-dropped: %s\nexcluded-files: %s\n",
		in.Model, in.ContextHash, in.PatchEventID, in.RepoID, in.Confidence, used, dropped, excluded,
	)
}

func buildSummaryContent(in PublishInput) string {
	var b strings.Builder

	// Walkthrough section (if available)
	if in.Walkthrough.Walkthrough != "" {
		b.WriteString("Walkthrough\n")
		b.WriteString(plainText(strings.TrimSpace(in.Walkthrough.Walkthrough)))
		if len(in.Walkthrough.FileSummaries) > 0 {
			b.WriteString("\n\nChanged files\n")
			for _, fs := range in.Walkthrough.FileSummaries {
				b.WriteString(fmt.Sprintf("%s: %s\n", fs.File, plainText(strings.TrimSpace(fs.Summary))))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("Automated review summary\n")
	b.WriteString(plainText(strings.TrimSpace(in.Summary)))
	if len(in.Findings) > 0 {
		b.WriteString("\n\nFindings\n")
		for _, f := range in.Findings {
			line := fmt.Sprintf("%s | %s | %s:%d | %s", f.Severity, f.Category, f.File, f.Line, plainText(strings.TrimSpace(f.Explanation)))
			if f.HasSuggestion() {
				line += " | fix available"
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(footer(in))
	return b.String()
}

func buildFindingContent(f reviewengine.Finding, in PublishInput) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		"Automated review finding\nSeverity: %s\nCategory: %s\nLocation: %s:%d\nEvidence: %s\nExplanation: %s\nSuggestion: %s",
		f.Severity, f.Category, f.File, f.Line, plainText(f.Evidence), plainText(f.Explanation), plainText(f.Suggestion),
	))
	if f.SuggestedDiff != "" {
		b.WriteString("\n\nSuggested diff:\n```diff\n")
		b.WriteString(escapeFenceContent(truncateSuggestion(f.SuggestedDiff)))
		b.WriteString("\n```")
	}
	if f.SuggestedCode != "" {
		b.WriteString("\n\nSuggested code:\n```\n")
		b.WriteString(escapeFenceContent(truncateSuggestion(f.SuggestedCode)))
		b.WriteString("\n```")
	}
	b.WriteString(footer(in))
	return b.String()
}

func plainText(v string) string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	v = strings.ReplaceAll(v, "\t", " ")
	replacer := strings.NewReplacer(
		"```", "",
		"**", "",
		"__", "",
		"`", "",
		"#", "",
	)
	v = replacer.Replace(v)
	lines := strings.Split(v, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		lines[i] = line
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}
