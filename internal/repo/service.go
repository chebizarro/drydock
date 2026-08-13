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
	RepoID         string
	RepoPath       string
	ExpectedCommit string
	RootID         string
	AppliedIDs     []string
	FailureHint    string
	workspace      *reviewWorktree
	// BaseRepoConfig is the raw .drydock.yaml content from the canonical
	// base branch (before patch application). Nil if the file is absent.
	BaseRepoConfig []byte
	// Diff and its provenance fields are populated for PR-style events
	// (kind 1618/1619), whose event content is a cover letter rather than a
	// diff. They remain empty for kind 1617 patch series.
	Diff       string
	BaseCommit string
	TipCommit  string
	DiffSHA256 string
	DiffFiles  int
	DiffBytes  int64
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
	lease, err := s.manager.acquireRepoLease(ctx, rec.RepoID, cloneURLs, true)
	if err != nil {
		return PrepareResult{}, err
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			lease.release()
		}
	}()

	// Preserve the existing kind-1617 base semantics while isolating the
	// mutable patch application in its own worktree.
	baseCommit, err := s.manager.resolveCommit(ctx, lease.repoPath, "HEAD")
	if err != nil {
		return PrepareResult{}, err
	}
	baseConfig, cfgErr := s.manager.ReadFileAtDefaultRef(ctx, lease.repoPath, ".drydock.yaml")
	if cfgErr != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
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

	workspace, err := s.manager.createReviewWorktree(ctx, lease, rec.EventID, baseCommit)
	if err != nil {
		if workspace == nil {
			return PrepareResult{}, err
		}
		leaseOwned = false
		if cleanupErr := s.manager.cleanupReviewWorktree(ctx, workspace); cleanupErr != nil {
			s.logger.Warn("failed to clean up partially created review worktree", "error", cleanupErr)
		}
		return PrepareResult{
			RepoID: rec.RepoID, RepoPath: workspace.path, ExpectedCommit: baseCommit,
			RootID: rec.RootID, workspace: workspace,
		}, err
	}
	leaseOwned = false
	if err := s.manager.ApplyPatchSeries(ctx, workspace.path, ordered); err != nil {
		if cleanupErr := s.manager.cleanupReviewWorktree(ctx, workspace); cleanupErr != nil {
			s.logger.Warn("failed to clean up review worktree after patch apply failure", "error", cleanupErr)
		}
		return PrepareResult{
			RepoID: rec.RepoID, RepoPath: workspace.path, ExpectedCommit: baseCommit,
			RootID: rec.RootID, FailureHint: err.Error(), workspace: workspace,
		}, err
	}
	applied := make([]string, 0, len(ordered))
	for _, evt := range ordered {
		applied = append(applied, evt.ID.Hex())
	}

	result := PrepareResult{
		RepoID: rec.RepoID, RepoPath: workspace.path, ExpectedCommit: baseCommit,
		RootID: rec.RootID, AppliedIDs: applied, BaseRepoConfig: baseConfig, workspace: workspace,
	}
	s.logger.Info("prepared patch series in isolated worktree",
		"patch_event_id", rec.EventID, "repo_id", rec.RepoID,
		"worktree", workspace.path, "expected_commit", baseCommit, "series_len", len(applied))
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
	sourceLease, err := s.manager.acquireRepoLease(ctx, rec.RepoID, cloneURLs, false)
	if err != nil {
		return PrepareResult{}, err
	}
	defer sourceLease.release()

	canonicalURLs, err := s.store.GetRepositoryCloneURLs(ctx, rec.RepoID)
	if err != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
			fmt.Errorf("load canonical repository URLs: %w", err)
	}
	if len(canonicalURLs) == 0 {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
			fmt.Errorf("no canonical clone URLs for repository %s", rec.RepoID)
	}
	canonicalLease, err := s.manager.acquireRepoLease(ctx, rec.RepoID, canonicalURLs, true)
	if err != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
			fmt.Errorf("ensure canonical repo for review: %w", err)
	}
	canonicalOwned := true
	defer func() {
		if canonicalOwned {
			canonicalLease.release()
		}
	}()

	baseConfig, err := s.manager.ReadFileAtDefaultRef(ctx, canonicalLease.repoPath, ".drydock.yaml")
	if err != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
			fmt.Errorf("read canonical .drydock.yaml: %w", err)
	}

	tip, err := prTipCommit(target)
	if err != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID, FailureHint: err.Error()}, err
	}
	assertedBase, err := prMergeBaseCommit(target)
	if err != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID}, err
	}
	if err := s.manager.EnsureCommitAvailable(ctx, sourceLease.repoPath, target.ID.Hex(), tip, cloneURLs); err != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID, FailureHint: err.Error()}, err
	}
	if assertedBase != "" {
		if err := s.manager.EnsureCommitAvailable(ctx, sourceLease.repoPath, target.ID.Hex(), assertedBase, cloneURLs); err != nil {
			return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
				fmt.Errorf("fetch event-asserted merge-base %s: %w", assertedBase, err)
		}
	}

	// Preserve the provenance selection added in bdfdd0b: implicit bases are
	// canonical-only, while an asserted base may be verified in the PR cache.
	diffRepoPath := canonicalLease.repoPath
	canonicalTipErr := s.manager.EnsureCommitAvailable(ctx, canonicalLease.repoPath, rec.EventID, tip, cloneURLs)
	if canonicalTipErr != nil {
		if assertedBase == "" {
			return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
				fmt.Errorf("fetch PR tip %s into canonical clone for implicit-base diff: %w", tip, canonicalTipErr)
		}
		diffRepoPath = sourceLease.repoPath
		s.logger.Warn("could not fetch PR tip into canonical clone; using event-asserted base in PR clone",
			"repo_id", rec.RepoID, "tip", tip, "base", assertedBase, "error", canonicalTipErr)
	} else if assertedBase != "" {
		if fetchErr := s.manager.EnsureCommitAvailable(ctx, canonicalLease.repoPath, rec.EventID, assertedBase, cloneURLs); fetchErr != nil {
			diffRepoPath = sourceLease.repoPath
			s.logger.Warn("could not fetch asserted PR base into canonical clone; verifying it in PR clone",
				"repo_id", rec.RepoID, "tip", tip, "base", assertedBase, "error", fetchErr)
		}
	}
	diffResult, diffErr := s.manager.DiffAgainstDefaultBranch(ctx, diffRepoPath, tip, assertedBase)
	if diffErr != nil {
		return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
			fmt.Errorf("diff PR tip %s: %w", tip, diffErr)
	}

	// The review tree is always linked to the canonical clone. If provenance
	// verification used the asserted-base fallback, import only the already
	// verified commit objects from the manager-owned PR cache; canonical refs
	// remain unchanged.
	if canonicalTipErr != nil {
		if err := s.manager.importCommit(ctx, canonicalLease.repoPath, sourceLease.repoPath, diffResult.TipCommit); err != nil {
			return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID},
				fmt.Errorf("import verified PR tip into canonical clone: %w", err)
		}
	}
	workspace, err := s.manager.createReviewWorktree(ctx, canonicalLease, rec.EventID, diffResult.TipCommit)
	if err != nil {
		if workspace == nil {
			return PrepareResult{RepoID: rec.RepoID, RootID: rec.RootID}, err
		}
		canonicalOwned = false
		if cleanupErr := s.manager.cleanupReviewWorktree(ctx, workspace); cleanupErr != nil {
			s.logger.Warn("failed to clean up partially created PR worktree", "error", cleanupErr)
		}
		return PrepareResult{
			RepoID: rec.RepoID, RepoPath: workspace.path, ExpectedCommit: diffResult.TipCommit,
			RootID: rec.RootID, workspace: workspace,
		}, err
	}
	canonicalOwned = false

	result := PrepareResult{
		RepoID: rec.RepoID, RepoPath: workspace.path, ExpectedCommit: diffResult.TipCommit, RootID: rec.RootID,
		AppliedIDs: []string{target.ID.Hex()}, BaseRepoConfig: baseConfig, workspace: workspace,
		Diff: diffResult.Diff, BaseCommit: diffResult.BaseCommit, TipCommit: diffResult.TipCommit,
		DiffSHA256: diffResult.SHA256, DiffFiles: diffResult.FileCount, DiffBytes: diffResult.ByteCount,
	}
	s.logger.Info("prepared PR tip in isolated worktree",
		"patch_event_id", rec.EventID, "repo_id", rec.RepoID, "worktree", workspace.path,
		"base", result.BaseCommit, "tip", result.TipCommit, "diff_sha256", result.DiffSHA256,
		"diff_files", result.DiffFiles, "diff_bytes", result.DiffBytes)
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

// BuildAutoFixPatch synthesizes a combined diff from the given suggestions in
// the isolated review worktree. Only suggestions whose diffs apply cleanly are
// included. The worktree is restored to its pre-fix state afterward.
func (s *Service) BuildAutoFixPatch(ctx context.Context, repoPath string, suggestions []AutoFixSuggestion) (AutoFixResult, error) {
	if len(suggestions) == 0 {
		return AutoFixResult{}, nil
	}
	return s.manager.buildAutoFixPatch(ctx, repoPath, suggestions)
}

func (s *Service) AssertPreparedReview(ctx context.Context, prep PrepareResult) error {
	if err := s.manager.assertReviewWorktree(ctx, prep.workspace, prep.RepoPath, prep.ExpectedCommit); err != nil {
		return fmt.Errorf("checkout identity assertion: %w", err)
	}
	return nil
}

func (s *Service) CleanupPreparedReview(ctx context.Context, prep PrepareResult) {
	if prep.workspace == nil {
		return
	}
	if err := s.manager.cleanupReviewWorktree(ctx, prep.workspace); err != nil {
		// Retry once: a successful remove followed by a transient prune error
		// is recoverable and should not leave the canonical cache pinned.
		if retryErr := s.manager.cleanupReviewWorktree(ctx, prep.workspace); retryErr != nil {
			s.logger.Warn("failed to clean up review worktree",
				"repo_id", prep.RepoID, "worktree", prep.RepoPath,
				"error", err, "retry_error", retryErr)
		}
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

func prMergeBaseCommit(event nostr.Event) (string, error) {
	tag := event.Tags.Find("merge-base")
	if tag == nil {
		return "", nil
	}
	if len(tag) < 2 {
		return "", fmt.Errorf("PR event %s has empty merge-base tag", event.ID.Hex())
	}
	base := strings.TrimSpace(tag[1])
	if len(base) != 40 || !isHexString(base) {
		return "", fmt.Errorf("PR event %s has invalid merge-base commit", event.ID.Hex())
	}
	return strings.ToLower(base), nil
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
