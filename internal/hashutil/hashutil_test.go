package hashutil

import (
	"strings"
	"testing"
)

func TestSHA256Hex(t *testing.T) {
	// Known SHA-256 of the empty input.
	if got := SHA256Hex(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("SHA256Hex(nil) = %q", got)
	}
	if SHA256Hex([]byte("a")) == SHA256Hex([]byte("b")) {
		t.Fatal("distinct inputs hashed equal")
	}
}

func TestIsProbablyText(t *testing.T) {
	if !IsProbablyText([]byte("hello world")) {
		t.Error("expected text")
	}
	if IsProbablyText([]byte{0x00, 0x01, 0x02}) {
		t.Error("expected binary")
	}
	if !IsProbablyText(nil) {
		t.Error("empty input should be treated as text")
	}

	// Drift regression (DRYDOCK-aubv): a file whose first NUL byte falls past
	// byte 512 is treated as text — the prefix-sampling heuristic. Before
	// consolidation one copy scanned the whole buffer and disagreed.
	data := append([]byte(strings.Repeat("x", 600)), 0x00)
	if !IsProbablyText(data) {
		t.Error("NUL past byte 512 must not flip the prefix heuristic to binary")
	}
}

func TestTruncateForLog(t *testing.T) {
	if got := TruncateForLog("short", 10); got != "short" {
		t.Fatalf("TruncateForLog short = %q", got)
	}
	if got := TruncateForLog("0123456789", 4); got != "0123..." {
		t.Fatalf("TruncateForLog trimmed = %q", got)
	}
}
