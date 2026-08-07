package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"drydock/internal/db"
	"drydock/internal/nostrprobe"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip59"
)

func TestPublishSecurityAuditEventsKeepDetailPrivate(t *testing.T) {
	ctx := context.Background()
	sender := keyer.NewPlainKeySigner(nostr.Generate())
	recipient := keyer.NewPlainKeySigner(nostr.Generate())
	recipientPK, err := recipient.GetPublicKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := nostr.GetPublicKey(nostr.Generate())
	announcement := nostr.Event{
		ID:        nostr.MustIDFromHex("1111111111111111111111111111111111111111111111111111111111111111"),
		PubKey:    owner,
		Kind:      30617,
		CreatedAt: 1700000000,
		Tags:      nostr.Tags{{"d", "widgets"}},
	}
	relays := &fakeRelayPublisher{}
	store, auditID := newSecurityAuditTestStore(t, ctx)
	svc := New(Config{DefaultRelays: []string{"wss://relay.test"}}, store, sender, relays, testLogger())

	in := PublishSecurityAuditInput{
		AuditID:      auditID,
		Complete:     true,
		Coverage:     SecurityAuditCoverage{ScanOperationsScanned: 8, ScanOperationsSkipped: 1},
		Announcement: announcement,
		Ref:          "refs/heads/main",
		Commit:       "abc123",
		Summary:      "No critical issues found.",
		Depth:        "deep",
		Verified:     true,
		Requester:    recipientPK,
		GeneratedAt:  time.Unix(1737000000, 0),
		Tools: []AuditTool{
			{Name: "drydock", Version: "abc123"},
			{Name: "sec70b", Version: "served-model"},
		},
		ProbeEvidence: []nostrprobe.SecurityEvidence{{
			RuleID: nostrprobe.RuleRelaySignature, Status: nostrprobe.StatusFail, Target: "wss://private-target.example",
		}},
		Findings: []SecurityAuditFinding{{
			RuleID:      "SQL-INJECTION",
			CWE:         "CWE-89",
			Severity:    "high",
			Message:     "SQL injection",
			File:        "internal/db/query.go",
			Line:        42,
			Evidence:    "attacker-controlled id reaches db.Query",
			Taint:       "request.id -> query",
			Remediation: "Use a parameterized query.",
			Confidence:  0.98,
		}},
	}
	result, err := svc.PublishSecurityAudit(ctx, in)
	if err != nil {
		t.Fatalf("PublishSecurityAudit() error = %v", err)
	}
	if len(relays.calls) != 3 {
		t.Fatalf("published events = %d, want 3", len(relays.calls))
	}

	var report, wrapped, fallback nostr.Event
	for _, call := range relays.calls {
		if len(call.relays) != 1 || call.relays[0] != "wss://relay.test" {
			t.Fatalf("unexpected relays: %#v", call.relays)
		}
		switch call.event.Kind {
		case KindSecurityAuditReport:
			report = call.event
		case nostr.KindGiftWrap:
			wrapped = call.event
		case nostr.KindComment:
			fallback = call.event
		}
	}
	if report.ID.Hex() != result.ReportEventID || wrapped.ID.Hex() != result.DetailEventID || fallback.ID.Hex() != result.FallbackEventID {
		t.Fatal("result event IDs do not match published events")
	}
	if strings.Contains(report.Content, "query.go") || strings.Contains(report.Content, "attacker-controlled") || strings.Contains(report.Content, "request.id") || strings.Contains(report.Content, "private-target.example") {
		t.Fatalf("public report leaked finding detail: %s", report.Content)
	}
	if strings.Contains(fallback.Content, "query.go") || strings.Contains(fallback.Content, "attacker-controlled") || strings.Contains(fallback.Content, "request.id") || strings.Contains(fallback.Content, "private-target.example") {
		t.Fatalf("fallback comment leaked finding detail: %s", fallback.Content)
	}

	var public SecurityAuditPublicContent
	if err := json.Unmarshal([]byte(report.Content), &public); err != nil {
		t.Fatalf("decode public report: %v", err)
	}
	if public.SchemaVersion != 2 || public.Ref != "abc123" || public.DetailDelivery != "nip59" || public.Counts.High != 1 || public.ProbeCounts == nil || public.ProbeCounts.Fail != 1 {
		t.Fatalf("unexpected public content: %#v", public)
	}
	if public.Complete || public.Coverage.ScanOperationsScanned != 8 || public.Coverage.ScanOperationsSkipped != 1 {
		t.Fatalf("unexpected public coverage: %#v", public)
	}
	if len(public.CWETop) != 1 || public.CWETop[0] != "CWE-89" {
		t.Fatalf("unexpected cwe_top: %#v", public.CWETop)
	}
	if public.ReportDigest != result.ReportDigest || auditTagValue(report.Tags, "report") != result.ReportDigest {
		t.Fatal("public report digest does not match result")
	}
	assertAuditTag(t, report.Tags, "d", "widgets:refs/heads/main")
	assertAuditTag(t, report.Tags, "a", "30617:"+owner.Hex()+":widgets")
	assertAuditTag(t, report.Tags, "r", "abc123")
	assertAuditTag(t, report.Tags, "t", "security-audit")
	assertAuditTag(t, report.Tags, "severity", "high", "1")
	assertAuditTag(t, report.Tags, "tool", "drydock", "abc123")
	assertAuditTag(t, fallback.Tags, "A", "30617:"+owner.Hex()+":widgets")

	if len(wrapped.Tags) != 1 || len(wrapped.Tags[0]) != 2 || wrapped.Tags[0][0] != "p" || wrapped.Tags[0][1] != recipientPK.Hex() {
		t.Fatalf("gift wrap leaked non-routing tags: %#v", wrapped.Tags)
	}
	rumor, err := nip59.GiftUnwrap(wrapped, func(other nostr.PubKey, ciphertext string) (string, error) {
		return recipient.Decrypt(ctx, ciphertext, other)
	})
	if err != nil {
		t.Fatalf("GiftUnwrap() error = %v", err)
	}
	if rumor.Kind != kindPrivateDirectMessage || !rumor.Tags.ContainsAny("e", []string{report.ID.Hex()}) {
		t.Fatalf("unexpected detail rumor: %#v", rumor)
	}
	var detail securityAuditDetail
	if err := json.Unmarshal([]byte(rumor.Content), &detail); err != nil {
		t.Fatalf("decode private detail: %v", err)
	}
	if len(detail.Findings) != 1 || detail.Findings[0].File != "internal/db/query.go" || detail.Findings[0].Taint != "request.id -> query" {
		t.Fatalf("private detail missing sensitive finding fields: %#v", detail)
	}
	if len(detail.ProbeEvidence) != 1 || detail.ProbeEvidence[0].Target != "wss://private-target.example" {
		t.Fatalf("private detail missing probe target: %#v", detail.ProbeEvidence)
	}
	if detail.AuditID != auditID || detail.SARIFRef != securityAuditSARIFRef(auditID) || detail.Complete || detail.Coverage.ScanOperationsScanned != 8 {
		t.Fatalf("private detail missing durable artifact reference or coverage: %#v", detail)
	}
	if got := sha256Hex([]byte(rumor.Content)); got != result.ReportDigest {
		t.Fatalf("detail digest = %s, want %s", got, result.ReportDigest)
	}
	if got := sha256Hex(result.SARIF); got != result.SARIFSHA256 {
		t.Fatalf("SARIF digest = %s, want %s", got, result.SARIFSHA256)
	}
	storedSARIF, storedHash, err := store.SecurityAuditSARIF(ctx, auditID)
	if err != nil {
		t.Fatalf("SecurityAuditSARIF() error = %v", err)
	}
	if string(storedSARIF) != string(result.SARIF) || storedHash != result.SARIFSHA256 {
		t.Fatal("durable SARIF does not match publication result")
	}
	var state string
	var outboxCount, deliveredCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM security_audits WHERE id=?`, auditID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*), SUM(CASE WHEN delivered_at != 0 THEN 1 ELSE 0 END)
		FROM security_audit_publication_outbox WHERE audit_id=?`, auditID).Scan(&outboxCount, &deliveredCount); err != nil {
		t.Fatal(err)
	}
	if state != "published" || outboxCount != 3 || deliveredCount != 3 {
		t.Fatalf("state=%q outbox=%d delivered=%d", state, outboxCount, deliveredCount)
	}
}

type failOnceSecurityAuditPublisher struct {
	calls  []publishCall
	failAt int
	failed bool
}

func (f *failOnceSecurityAuditPublisher) Publish(_ context.Context, relays []string, event nostr.Event) error {
	f.calls = append(f.calls, publishCall{relays: append([]string(nil), relays...), event: event})
	if !f.failed && len(f.calls) == f.failAt {
		f.failed = true
		return errors.New("relay unavailable")
	}
	return nil
}

func TestSecurityAuditPublicationResumesUndeliveredEvents(t *testing.T) {
	ctx := context.Background()
	store, auditID := newSecurityAuditTestStore(t, ctx)
	sender := keyer.NewPlainKeySigner(nostr.Generate())
	recipient := keyer.NewPlainKeySigner(nostr.Generate())
	requester, err := recipient.GetPublicKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	announcement := nostr.Event{
		ID:     nostr.MustIDFromHex("2222222222222222222222222222222222222222222222222222222222222222"),
		PubKey: nostr.GetPublicKey(nostr.Generate()), Kind: 30617,
		CreatedAt: 1700000000, Tags: nostr.Tags{{"d", "widgets"}},
	}
	firstRelay := &failOnceSecurityAuditPublisher{failAt: 2}
	svc := New(Config{DefaultRelays: []string{"wss://relay.test"}}, store, sender, firstRelay, testLogger())
	result, err := svc.PublishSecurityAudit(ctx, PublishSecurityAuditInput{
		AuditID: auditID, Announcement: announcement, Commit: "abc123", Depth: "standard",
		Complete: true, Verified: true, Coverage: SecurityAuditCoverage{ScanOperationsScanned: 4},
		Requester: requester,
	})
	if err == nil {
		t.Fatal("PublishSecurityAudit succeeded despite second-event relay failure")
	}
	if result.ReportEventID == "" || result.DetailEventID == "" || result.FallbackEventID == "" {
		t.Fatalf("durable publication IDs were not returned: %+v", result)
	}
	var total, delivered int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*),
		SUM(CASE WHEN delivered_at != 0 THEN 1 ELSE 0 END)
		FROM security_audit_publication_outbox WHERE audit_id=?`, auditID).Scan(&total, &delivered); err != nil {
		t.Fatal(err)
	}
	if total != 3 || delivered != 1 {
		t.Fatalf("outbox total=%d delivered=%d, want 3/1", total, delivered)
	}

	reset, err := store.ResetStuckAudits(ctx)
	if err != nil || reset != 1 {
		t.Fatalf("ResetStuckAudits() = %d, %v", reset, err)
	}
	resumedRelay := &fakeRelayPublisher{}
	completed, err := ResumeSecurityAuditPublications(ctx, store, resumedRelay)
	if err != nil {
		t.Fatalf("ResumeSecurityAuditPublications: %v", err)
	}
	if completed != 1 || len(resumedRelay.calls) != 2 {
		t.Fatalf("completed=%d resumed calls=%d, want 1/2", completed, len(resumedRelay.calls))
	}
	for _, call := range resumedRelay.calls {
		if call.event.ID.Hex() == result.ReportEventID {
			t.Fatal("already delivered report was published again")
		}
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT state FROM security_audits WHERE id=?`, auditID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "published" {
		t.Fatalf("state=%q, want published", state)
	}
}

func newSecurityAuditTestStore(t *testing.T, ctx context.Context) (*db.Store, int64) {
	t.Helper()
	store, err := db.Open(ctx, filepath.Join(t.TempDir(), "security-audit.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	auditID, err := store.CreateSecurityAudit(ctx, "repo", "main", "standard", "requester")
	if err != nil {
		t.Fatalf("CreateSecurityAudit: %v", err)
	}
	if err := store.StartSecurityAudit(ctx, auditID); err != nil {
		t.Fatalf("StartSecurityAudit: %v", err)
	}
	return store, auditID
}

func TestGenerateSARIFGolden(t *testing.T) {
	got, err := GenerateSARIF([]SecurityAuditFinding{
		{
			RuleID:      "SQL-INJECTION",
			CWE:         "CWE-89",
			Severity:    "critical",
			Message:     "Untrusted input reaches SQL execution.",
			File:        "internal/db/query.go",
			Line:        42,
			EndLine:     44,
			Evidence:    "db.Query builds SQL with user input",
			Taint:       "request.id -> fmt.Sprintf -> db.Query",
			Remediation: "Use placeholders.",
			Confidence:  0.99,
		},
		{
			CWE:      "CWE-78",
			Severity: "medium",
			Message:  "Command arguments are not constrained.",
			File:     "internal/run/exec.go",
			Line:     17,
			Evidence: "request command reaches exec.Command",
		},
	}, []AuditTool{{Name: "drydock", Version: "test-ref"}})
	if err != nil {
		t.Fatalf("GenerateSARIF() error = %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "security-audit.sarif.golden"))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("SARIF mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func assertAuditTag(t *testing.T, tags nostr.Tags, want ...string) {
	t.Helper()
	for _, tag := range tags {
		if len(tag) != len(want) {
			continue
		}
		match := true
		for i := range want {
			if tag[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return
		}
	}
	t.Fatalf("missing tag %#v in %#v", want, tags)
}

func auditTagValue(tags nostr.Tags, name string) string {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == name {
			return tag[1]
		}
	}
	return ""
}
