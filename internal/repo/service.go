package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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

	var target nostr.Event
	if err := json.Unmarshal([]byte(rec.RawEvent), &target); err != nil {
		return PrepareResult{}, fmt.Errorf("decode patch event: %w", err)
	}

	switch rec.Kind {
	case 1617:
		return s.preparePatchSeries(ctx, rec, target)
	case 1618, 1619:
		return s.preparePRTip(ctx, rec, target)
	default:
		return PrepareResult{}, fmt.Errorf("event %s kind %d is not a NIP-34 patch/PR event", patchEventID, rec.Kind)
	}
}

func (s *Service) preparePatchSeries(ctx context.Context, rec db.PatchEventRecord, target nostr.Event) (PrepareResult, error) {
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
		ordered = []nostr.Event{target}
	}

	branch, err := s.manager.ApplyPatchSeries(ctx, repoPath, rec.EventID, ordered)
	if err != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, FailureHint: err.Error()}, err
	}
	applied := make([]string, 0, len(ordered))
	for _, evt := range ordered {
		applied = append(applied, evt.ID.Hex())
	}

	result := PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, Branch: branch, AppliedIDs: applied}
	s.logger.Info("prepared patch series on review branch", "patch_event_id", rec.EventID, "repo_id", rec.RepoID, "branch", branch, "series_len", len(applied))
	return result, nil
}

func (s *Service) preparePRTip(ctx context.Context, rec db.PatchEventRecord, target nostr.Event) (PrepareResult, error) {
	cloneURLs := cloneURLsFromEvent(target)
	if len(cloneURLs) == 0 {
		var err error
		cloneURLs, err = s.store.GetRepositoryCloneURLs(ctx, rec.RepoID)
		if err != nil {
			return PrepareResult{}, err
		}
	}
	repoPath, err := s.manager.EnsureRepo(ctx, rec.RepoID, cloneURLs)
	if err != nil {
		return PrepareResult{}, err
	}

	tip, err := prTipCommit(target)
	if err != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, FailureHint: err.Error()}, err
	}
	if err := s.manager.EnsureCommitAvailable(ctx, repoPath, target.ID.Hex(), tip, cloneURLs); err != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, FailureHint: err.Error()}, err
	}
	branch := "review/" + shortID(rec.EventID)
	if err := s.manager.CheckoutCommitOnBranch(ctx, repoPath, branch, tip); err != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, FailureHint: err.Error()}, err
	}

	result := PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, Branch: branch, AppliedIDs: []string{target.ID.Hex()}}
	s.logger.Info("prepared PR tip on review branch", "patch_event_id", rec.EventID, "repo_id", rec.RepoID, "branch", branch, "tip", tip)
	return result, nil
}

func (s *Service) CleanupReviewBranch(ctx context.Context, repoPath, branch string) {
	if branch == "" || repoPath == "" {
		return
	}
	if err := s.manager.CleanupReviewBranch(ctx, repoPath, branch); err != nil {
		s.logger.Warn("failed to clean up review branch", "branch", branch, "error", err)
	}
}

func cloneURLsFromEvent(event nostr.Event) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 2)
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "clone" {
			continue
		}
		for _, raw := range tag[1:] {
			v := strings.TrimSpace(raw)
			if v == "" {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func prTipCommit(event nostr.Event) (string, error) {
	tag := event.Tags.Find("c")
	if tag == nil || len(tag) < 2 {
		return "", fmt.Errorf("PR event %s missing c tag", event.ID.Hex())
	}
	tip := strings.TrimSpace(tag[1])
	if len(tip) != 40 || !isHexString(tip) {
		return "", fmt.Errorf("PR event %s has invalid c tag commit", event.ID.Hex())
	}
	return strings.ToLower(tip), nil
}

func isHexString(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
