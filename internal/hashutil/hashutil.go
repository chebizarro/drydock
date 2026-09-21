// Package hashutil holds the small content helpers that were independently
// re-implemented across the indexing, ingest, embedding and vector-store
// packages. Several copies had drifted (see DRYDOCK-aubv): the text detector in
// particular sampled a 512-byte prefix in some copies and the whole buffer in
// another, so a file whose first NUL byte fell past byte 512 was indexed by one
// path and skipped by another. This package is the single source of truth.
package hashutil

import (
	"crypto/sha256"
	"encoding/hex"
)

// SHA256Hex returns the lowercase hex-encoded SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// IsProbablyText reports whether data looks like text by scanning the first 512
// bytes for a NUL. This is the git-style heuristic: it samples a prefix rather
// than the whole buffer so classification is cheap and bounded. Empty input is
// treated as text.
func IsProbablyText(data []byte) bool {
	check := data
	if len(check) > 512 {
		check = check[:512]
	}
	for _, b := range check {
		if b == 0 {
			return false
		}
	}
	return true
}

// TruncateForLog shortens s to at most n bytes, appending an ellipsis when it
// trims. It is for human-facing log and error-preview strings, not for
// correctness-sensitive data.
func TruncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
