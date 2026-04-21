// Package driftguard implements the convention drift guard described in spec §6.4.
//
// A human reviews a sample of recent meta-reviews and flags any that recommend
// generic best practices conflicting with project-specific conventions. Flagged
// reviews become negative training signals for the meta-reviewer prompt.
package driftguard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"drydock/internal/db"
)

// Service provides the convention drift guard workflow.
type Service struct {
	store  *db.Store
	logger *slog.Logger
}

// NewService creates a drift guard service.
func NewService(store *db.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, logger: logger}
}

// ExportSample writes a human-readable sample of N recent meta-reviews to w.
// Returns the number of reviews exported.
func (s *Service) ExportSample(ctx context.Context, w io.Writer, n int) (int, error) {
	samples, err := s.store.SampleRecentMetaReviews(ctx, n)
	if err != nil {
		return 0, fmt.Errorf("sample meta reviews: %w", err)
	}
	if len(samples) == 0 {
		fmt.Fprintln(w, "No meta-reviews found.")
		return 0, nil
	}

	fmt.Fprintf(w, "=== Convention Drift Guard: %d Meta-Review Samples ===\n\n", len(samples))

	for i, sample := range samples {
		fmt.Fprintf(w, "--- Review #%d (ID: %d) ---\n", i+1, sample.ID)
		fmt.Fprintf(w, "Patch Event: %s\n", sample.PatchEventID)
		fmt.Fprintf(w, "Repo:        %s\n", sample.RepoID)
		fmt.Fprintf(w, "Gate Reason: %s\n", sample.GateReason)
		fmt.Fprintf(w, "Date:        %s\n", time.Unix(sample.CreatedAt, 0).Format(time.RFC3339))

		// Pretty-print the response JSON if possible.
		var pretty map[string]any
		if err := json.Unmarshal([]byte(sample.ResponseJSON), &pretty); err == nil {
			formatted, _ := json.MarshalIndent(pretty, "  ", "  ")
			fmt.Fprintf(w, "Response:\n  %s\n", string(formatted))
		} else {
			fmt.Fprintf(w, "Response: %s\n", sample.ResponseJSON)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "To flag a review as convention drift:")
	fmt.Fprintln(w, "  DRYDOCK_MODE=drift-guard drydock flag <ID> [notes]")
	return len(samples), nil
}

// FlagReview marks a meta-review as exhibiting convention drift.
func (s *Service) FlagReview(ctx context.Context, metaReviewID int64, notes string) error {
	if err := s.store.InsertDriftFlag(ctx, metaReviewID, notes); err != nil {
		return fmt.Errorf("flag review: %w", err)
	}
	s.logger.Info("flagged meta-review as convention drift",
		"meta_review_id", metaReviewID,
		"notes", notes,
	)
	return nil
}

// ListFlagged writes all drift-flagged reviews to w in human-readable format.
func (s *Service) ListFlagged(ctx context.Context, w io.Writer) (int, error) {
	flags, err := s.store.GetDriftFlaggedExamples(ctx, 100)
	if err != nil {
		return 0, fmt.Errorf("get drift flags: %w", err)
	}
	if len(flags) == 0 {
		fmt.Fprintln(w, "No drift-flagged reviews.")
		return 0, nil
	}

	fmt.Fprintf(w, "=== %d Drift-Flagged Reviews ===\n\n", len(flags))
	for _, f := range flags {
		fmt.Fprintf(w, "Flag ID: %d  |  Meta-Review ID: %d\n", f.ID, f.MetaReviewID)
		fmt.Fprintf(w, "Flagged: %s\n", time.Unix(f.FlaggedAt, 0).Format(time.RFC3339))
		if f.Notes != "" {
			fmt.Fprintf(w, "Notes:   %s\n", f.Notes)
		}
		fmt.Fprintln(w)
	}
	return len(flags), nil
}

// NegativeExamplesForPrompt returns formatted negative examples from drift-flagged
// reviews for injection into the meta-reviewer system prompt.
func (s *Service) NegativeExamplesForPrompt(ctx context.Context, limit int) (string, error) {
	flagged, err := s.store.GetDriftFlaggedResponses(ctx, limit)
	if err != nil {
		return "", fmt.Errorf("get drift flagged responses: %w", err)
	}
	if len(flagged) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("\n\nIMPORTANT — Convention drift examples (DO NOT recommend similar advice):\n")
	for i, f := range flagged {
		b.WriteString(fmt.Sprintf("\nDrift example %d", i+1))
		if f.Notes != "" {
			b.WriteString(fmt.Sprintf(" (%s)", f.Notes))
		}
		b.WriteString(":\n")
		b.WriteString(f.ResponseJSON)
		b.WriteString("\n")
	}
	return b.String(), nil
}
