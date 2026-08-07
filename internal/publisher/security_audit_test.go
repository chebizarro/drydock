package publisher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	svc := New(Config{DefaultRelays: []string{"wss://relay.test"}}, nil, sender, relays, testLogger())

	in := PublishSecurityAuditInput{
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
	if strings.Contains(report.Content, "query.go") || strings.Contains(report.Content, "attacker-controlled") || strings.Contains(report.Content, "request.id") {
		t.Fatalf("public report leaked finding detail: %s", report.Content)
	}
	if strings.Contains(fallback.Content, "query.go") || strings.Contains(fallback.Content, "attacker-controlled") || strings.Contains(fallback.Content, "request.id") {
		t.Fatalf("fallback comment leaked finding detail: %s", fallback.Content)
	}

	var public SecurityAuditPublicContent
	if err := json.Unmarshal([]byte(report.Content), &public); err != nil {
		t.Fatalf("decode public report: %v", err)
	}
	if public.SchemaVersion != 1 || public.Ref != "abc123" || public.DetailDelivery != "nip59" || public.Counts.High != 1 {
		t.Fatalf("unexpected public content: %#v", public)
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
	if got := sha256Hex([]byte(rumor.Content)); got != result.ReportDigest {
		t.Fatalf("detail digest = %s, want %s", got, result.ReportDigest)
	}
	if got := sha256Hex(result.SARIF); got != result.SARIFSHA256 {
		t.Fatalf("SARIF digest = %s, want %s", got, result.SARIFSHA256)
	}
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
