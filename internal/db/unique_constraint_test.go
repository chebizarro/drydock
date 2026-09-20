package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// TestIsSQLiteUniqueConstraintUsesDriverCode pins the double-spend gate to the
// driver's extended result code rather than its message text. The predicate
// gates ErrTokenHashAlreadyReserved, so a driver message reformat must not be
// able to turn a replayed Cashu token into a generic database error.
func TestIsSQLiteUniqueConstraintUsesDriverCode(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`CREATE TABLE probe (token_hash TEXT UNIQUE, id TEXT PRIMARY KEY, note TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO probe (token_hash, id, note) VALUES ('hash-a', 'id-1', 'first')`); err != nil {
		t.Fatal(err)
	}

	t.Run("unique index violation", func(t *testing.T) {
		_, err := database.ExecContext(ctx,
			`INSERT INTO probe (token_hash, id, note) VALUES ('hash-a', 'id-2', 'replay')`)
		if !isSQLiteUniqueConstraint(err) {
			t.Fatalf("isSQLiteUniqueConstraint(%v) = false, want true", err)
		}
	})

	t.Run("primary key violation", func(t *testing.T) {
		_, err := database.ExecContext(ctx,
			`INSERT INTO probe (token_hash, id, note) VALUES ('hash-b', 'id-1', 'replay')`)
		if !isSQLiteUniqueConstraint(err) {
			t.Fatalf("isSQLiteUniqueConstraint(%v) = false, want true", err)
		}
	})

	t.Run("survives error wrapping", func(t *testing.T) {
		_, err := database.ExecContext(ctx,
			`INSERT INTO probe (token_hash, id, note) VALUES ('hash-a', 'id-3', 'replay')`)
		wrapped := fmt.Errorf("reserve payment token: %w", err)
		if !isSQLiteUniqueConstraint(wrapped) {
			t.Fatalf("isSQLiteUniqueConstraint on wrapped error = false, want true")
		}
	})

	t.Run("other constraint violations are not uniqueness", func(t *testing.T) {
		_, err := database.ExecContext(ctx,
			`INSERT INTO probe (token_hash, id, note) VALUES ('hash-c', 'id-4', NULL)`)
		if err == nil {
			t.Fatal("expected NOT NULL violation")
		}
		if isSQLiteUniqueConstraint(err) {
			t.Fatalf("NOT NULL violation misclassified as uniqueness: %v", err)
		}
	})

	t.Run("nil and unrelated errors", func(t *testing.T) {
		if isSQLiteUniqueConstraint(nil) {
			t.Fatal("nil must not be a uniqueness violation")
		}
		if isSQLiteUniqueConstraint(errors.New("UNIQUE constraint failed: probe.token_hash")) {
			t.Fatal("a plain error that merely contains the driver's message text must not match")
		}
	})
}
