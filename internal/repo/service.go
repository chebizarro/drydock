package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"drydock/internal/db"

	"fiatjaf.com/nostr"
)

type Service struct {
	store   *db.Store
	manager *Manager
	logger  *slog.Logger
}

type PrepareResult struct {
	RepoID      string
	RepoPath    string
	Branch      string
	RootID      string
	AppliedIDs  []string
	FailureHint string
}

func NewService(store *db.Store, manager *Manager, logger *slog.Logger) *Service {
	return &Service{store: store, manager: manager, logger: logger}
}

func (s *Service) PreparePatchSeries(ctx context.Context, patchEventID string) (PrepareResult, error) {
	rec, err := s.store.GetPatchEvent(ctx, patchEventID)
	if err != nil {
		return PrepareResult{}, err
	}
	if rec.Kind != 1617 {
		return PrepareResult{}, fmt.Errorf("event %s kind %d is not a patch event (1617)", patchEventID, rec.Kind)
	}

	cloneURLs, err := s.store.GetRepositoryCloneURLs(ctx, rec.RepoID)
	if err != nil {
		return PrepareResult{}, err
	}
	repoPath, err := s.manager.EnsureRepo(ctx, rec.RepoID, cloneURLs)
	if err != nil {
		return PrepareResult{}, err
	}

	threadEvents, err := s.store.ListPatchThreadEvents(ctx, rec.RootID, rec.RepoID)
	if err != nil {
		return PrepareResult{}, err
	}
	ordered := OrderPatchSeries(threadEvents)
	if len(ordered) == 0 {
		var single nostr.Event
		if err := json.Unmarshal([]byte(rec.RawEvent), &single); err != nil {
			return PrepareResult{}, fmt.Errorf("decode patch event: %w", err)
		}
		ordered = []nostr.Event{single}
	}

	branch, err := s.manager.ApplyPatchSeries(ctx, repoPath, patchEventID, ordered)
	if err != nil {
		return PrepareResult{
			RepoID:      rec.RepoID,
			RepoPath:    repoPath,
			RootID:      rec.RootID,
			FailureHint: err.Error(),
		}, err
	}
	applied := make([]string, 0, len(ordered))
	for _, evt := range ordered {
		applied = append(applied, evt.ID.Hex())
	}

	result := PrepareResult{
		RepoID:     rec.RepoID,
		RepoPath:   repoPath,
		RootID:     rec.RootID,
		Branch:     branch,
		AppliedIDs: applied,
	}
	s.logger.Info("prepared patch series on review branch", "patch_event_id", patchEventID, "repo_id", rec.RepoID, "branch", branch, "series_len", len(applied))
	return result, nil
}

