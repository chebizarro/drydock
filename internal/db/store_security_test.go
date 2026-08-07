package db

import (
	"context"
	"testing"
	"time"
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
		"running-repo":   "pending",
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
