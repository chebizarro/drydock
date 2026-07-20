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
	// BaseRepoConfig is the raw .drydock.yaml content from the canonical
	// base branch (before patch application). Nil if the file is absent.
	BaseRepoConfig []byte
	// Diff is the unified diff of the prepared change. It is populated for
	// PR-style events (kind 1618/1619), whose event content is a cover
	// letter rather than a diff. Empty for kind 1617 patch series, where
	// the event content already carries the diff.
	Diff string
}

func NewService(store *db.Store, manager *Manager, logger *slog.Logger) *Service {
	return &Service{store: store, manager: manager, logger: logger}
}

// LoadBaseRepoConfig reads .drydock.yaml from the canonical repository's
// default ref without applying a patch or creating a review branch.
func (s *Service) LoadBaseRepoConfig(ctx context.Context, repoID string) ([]byte, error) {
	cloneURLs, err := s.store.GetRepositoryCloneURLs(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if len(cloneURLs) == 0 {
		return nil, fmt.Errorf("no canonical clone URLs for repository %s", repoID)
	}
	repoPath, err := s.manager.EnsureCanonicalRepo(ctx, repoID, cloneURLs)
	if err != nil {
		return nil, err
	}
	return s.manager.ReadFileAtDefaultRef(ctx, repoPath, ".drydock.yaml")
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
	repoPath, err := s.manager.EnsureCanonicalRepo(ctx, rec.RepoID, cloneURLs)
	if err != nil {
		return PrepareResult{}, err
	}

	// Read .drydock.yaml from the canonical base ref BEFORE applying patches.
	baseConfig, cfgErr := s.manager.ReadFileAtDefaultRef(ctx, repoPath, ".drydock.yaml")
	if cfgErr != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID},
			fmt.Errorf("read canonical .drydock.yaml: %w", cfgErr)
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

	result := PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, Branch: branch, AppliedIDs: applied, BaseRepoConfig: baseConfig}
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

	// Read .drydock.yaml from the canonical base repo, NOT the PR clone.
	// For PRs, cloneURLs may come from the fork — we must source config
	// from the canonical repository to prevent a fork/PR from influencing
	// its own review policy.
	var baseConfig []byte
	canonicalURLs, canonErr := s.store.GetRepositoryCloneURLs(ctx, rec.RepoID)
	if canonErr != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID},
			fmt.Errorf("load canonical repository URLs: %w", canonErr)
	}
	if len(canonicalURLs) == 0 {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID},
			fmt.Errorf("no canonical clone URLs for repository %s", rec.RepoID)
	}
	// Ensure the canonical repo is available in a cache entry that is
	// distinct from any PR/fork clone for this repo ID.
	canonPath, ensureErr := s.manager.EnsureCanonicalRepo(ctx, rec.RepoID, canonicalURLs)
	if ensureErr != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID},
			fmt.Errorf("ensure canonical repo for config read: %w", ensureErr)
	}
	baseConfig, canonErr = s.manager.ReadFileAtDefaultRef(ctx, canonPath, ".drydock.yaml")
	if canonErr != nil {
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID},
			fmt.Errorf("read canonical .drydock.yaml: %w", canonErr)
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

	// PR event content is a cover letter, not a diff — compute the real diff
	// of the PR tip against its merge-base with the default branch so the
	// review pipeline sees actual code changes. Prefer the canonical clone as
	// the diff repository: the PR clone's origin is fork-controlled, and a
	// fork must not be able to choose the diff base and hide changes.
	diffRepoPath := repoPath
	if canonPath != "" {
		if fetchErr := s.manager.EnsureCommitAvailable(ctx, canonPath, rec.EventID, tip, cloneURLs); fetchErr == nil {
			diffRepoPath = canonPath
		} else {
			s.logger.Warn("could not fetch PR tip into canonical clone, diffing in PR clone",
				"repo_id", rec.RepoID, "tip", tip, "error", fetchErr)
		}
	}
	diff, diffErr := s.manager.DiffAgainstDefaultBranch(ctx, diffRepoPath, tip)
	if diffErr != nil {
		// Internal failure to determine the diff base — clean up the review
		// branch (the runner only installs its cleanup on success) and do NOT
		// set a FailureHint: that would publish a misleading "patch does not
		// apply, please rebase" review for what is not an apply failure.
		s.CleanupReviewBranch(ctx, repoPath, branch)
		return PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID}, fmt.Errorf("diff PR tip %s: %w", tip, diffErr)
	}

	result := PrepareResult{RepoID: rec.RepoID, RepoPath: repoPath, RootID: rec.RootID, Branch: branch, AppliedIDs: []string{target.ID.Hex()}, BaseRepoConfig: baseConfig, Diff: diff}
	s.logger.Info("prepared PR tip on review branch", "patch_event_id", rec.EventID, "repo_id", rec.RepoID, "branch", branch, "tip", tip)
	return result, nil
}

// AutoFixSuggestion is a single eligible finding for auto-fix patch generation.
type AutoFixSuggestion struct {
	FilePath      string
	SuggestedDiff string
	Confidence    float64
}

// AutoFixResult describes the outcome of auto-fix patch synthesis.
type AutoFixResult struct {
	PatchDiff    string   // combined unified diff of all applied fixes
	AppliedCount int      // number of suggestions that applied cleanly
	AppliedFiles []string // files modified by applied fixes
}

// BuildAutoFixPatch synthesizes a combined diff from the given suggestions on
// the current review branch. Only suggestions whose diffs apply cleanly are
// included. The working tree is restored to its pre-fix state afterward so
// branch cleanup can proceed normally.
func (s *Service) BuildAutoFixPatch(ctx context.Context, repoPath string, suggestions []AutoFixSuggestion) (AutoFixResult, error) {
	if len(suggestions) == 0 {
		return AutoFixResult{}, nil
	}
	return s.manager.buildAutoFixPatch(ctx, repoPath, suggestions)
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
