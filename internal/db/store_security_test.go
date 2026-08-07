package db

import (
	"bytes"
	"context"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

func TestResetStuckAudits(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)

	now := time.Now().Unix()
	for _, audit := range []struct {
		repoID string
		state  string
	}{
		{repoID: "running-repo", state: "running"},
		{repoID: "pending-repo", state: "pending"},
		{repoID: "published-repo", state: "published"},
		{repoID: "failed-repo", state: "failed"},
	} {
		_, err := store.db.ExecContext(ctx,
			`INSERT INTO security_audits(
				repo_id, ref, depth, requested_by, state, created_at, updated_at
			) VALUES (?, 'main', 'standard', 'requester', ?, ?, ?)`,
			audit.repoID, audit.state, now-60, now-60)
		if err != nil {
			t.Fatalf("insert %s audit: %v", audit.state, err)
		}
	}

	reset, err := store.ResetStuckAudits(ctx)
	if err != nil {
		t.Fatalf("ResetStuckAudits: %v", err)
	}
	if reset != 1 {
		t.Fatalf("ResetStuckAudits reset %d audits, want 1", reset)
	}

	for repoID, want := range map[string]string{
		"running-repo":   "failed",
		"pending-repo":   "pending",
		"published-repo": "published",
		"failed-repo":    "failed",
	} {
		var got string
		if err := store.db.QueryRowContext(ctx,
			`SELECT state FROM security_audits WHERE repo_id=?`, repoID,
		).Scan(&got); err != nil {
			t.Fatalf("read %s audit: %v", repoID, err)
		}
		if got != want {
			t.Errorf("%s state = %q, want %q", repoID, got, want)
		}
	}
}

func TestSecurityAuditPublicationSetPersistsCoverageAndSARIF(t *testing.T) {
	ctx := context.Background()
	store := mustOpenStore(t, ctx)
	auditID, err := store.CreateSecurityAudit(ctx, "repo", "main", "standard", "requester")
	if err != nil {
		t.Fatalf("CreateSecurityAudit: %v", err)
	}
	if err := store.StartSecurityAudit(ctx, auditID); err != nil {
		t.Fatalf("StartSecurityAudit: %v", err)
	}
	coverage := SecurityAuditCoverage{ScanOperationsScanned: 12, ScanOperationsSkipped: 2, ScanOperationsErrored: 0, UnitsDropped: 3}
	if err := store.UpdateSecurityAuditCoverage(ctx, auditID, coverage); err != nil {
		t.Fatalf("UpdateSecurityAuditCoverage: %v", err)
	}

	sk := nostr.Generate()
	publications := make([]SecurityAuditPublication, 0, 3)
	for _, eventType := range []string{
		SecurityAuditPublicationReport,
		SecurityAuditPublicationDetail,
		SecurityAuditPublicationFallback,
	} {
		event := nostr.Event{Kind: nostr.KindComment, CreatedAt: nostr.Now(), Content: eventType}
		if err := event.Sign(sk); err != nil {
			t.Fatalf("sign %s: %v", eventType, err)
		}
		publications = append(publications, SecurityAuditPublication{
			EventType: eventType, Event: event, Relays: []string{"wss://relay.test"},
		})
	}
	sarif := []byte(`{"version":"2.1.0"}`)
	sarifHash := securityAuditArtifactHash(sarif)
	set, err := store.ReserveSecurityAuditPublicationSet(ctx, auditID, sarifHash, sarif, publications)
	if err != nil {
		t.Fatalf("ReserveSecurityAuditPublicationSet: %v", err)
	}
	if len(set.Publications) != 3 {
		t.Fatalf("publication count = %d, want 3", len(set.Publications))
	}
	firstReportID := set.Publications[0].Event.ID.Hex()

	replacement := append([]SecurityAuditPublication(nil), publications...)
	replacement[0].Event = nostr.Event{Kind: nostr.KindComment, CreatedAt: nostr.Now(), Content: "replacement"}
	if err := replacement[0].Event.Sign(sk); err != nil {
		t.Fatal(err)
	}
	replacementSARIF := []byte("different")
	reused, err := store.ReserveSecurityAuditPublicationSet(ctx, auditID, securityAuditArtifactHash(replacementSARIF), replacementSARIF, replacement)
	if err != nil {
		t.Fatalf("repeat reservation: %v", err)
	}
	if reused.Publications[0].Event.ID.Hex() != firstReportID || !bytes.Equal(reused.SARIF, sarif) || reused.SARIFHash != sarifHash {
		t.Fatalf("reservation was replaced: %+v", reused)
	}

	gotSARIF, gotHash, err := store.SecurityAuditSARIF(ctx, auditID)
	if err != nil {
		t.Fatalf("SecurityAuditSARIF: %v", err)
	}
	if !bytes.Equal(gotSARIF, sarif) || gotHash != sarifHash {
		t.Fatalf("SARIF = %q hash=%q", gotSARIF, gotHash)
	}
	if _, _, err := store.SecurityAuditSARIFForRequester(ctx, auditID, "requester"); err != nil {
		t.Fatalf("authorized SecurityAuditSARIFForRequester: %v", err)
	}
	if _, _, err := store.SecurityAuditSARIFForRequester(ctx, auditID, "other"); err == nil {
		t.Fatal("unauthorized requester retrieved SARIF")
	}
	var scanned, skipped, errored, dropped int
	if err := store.DB().QueryRowContext(ctx, `SELECT scan_operations_scanned, scan_operations_skipped, scan_operations_errored, units_dropped
		FROM security_audits WHERE id=?`, auditID).Scan(&scanned, &skipped, &errored, &dropped); err != nil {
		t.Fatalf("read coverage: %v", err)
	}
	if scanned != 12 || skipped != 2 || errored != 0 || dropped != 3 {
		t.Fatalf("coverage = %d/%d/%d/%d", scanned, skipped, errored, dropped)
	}

	if err := store.MarkSecurityAuditPublicationDelivered(ctx, auditID, SecurityAuditPublicationReport); err != nil {
		t.Fatalf("mark report delivered: %v", err)
	}
	if err := store.CompleteSecurityAuditPublication(ctx, auditID); err == nil {
		t.Fatal("incomplete outbox was marked published")
	}
	for _, eventType := range []string{SecurityAuditPublicationDetail, SecurityAuditPublicationFallback} {
		if err := store.MarkSecurityAuditPublicationDelivered(ctx, auditID, eventType); err != nil {
			t.Fatalf("mark %s delivered: %v", eventType, err)
		}
	}
	if err := store.CompleteSecurityAuditPublication(ctx, auditID); err != nil {
		t.Fatalf("CompleteSecurityAuditPublication: %v", err)
	}
	var state, reportID string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT state, report_event_id FROM security_audits WHERE id=?`, auditID,
	).Scan(&state, &reportID); err != nil {
		t.Fatalf("read completed audit: %v", err)
	}
	if state != "published" || reportID != firstReportID {
		t.Fatalf("state=%q report=%q, want published/%q", state, reportID, firstReportID)
	}
}

func TestSecurityFindingFingerprintSurvivesLineDrift(t *testing.T) {
	first := SecurityFindingFingerprint(
		"./internal\\auth\\handler.go",
		"cwe-89",
		"if userID != \"\" {\n    query(userID)\n}",
	)
	afterLineDrift := SecurityFindingFingerprint(
		"internal/auth/handler.go",
		" CWE-89 ",
		"if  userID != \"\" { query(userID) }",
	)
	if first != afterLineDrift {
		t.Fatalf("equivalent findings have different fingerprints:\n%s\n%s", first, afterLineDrift)
	}

	changedShape := SecurityFindingFingerprint(
		"internal/auth/handler.go",
		"CWE-89",
		"if userID != \"\" { safeQuery(userID) }",
	)
	if first == changedShape {
		t.Fatal("different code shapes have the same fingerprint")
	}
}
