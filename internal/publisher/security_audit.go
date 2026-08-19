package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"drydock/internal/db"
	"drydock/internal/nostrprobe"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip59"
)

const (
	KindSecurityAuditReport  nostr.Kind = 30619
	kindPrivateDirectMessage nostr.Kind = 14
)

// GiftWrapSigner is the signing and NIP-44 encryption surface required to
// deliver private security-audit detail.
type GiftWrapSigner interface {
	Signer
	Encrypt(ctx context.Context, plaintext string, recipient nostr.PubKey) (string, error)
}

type AuditTool struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// SecurityAuditFinding is private audit detail. None of these fields are
// serialized into the public kind-30619 event.
type SecurityAuditFinding struct {
	RuleID      string  `json:"rule_id,omitempty"`
	CWE         string  `json:"cwe,omitempty"`
	Severity    string  `json:"severity"`
	Message     string  `json:"message"`
	File        string  `json:"file"`
	Line        int     `json:"line"`
	EndLine     int     `json:"end_line,omitempty"`
	Evidence    string  `json:"evidence"`
	Taint       string  `json:"taint,omitempty"`
	Remediation string  `json:"remediation,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
	Sensitive   bool    `json:"sensitive,omitempty"`
}

type SeverityCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Info     int `json:"info"`
}

type ProbeCounts struct {
	Pass         int `json:"pass"`
	Fail         int `json:"fail"`
	Inconclusive int `json:"inconclusive"`
}

type SecurityAuditCoverage struct {
	ScanOperationsScanned int `json:"scan_operations_scanned"`
	ScanOperationsSkipped int `json:"scan_operations_skipped"`
	ScanOperationsErrored int `json:"scan_operations_errored"`
	UnitsDropped          int `json:"units_dropped"`
}

type SecurityAuditPublicContent struct {
	SchemaVersion  int                   `json:"schema_version"`
	Ref            string                `json:"ref"`
	GeneratedAt    int64                 `json:"generated_at"`
	Depth          string                `json:"depth"`
	Counts         SeverityCounts        `json:"counts"`
	CWETop         []string              `json:"cwe_top"`
	ProbeCounts    *ProbeCounts          `json:"probe_counts,omitempty"`
	Coverage       SecurityAuditCoverage `json:"coverage"`
	Complete       bool                  `json:"complete"`
	Verified       bool                  `json:"verified"`
	ReportDigest   string                `json:"report_digest"`
	DetailDelivery string                `json:"detail_delivery"`
}

type securityAuditDetail struct {
	SchemaVersion int                           `json:"schema_version"`
	AuditID       int64                         `json:"audit_id"`
	RepoAddress   string                        `json:"repo_address"`
	Ref           string                        `json:"ref"`
	GeneratedAt   int64                         `json:"generated_at"`
	Findings      []SecurityAuditFinding        `json:"findings"`
	ProbeEvidence []nostrprobe.SecurityEvidence `json:"probe_evidence,omitempty"`
	Coverage      SecurityAuditCoverage         `json:"coverage"`
	Complete      bool                          `json:"complete"`
	SARIFSHA256   string                        `json:"sarif_sha256"`
	SARIFRef      string                        `json:"sarif_ref"`
}

type PublishSecurityAuditInput struct {
	// Announcement is the kind-30617 repository announcement being audited.
	// Its d tag supplies the repository identifier.
	Announcement nostr.Event
	// Ref scopes the addressable report. Leave empty for the latest audit of
	// the default branch.
	AuditID       int64
	Ref           string
	Commit        string
	Summary       string
	Depth         string
	Complete      bool
	Verified      bool
	Coverage      SecurityAuditCoverage
	Findings      []SecurityAuditFinding
	ProbeEvidence []nostrprobe.SecurityEvidence
	Tools         []AuditTool
	Requester     nostr.PubKey
	Relays        []string
	// GeneratedAt is optional; zero uses the current time.
	GeneratedAt time.Time
}

type PublishSecurityAuditResult struct {
	ReportEventID   string
	DetailEventID   string
	FallbackEventID string
	ReportDigest    string
	SARIF           []byte
	SARIFSHA256     string
}

func (s *Service) PublishSecurityAudit(ctx context.Context, in PublishSecurityAuditInput) (PublishSecurityAuditResult, error) {
	var out PublishSecurityAuditResult
	if s == nil || s.store == nil {
		return out, errors.New("security audit publisher store is required")
	}
	if s.publish == nil {
		return out, errors.New("security audit relay publisher is required")
	}
	repoID, repoAddress, generatedAt, relays, err := s.validateSecurityAuditInput(in)
	if err != nil {
		return out, err
	}
	keyer, ok := s.signer.(GiftWrapSigner)
	if !ok {
		return out, errors.New("security audit publisher signer does not support NIP-44 encryption")
	}

	in.Findings = append([]SecurityAuditFinding(nil), in.Findings...)
	for i := range in.Findings {
		finding := &in.Findings[i]
		sanitizeSensitiveFindingText(finding.Sensitive,
			&finding.Message,
			&finding.Evidence,
			&finding.Taint,
			&finding.Remediation,
		)
	}

	sarif, err := GenerateSARIF(in.Findings, in.Tools)
	if err != nil {
		return out, fmt.Errorf("generate SARIF: %w", err)
	}
	sarifHash := sha256Hex(sarif)
	complete := in.Complete &&
		in.Coverage.ScanOperationsSkipped == 0 &&
		in.Coverage.ScanOperationsErrored == 0 &&
		in.Coverage.UnitsDropped == 0

	detail := securityAuditDetail{
		SchemaVersion: 2,
		AuditID:       in.AuditID,
		RepoAddress:   repoAddress,
		Ref:           auditCommit(in),
		GeneratedAt:   generatedAt.Unix(),
		Findings:      nonNilFindings(in.Findings),
		ProbeEvidence: in.ProbeEvidence,
		Coverage:      in.Coverage,
		Complete:      complete,
		SARIFSHA256:   sarifHash,
		SARIFRef:      securityAuditSARIFRef(in.AuditID),
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return out, fmt.Errorf("marshal security audit detail: %w", err)
	}
	reportDigest := sha256Hex(detailJSON)

	counts := countAuditSeverities(in.Findings)
	publicContent := SecurityAuditPublicContent{
		SchemaVersion:  2,
		Ref:            auditCommit(in),
		GeneratedAt:    generatedAt.Unix(),
		Depth:          strings.TrimSpace(in.Depth),
		Counts:         counts,
		CWETop:         topCWEs(in.Findings, 3),
		ProbeCounts:    countProbeOutcomes(in.ProbeEvidence),
		Coverage:       in.Coverage,
		Complete:       complete,
		Verified:       in.Verified,
		ReportDigest:   reportDigest,
		DetailDelivery: "nip59",
	}
	if publicContent.Depth == "" {
		publicContent.Depth = "deep"
	}
	publicJSON, err := json.Marshal(publicContent)
	if err != nil {
		return out, fmt.Errorf("marshal public security audit summary: %w", err)
	}

	report := nostr.Event{
		Kind:      KindSecurityAuditReport,
		CreatedAt: nostr.Timestamp(generatedAt.Unix()),
		Tags:      buildSecurityAuditReportTags(repoID, repoAddress, in, counts, reportDigest),
		Content:   string(publicJSON),
	}
	if err := s.signer.SignEvent(ctx, &report); err != nil {
		return out, fmt.Errorf("sign security audit report: %w", err)
	}
	detailEvent, err := buildSecurityAuditGiftWrap(ctx, keyer, in.Requester, report, detailJSON)
	if err != nil {
		return out, err
	}
	fallback := nostr.Event{
		Kind:      nostr.KindComment,
		CreatedAt: nostr.Timestamp(generatedAt.Unix()),
		Tags:      buildSecurityAuditFallbackTags(in.Announcement, repoAddress),
		Content:   buildSecurityAuditFallbackContent(in.Summary, publicContent),
	}
	if err := s.signer.SignEvent(ctx, &fallback); err != nil {
		return out, fmt.Errorf("sign security audit fallback comment: %w", err)
	}

	set, err := s.store.ReserveSecurityAuditPublicationSet(ctx, in.AuditID, sarifHash, sarif, []db.SecurityAuditPublication{
		{AuditID: in.AuditID, EventType: db.SecurityAuditPublicationReport, Event: report, Relays: relays},
		{AuditID: in.AuditID, EventType: db.SecurityAuditPublicationDetail, Event: detailEvent, Relays: relays},
		{AuditID: in.AuditID, EventType: db.SecurityAuditPublicationFallback, Event: fallback, Relays: relays},
	})
	if err != nil {
		return out, fmt.Errorf("persist security audit publication set: %w", err)
	}
	out, err = securityAuditResultFromSet(set)
	if err != nil {
		return out, err
	}
	if err := deliverSecurityAuditPublicationSet(ctx, s.store, s.publish, set); err != nil {
		return out, err
	}
	if err := s.store.CompleteSecurityAuditPublication(ctx, in.AuditID); err != nil {
		return out, fmt.Errorf("complete security audit publication: %w", err)
	}
	return out, nil
}

// ResumeSecurityAuditPublications retries durable audit events not previously
// acknowledged by relays, then finalizes each fully delivered audit.
func ResumeSecurityAuditPublications(ctx context.Context, store *db.Store, relayPublisher RelayPublisher) (int, error) {
	if store == nil || relayPublisher == nil {
		return 0, errors.New("security audit publication recovery requires store and relay publisher")
	}
	sets, err := store.ListRecoverableSecurityAuditPublicationSets(ctx)
	if err != nil {
		return 0, err
	}
	var completed int
	var recoveryErrors []error
	for _, set := range sets {
		if len(set.Publications) != 3 {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("security audit %d durable publication set has %d events", set.AuditID, len(set.Publications)))
			continue
		}
		if err := deliverSecurityAuditPublicationSet(ctx, store, relayPublisher, set); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("resume security audit %d publication: %w", set.AuditID, err))
			continue
		}
		if err := store.CompleteSecurityAuditPublication(ctx, set.AuditID); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("complete resumed security audit %d: %w", set.AuditID, err))
			continue
		}
		completed++
	}
	return completed, errors.Join(recoveryErrors...)
}

func deliverSecurityAuditPublicationSet(ctx context.Context, store *db.Store, relayPublisher RelayPublisher, set db.SecurityAuditPublicationSet) error {
	for _, publication := range set.Publications {
		if publication.Delivered {
			continue
		}
		if err := relayPublisher.Publish(ctx, publication.Relays, publication.Event); err != nil {
			return fmt.Errorf("publish security audit %s: %w", publication.EventType, err)
		}
		if err := store.MarkSecurityAuditPublicationDelivered(ctx, set.AuditID, publication.EventType); err != nil {
			return fmt.Errorf("persist security audit %s delivery: %w", publication.EventType, err)
		}
	}
	return nil
}

func securityAuditResultFromSet(set db.SecurityAuditPublicationSet) (PublishSecurityAuditResult, error) {
	out := PublishSecurityAuditResult{
		SARIF: append([]byte(nil), set.SARIF...), SARIFSHA256: set.SARIFHash,
	}
	for _, publication := range set.Publications {
		switch publication.EventType {
		case db.SecurityAuditPublicationReport:
			out.ReportEventID = publication.Event.ID.Hex()
			var content SecurityAuditPublicContent
			if err := json.Unmarshal([]byte(publication.Event.Content), &content); err != nil {
				return out, fmt.Errorf("decode persisted security audit report: %w", err)
			}
			out.ReportDigest = content.ReportDigest
		case db.SecurityAuditPublicationDetail:
			out.DetailEventID = publication.Event.ID.Hex()
		case db.SecurityAuditPublicationFallback:
			out.FallbackEventID = publication.Event.ID.Hex()
		}
	}
	if out.ReportEventID == "" || out.DetailEventID == "" || out.FallbackEventID == "" || out.ReportDigest == "" {
		return out, errors.New("persisted security audit publication set is incomplete")
	}
	return out, nil
}

func securityAuditSARIFRef(auditID int64) string {
	return fmt.Sprintf("contextvm:%s?audit_id=%d", "security/audit/sarif", auditID)
}

func (s *Service) validateSecurityAuditInput(in PublishSecurityAuditInput) (string, string, time.Time, []string, error) {
	if in.AuditID <= 0 {
		return "", "", time.Time{}, nil, errors.New("security audit id is required")
	}
	if in.Coverage.ScanOperationsScanned < 0 || in.Coverage.ScanOperationsSkipped < 0 || in.Coverage.ScanOperationsErrored < 0 || in.Coverage.UnitsDropped < 0 {
		return "", "", time.Time{}, nil, errors.New("security audit coverage cannot be negative")
	}
	if in.Announcement.Kind != 30617 {
		return "", "", time.Time{}, nil, fmt.Errorf("repository announcement kind = %d, want 30617", in.Announcement.Kind)
	}
	repoTag := in.Announcement.Tags.Find("d")
	if repoTag == nil || len(repoTag) < 2 || strings.TrimSpace(repoTag[1]) == "" {
		return "", "", time.Time{}, nil, errors.New("repository announcement missing d tag")
	}
	if in.Announcement.PubKey == nostr.ZeroPK {
		return "", "", time.Time{}, nil, errors.New("repository announcement pubkey is required")
	}
	if in.Announcement.ID == nostr.ZeroID {
		return "", "", time.Time{}, nil, errors.New("repository announcement event id is required")
	}
	if in.Requester == nostr.ZeroPK {
		return "", "", time.Time{}, nil, errors.New("security audit requester pubkey is required")
	}
	if strings.TrimSpace(auditCommit(in)) == "" {
		return "", "", time.Time{}, nil, errors.New("security audit commit is required")
	}
	repoID := strings.TrimSpace(repoTag[1])
	repoAddress := "30617:" + in.Announcement.PubKey.Hex() + ":" + repoID
	generatedAt := in.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now()
	}
	relays := dedupeNonEmpty(in.Relays)
	if len(relays) == 0 {
		relays = dedupeNonEmpty(s.cfg.DefaultRelays)
	}
	if len(relays) == 0 {
		return "", "", time.Time{}, nil, errors.New("no relays available for publishing")
	}
	return repoID, repoAddress, generatedAt, relays, nil
}

func buildSecurityAuditReportTags(repoID, repoAddress string, in PublishSecurityAuditInput, counts SeverityCounts, reportDigest string) nostr.Tags {
	d := repoID
	if ref := strings.TrimSpace(in.Ref); ref != "" {
		d += ":" + ref
	}
	tags := nostr.Tags{
		{"d", d},
		{"a", repoAddress},
		{"r", auditCommit(in)},
		{"t", "security-audit"},
		{"severity", "critical", strconv.Itoa(counts.Critical)},
		{"severity", "high", strconv.Itoa(counts.High)},
		{"severity", "medium", strconv.Itoa(counts.Medium)},
		{"severity", "low", strconv.Itoa(counts.Low)},
		{"severity", "info", strconv.Itoa(counts.Info)},
	}
	for _, tool := range in.Tools {
		if name := strings.TrimSpace(tool.Name); name != "" {
			tags = append(tags, nostr.Tag{"tool", name, strings.TrimSpace(tool.Version)})
		}
	}
	return append(tags,
		nostr.Tag{"report", reportDigest},
		nostr.Tag{"alt", fmt.Sprintf("Security audit report for %s at %s", repoID, auditCommit(in))},
	)
}

func buildSecurityAuditGiftWrap(ctx context.Context, keyer GiftWrapSigner, requester nostr.PubKey, report nostr.Event, detailJSON []byte) (nostr.Event, error) {
	sender, err := keyer.GetPublicKey(ctx)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("get security audit sender pubkey: %w", err)
	}
	rumor := nostr.Event{
		PubKey:    sender,
		Kind:      kindPrivateDirectMessage,
		CreatedAt: report.CreatedAt,
		Content:   string(detailJSON),
		Tags: nostr.Tags{
			{"p", requester.Hex()},
			{"e", report.ID.Hex()},
		},
	}
	rumor.ID = rumor.GetID()
	wrapped, err := nip59.GiftWrap(
		rumor,
		requester,
		func(plaintext string) (string, error) {
			return keyer.Encrypt(ctx, plaintext, requester)
		},
		func(event *nostr.Event) error {
			return keyer.SignEvent(ctx, event)
		},
		nil,
	)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("gift-wrap security audit detail: %w", err)
	}
	return wrapped, nil
}

func buildSecurityAuditFallbackTags(announcement nostr.Event, repoAddress string) nostr.Tags {
	owner := announcement.PubKey.Hex()
	return nostr.Tags{
		{"E", announcement.ID.Hex(), "", owner},
		{"K", "30617"},
		{"P", owner},
		{"e", announcement.ID.Hex(), "", owner},
		{"k", "30617"},
		{"p", owner},
		{"A", repoAddress},
	}
}

func buildSecurityAuditFallbackContent(summary string, public SecurityAuditPublicContent) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = "Security audit completed."
	}
	return fmt.Sprintf(
		"%s\n\nFindings: critical %d, high %d, medium %d, low %d, info %d. Complete: %t. Verified: %t. Coverage: scanned %d, skipped %d, errored %d, dropped %d. Report digest: %s",
		summary,
		public.Counts.Critical,
		public.Counts.High,
		public.Counts.Medium,
		public.Counts.Low,
		public.Counts.Info,
		public.Complete,
		public.Verified,
		public.Coverage.ScanOperationsScanned,
		public.Coverage.ScanOperationsSkipped,
		public.Coverage.ScanOperationsErrored,
		public.Coverage.UnitsDropped,
		public.ReportDigest,
	)
}

func auditCommit(in PublishSecurityAuditInput) string {
	if commit := strings.TrimSpace(in.Commit); commit != "" {
		return commit
	}
	return strings.TrimSpace(in.Ref)
}

func countProbeOutcomes(evidence []nostrprobe.SecurityEvidence) *ProbeCounts {
	if len(evidence) == 0 {
		return nil
	}
	counts := &ProbeCounts{}
	for _, item := range evidence {
		switch item.Status {
		case nostrprobe.StatusPass:
			counts.Pass++
		case nostrprobe.StatusFail:
			counts.Fail++
		default:
			counts.Inconclusive++
		}
	}
	return counts
}

func countAuditSeverities(findings []SecurityAuditFinding) SeverityCounts {
	var counts SeverityCounts
	for _, finding := range findings {
		switch strings.ToLower(strings.TrimSpace(finding.Severity)) {
		case "critical":
			counts.Critical++
		case "high":
			counts.High++
		case "medium":
			counts.Medium++
		case "low":
			counts.Low++
		case "info":
			counts.Info++
		}
	}
	return counts
}

func topCWEs(findings []SecurityAuditFinding, limit int) []string {
	counts := make(map[string]int)
	for _, finding := range findings {
		if cwe := strings.ToUpper(strings.TrimSpace(finding.CWE)); cwe != "" {
			counts[cwe]++
		}
	}
	cwes := make([]string, 0, len(counts))
	for cwe := range counts {
		cwes = append(cwes, cwe)
	}
	slices.SortFunc(cwes, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	if len(cwes) > limit {
		cwes = cwes[:limit]
	}
	if cwes == nil {
		return []string{}
	}
	return cwes
}

func nonNilFindings(findings []SecurityAuditFinding) []SecurityAuditFinding {
	if findings == nil {
		return []SecurityAuditFinding{}
	}
	return findings
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
