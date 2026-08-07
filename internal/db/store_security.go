package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

// SecurityFindingFingerprint returns a stable fingerprint for a security
// finding. It intentionally excludes the line number so findings survive line
// drift, and normalizes path separators and code whitespace before hashing.
func SecurityFindingFingerprint(file, cwe, nearbyCode string) string {
	normalizedPath := strings.ReplaceAll(strings.TrimSpace(file), "\\", "/")
	normalizedPath = path.Clean(normalizedPath)
	if normalizedPath == "." {
		normalizedPath = ""
	}
	normalizedPath = strings.TrimPrefix(normalizedPath, "./")

	normalizedCWE := strings.ToUpper(strings.TrimSpace(cwe))
	codeShape := strings.Join(strings.Fields(nearbyCode), " ")

	sum := sha256.Sum256([]byte(normalizedPath + "\x00" + normalizedCWE + "\x00" + codeShape))
	return hex.EncodeToString(sum[:])
}

// ResetStuckAudits transitions audits stuck in "running" (e.g. from a crash)
// back to "pending" so they can be retried.
func (s *Store) ResetStuckAudits(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE security_audits SET state='pending', updated_at=?
		  WHERE state='running'`, now)
	if err != nil {
		return 0, fmt.Errorf("reset stuck audits: %w", err)
	}
	return res.RowsAffected()
}
