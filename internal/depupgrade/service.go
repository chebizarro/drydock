package depupgrade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/deprunner"
	"git.sharegap.net/cascadia/drydock/internal/eventkind"
	"git.sharegap.net/cascadia/drydock/internal/publisher"
	"git.sharegap.net/cascadia/drydock/internal/repo"
	"git.sharegap.net/cascadia/drydock/internal/repoconfig"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
	"git.sharegap.net/cascadia/drydock/internal/safepath"

	"golang.org/x/mod/modfile"
	gomodsemver "golang.org/x/mod/semver"
)

// Trigger records what caused a scan; it is informational (logging/metrics).
// A new-patch trigger is intentionally absent: cheap manifest pre-detection is
// only possible for kind-1617 patches (diff in content), so it would silently
// no-op for pull-request events — see repoconfig.UpgradeTriggersConfig.
type Trigger string

const (
	TriggerManual        Trigger = "manual"
	TriggerSchedule      Trigger = "schedule"
	TriggerDefaultBranch Trigger = "default_branch"
)

// Scanner runs software-composition analysis over a prepared worktree and
// returns findings carrying package identity. internal/sca is the production
// implementation; tests stub it because no SCA tool ships in CI or the image.
type Scanner interface {
	Scan(ctx context.Context, repoPath string) ([]reviewengine.Finding, error)
}

// Updater performs a manifest edit in the isolated dep-runner sidecar and returns
// the changed manifest bytes. *deprunner.Client is the production implementation.
type Updater interface {
	Update(ctx context.Context, req deprunner.UpdateRequest) (*deprunner.UpdateResponse, error)
}

// VersionResolver resolves a scanner-reported fix to a concrete target version
// under a policy. *Resolver is the production implementation.
type VersionResolver interface {
	Resolve(ctx context.Context, pkg reviewengine.PackageIdentity, policy string) (Resolution, error)
}

// Publisher emits the upgrade as a root NIP-34 patch. *publisher.Service is the
// production implementation.
type Publisher interface {
	PublishUpgradePatch(ctx context.Context, in publisher.PublishUpgradePatchInput) (publisher.PublishFixPatchResult, error)
}

// Workspaces prepares an isolated worktree and produces the authoritative git
// diff. *repo.Service is the production implementation.
type Workspaces interface {
	PrepareUpgradeWorkspace(ctx context.Context, repoID string) (repo.PrepareResult, error)
	GenerateWorktreeDiff(ctx context.Context, repoPath string, mutate func(context.Context) error) (string, []string, error)
	CleanupPreparedReview(ctx context.Context, prep repo.PrepareResult)
}

// Membership is the fail-closed monitored-repository gate. *monitoring.Registry
// is the production implementation (shared with the pipeline and review-order
// service).
type Membership interface {
	Contains(repositoryAddress string) bool
}

// Repositories enumerates the monitored repositories for the scheduled sweep.
// *monitoring.Registry is the production implementation. Enumeration is separate
// from the Membership gate: the sweep lists candidates, then every scan still
// passes through Contains (fail-closed) at entry and again before publishing.
type Repositories interface {
	MonitoredRepositoryIDs() []string
}

// Store is the persistence surface the service needs. *db.Store satisfies it.
type Store interface {
	InsertDependencyUpgrade(ctx context.Context, up db.DependencyUpgrade) (db.DependencyUpgradeResult, error)
	SupersedeOpenDependencyUpgrades(ctx context.Context, repoID, ecosystem, packageName, keepFrom, keepTo string) (int, error)
	MarkDependencyUpgradeStatus(ctx context.Context, id int64, status db.DependencyUpgradeStatus, lastError string) error
	RecordDependencyUpgradePatchEvent(ctx context.Context, id int64, patchEventID string) error
	GetOpenDependencyUpgrades(ctx context.Context, repoID string) ([]db.DependencyUpgrade, error)
	GetRootStatus(ctx context.Context, rootID, repoID string) (kind int, eventID string, createdAt int64, ok bool, err error)
}

// Config carries operator-level knobs for the service.
type Config struct {
	// Model is the deterministic producer label stamped on published patches.
	Model string
	// Workers is the number of concurrent scan workers draining the queue.
	// Defaults to 1.
	Workers int
	// QueueSize bounds the pending-scan channel. Defaults to defaultQueueSize.
	QueueSize int
}

// Dependencies are the collaborators wired at the composition root.
type Dependencies struct {
	Store        Store
	Workspaces   Workspaces
	Scanner      Scanner
	Updater      Updater
	Resolver     VersionResolver
	Publisher    Publisher
	Monitoring   Membership
	Repositories Repositories
	Logger       *slog.Logger
}

// defaultQueueSize bounds pending scans when the caller does not set QueueSize.
const defaultQueueSize = 64

// scanJob is a queued repository scan request.
type scanJob struct {
	repoID  string
	trigger Trigger
}

// Service turns vulnerable-dependency findings into published upgrade patches.
// The path is deliberately LLM-free: policy is deterministic configuration.
type Service struct {
	cfg     Config
	deps    Dependencies
	workers int
	queue   chan scanJob
}

// New validates dependencies and returns a Service.
func New(cfg Config, deps Dependencies) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("depupgrade: store is required")
	}
	if deps.Workspaces == nil {
		return nil, errors.New("depupgrade: workspaces is required")
	}
	if deps.Scanner == nil {
		return nil, errors.New("depupgrade: scanner is required")
	}
	if deps.Updater == nil {
		return nil, errors.New("depupgrade: updater is required")
	}
	if deps.Resolver == nil {
		return nil, errors.New("depupgrade: resolver is required")
	}
	if deps.Publisher == nil {
		return nil, errors.New("depupgrade: publisher is required")
	}
	if deps.Monitoring == nil {
		return nil, errors.New("depupgrade: monitoring gate is required")
	}
	if deps.Repositories == nil {
		return nil, errors.New("depupgrade: repository enumerator is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = "drydock-depupgrade"
	}
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	queueSize := cfg.QueueSize
	if queueSize < 1 {
		queueSize = defaultQueueSize
	}
	return &Service{cfg: cfg, deps: deps, workers: workers, queue: make(chan scanJob, queueSize)}, nil
}

// ScanRepository scans one monitored repository for vulnerable dependencies and
// publishes an upgrade patch for each actionable finding under the repository's
// upgrade policy. It is fail-closed on the monitoring gate (checked at entry and
// again immediately before each publish) and idempotent end to end: the durable
// upgrade identity short-circuits re-proposals and the publication outbox reuses
// the same Nostr event id on retry.
func (s *Service) ScanRepository(ctx context.Context, repoID string, trigger Trigger) error {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return errors.New("depupgrade: repo id is required")
	}
	log := s.deps.Logger.With("repo_id", repoID, "trigger", string(trigger))

	// Fail-closed monitoring gate at entry.
	if !s.deps.Monitoring.Contains(repositoryAddress(repoID)) {
		log.Info("skipping dependency-upgrade scan: repository not monitored")
		return nil
	}

	prep, err := s.deps.Workspaces.PrepareUpgradeWorkspace(ctx, repoID)
	if err != nil {
		return fmt.Errorf("prepare upgrade workspace: %w", err)
	}
	defer s.deps.Workspaces.CleanupPreparedReview(context.WithoutCancel(ctx), prep)

	cfg, err := repoconfig.Parse(prep.BaseRepoConfig)
	if err != nil {
		// Upgrades execute repository changes, so every malformed config fails
		// closed here, including documents that contain only tuning blocks.
		return fmt.Errorf("invalid repository gating policy: %w", err)
	}
	up := cfg.Upgrades
	if !up.Enabled {
		log.Info("dependency upgrades disabled for repository")
		return nil
	}
	// Honor the repository's per-trigger opt-in. A manual scan always runs.
	switch trigger {
	case TriggerDefaultBranch:
		if !up.Triggers.DefaultBranchEnabled() {
			log.Info("default-branch upgrade trigger disabled for repository")
			return nil
		}
	case TriggerSchedule:
		if !up.Triggers.Schedule {
			log.Info("scheduled upgrade trigger disabled for repository")
			return nil
		}
	}

	findings, err := s.deps.Scanner.Scan(ctx, prep.RepoPath)
	if err != nil {
		return fmt.Errorf("scan dependencies: %w", err)
	}

	candidates := s.selectCandidates(findings, up, log)
	if len(candidates) == 0 {
		log.Info("no actionable dependency upgrades found")
		return nil
	}
	if up.MaxUpgradesPerRun > 0 && len(candidates) > up.MaxUpgradesPerRun {
		candidates = candidates[:up.MaxUpgradesPerRun]
	}

	var errs []error
	for _, cand := range candidates {
		if err := s.processCandidate(ctx, repoID, prep, up, cand, log); err != nil {
			// One package's failure must not abort the rest of the run.
			errs = append(errs, fmt.Errorf("%s %s: %w", cand.ecosystem, cand.name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("dependency upgrade scan completed with errors: %w", errors.Join(errs...))
	}
	return nil
}

// candidate is one grouped, policy-filtered upgrade target.
type candidate struct {
	ecosystem    string
	name         string
	installed    string
	fixed        string
	purl         string
	manifestPath string
	advisories   []string
}

// selectCandidates groups SCA findings by package identity, merges advisories and
// takes the highest fixed version across a package's advisories (so the chosen
// target satisfies all of them), then applies the repository's ecosystem,
// ignore, and allow policy. Findings without a resolvable package identity are
// dropped.
func (s *Service) selectCandidates(findings []reviewengine.Finding, up repoconfig.UpgradesConfig, log *slog.Logger) []candidate {
	ecosystems := toSet(up.Ecosystems)
	ignore := toSet(up.Ignore)
	allow := toSet(up.Allow)

	grouped := make(map[string]*candidate)
	var order []string
	for _, f := range findings {
		pkg := f.Package
		if pkg == nil {
			continue
		}
		eco := strings.TrimSpace(pkg.Ecosystem)
		name := strings.TrimSpace(pkg.Name)
		installed := strings.TrimSpace(pkg.InstalledVersion)
		if eco == "" || name == "" || installed == "" {
			continue
		}
		if _, ok := ecosystems[eco]; !ok {
			continue
		}
		if _, ok := ignore[name]; ok {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[name]; !ok {
				continue
			}
		}
		key := eco + "\x00" + name + "\x00" + installed
		c, ok := grouped[key]
		if !ok {
			c = &candidate{ecosystem: eco, name: name, installed: installed, purl: strings.TrimSpace(pkg.PURL), manifestPath: strings.TrimSpace(f.File)}
			grouped[key] = c
			order = append(order, key)
		}
		if c.manifestPath == "" {
			c.manifestPath = strings.TrimSpace(f.File)
		}
		if c.purl == "" {
			c.purl = strings.TrimSpace(pkg.PURL)
		}
		c.fixed = maxFixedVersion(eco, c.fixed, strings.TrimSpace(pkg.FixedVersion))
		c.advisories = mergeAdvisories(c.advisories, pkg.Advisories)
	}

	candidates := make([]candidate, 0, len(order))
	for _, key := range order {
		candidates = append(candidates, *grouped[key])
	}
	// Deterministic order so max_upgrades_per_run truncation is stable.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ecosystem != candidates[j].ecosystem {
			return candidates[i].ecosystem < candidates[j].ecosystem
		}
		return candidates[i].name < candidates[j].name
	})
	return candidates
}

// processCandidate resolves a target version, records durable state, edits the
// manifest via the sidecar, produces the authoritative diff, and publishes.
func (s *Service) processCandidate(ctx context.Context, repoID string, prep repo.PrepareResult, up repoconfig.UpgradesConfig, cand candidate, log *slog.Logger) error {
	clog := log.With("ecosystem", cand.ecosystem, "package", cand.name, "installed", cand.installed)

	resolution, err := s.deps.Resolver.Resolve(ctx, reviewengine.PackageIdentity{
		Ecosystem:        cand.ecosystem,
		Name:             cand.name,
		InstalledVersion: cand.installed,
		FixedVersion:     cand.fixed,
		PURL:             cand.purl,
		Advisories:       cand.advisories,
	}, up.Policy)
	if err != nil {
		return fmt.Errorf("resolve target version: %w", err)
	}

	if resolution.Status == StatusNoFix {
		// Record that no fix is known so a re-scan does not re-resolve it.
		_, err := s.deps.Store.InsertDependencyUpgrade(ctx, db.DependencyUpgrade{
			RepoID: repoID, Ecosystem: cand.ecosystem, PackageName: cand.name,
			PURL: cand.purl, FromVersion: cand.installed, ToVersion: "",
			ManifestPath: cand.manifestPath, Advisories: cand.advisories,
			Policy: up.Policy, Status: db.DependencyUpgradeNoFixAvailable,
		})
		if err != nil {
			return fmt.Errorf("record no-fix upgrade: %w", err)
		}
		clog.Info("no fixed version available; recorded no_fix_available")
		return nil
	}

	target := strings.TrimSpace(resolution.TargetVersion)
	if target == "" {
		return fmt.Errorf("resolver returned empty target for status %q", resolution.Status)
	}
	// If the installed version already satisfies the target there is nothing to
	// upgrade (e.g. a scanner false positive); skip without recording a row. Both
	// sides are canonicalized so Go's v-prefix mismatch does not defeat the check.
	if cmp, cmpErr := compareVersion(cand.ecosystem, canonicalFixedVersion(cand.ecosystem, target), canonicalFixedVersion(cand.ecosystem, cand.installed)); cmpErr == nil && cmp <= 0 {
		clog.Info("installed version already at or above target; skipping", "target", target)
		return nil
	}

	// Refuse Go modules carrying a non-local replace directive before recording
	// any state: a replace pointing at a module path (not a filesystem path) can
	// redirect `go get`/`go mod tidy` in the sidecar to an arbitrary VCS URL —
	// untrusted network egress and code fetch on repository-controlled input. No
	// row is written so the upgrade proceeds naturally once the repository removes
	// the replace. This is defense-in-depth ahead of the sidecar toolchain.
	if cand.ecosystem == "go" {
		if err := s.refuseNonLocalGoReplace(prep.RepoPath, cand.manifestPath); err != nil {
			if errors.Is(err, errNonLocalReplace) {
				clog.Warn("skipping Go upgrade: go.mod contains a non-local replace directive", "reason", err)
				return nil
			}
			return fmt.Errorf("check go.mod replace policy: %w", err)
		}
	}

	// Enforce at most one open proposal per package, then record this identity.
	if _, err := s.deps.Store.SupersedeOpenDependencyUpgrades(ctx, repoID, cand.ecosystem, cand.name, cand.installed, target); err != nil {
		return fmt.Errorf("supersede prior upgrades: %w", err)
	}
	insert, err := s.deps.Store.InsertDependencyUpgrade(ctx, db.DependencyUpgrade{
		RepoID: repoID, Ecosystem: cand.ecosystem, PackageName: cand.name,
		PURL: cand.purl, FromVersion: cand.installed, ToVersion: target,
		ManifestPath: cand.manifestPath, Advisories: cand.advisories,
		Policy: up.Policy, Status: db.DependencyUpgradeOpen,
	})
	if err != nil {
		return fmt.Errorf("record upgrade: %w", err)
	}
	row := insert.Upgrade
	// Respect a prior disposition: a superseded/rejected/merged/failed identity is
	// never resurrected, and an already-published open row is not republished.
	if insert.Disposition == db.DependencyUpgradeExisting {
		if row.Status != db.DependencyUpgradeOpen {
			clog.Info("upgrade identity already resolved; skipping", "status", string(row.Status), "target", target)
			return nil
		}
		if strings.TrimSpace(row.PatchEventID) != "" {
			clog.Info("upgrade already published; skipping", "target", target, "patch_event_id", row.PatchEventID)
			return nil
		}
	}

	manifests, err := s.gatherManifests(prep.RepoPath, cand.ecosystem, cand.manifestPath)
	if err != nil {
		return s.failUpgrade(ctx, row.ID, fmt.Errorf("gather manifests: %w", err), clog)
	}

	resp, err := s.deps.Updater.Update(ctx, deprunner.UpdateRequest{
		Ecosystem: cand.ecosystem, Package: cand.name,
		FromVersion: cand.installed, ToVersion: target, Manifests: manifests,
	})
	if err != nil {
		return s.failUpgrade(ctx, row.ID, fmt.Errorf("sidecar update: %w", err), clog)
	}
	if resp == nil || resp.Status == deprunner.StatusError {
		reason := "sidecar reported an error"
		if resp != nil && strings.TrimSpace(resp.Error) != "" {
			reason = resp.Error
		}
		return s.failUpgrade(ctx, row.ID, fmt.Errorf("sidecar update failed: %s", reason), clog)
	}
	if resp.Status == deprunner.StatusNoChange || len(resp.ChangedFiles) == 0 {
		return s.failUpgrade(ctx, row.ID, errors.New("sidecar produced no manifest changes"), clog)
	}

	diff, changedFiles, err := s.deps.Workspaces.GenerateWorktreeDiff(ctx, prep.RepoPath, func(context.Context) error {
		return applyManifestChanges(prep.RepoPath, cand.ecosystem, resp.ChangedFiles)
	})
	if err != nil {
		return s.failUpgrade(ctx, row.ID, fmt.Errorf("apply manifest changes: %w", err), clog)
	}
	if strings.TrimSpace(diff) == "" {
		return s.failUpgrade(ctx, row.ID, errors.New("manifest changes produced an empty diff"), clog)
	}
	if err := ensureAllowedChangedFiles(cand.ecosystem, changedFiles); err != nil {
		return s.failUpgrade(ctx, row.ID, err, clog)
	}

	// Re-check the monitoring gate immediately before publishing: a repository
	// removed from the operator list mid-scan must not receive a patch.
	if !s.deps.Monitoring.Contains(repositoryAddress(repoID)) {
		clog.Warn("repository left the monitored set mid-scan; not publishing")
		return nil
	}

	result, err := s.deps.Publisher.PublishUpgradePatch(ctx, publisher.PublishUpgradePatchInput{
		UpgradeID: row.ID, RepoID: repoID,
		Ecosystem: cand.ecosystem, Package: cand.name,
		FromVersion: cand.installed, ToVersion: target,
		Advisories: cand.advisories, PatchDiff: diff, Model: s.cfg.Model,
		Degraded: resolution.Degraded, DegradedReason: resolution.Warning,
	})
	if err != nil {
		return s.failUpgrade(ctx, row.ID, fmt.Errorf("publish upgrade patch: %w", err), clog)
	}
	if !result.Published {
		return s.failUpgrade(ctx, row.ID, fmt.Errorf("upgrade patch not published: %s", result.Reason), clog)
	}

	// Recording the patch event id deliberately does NOT change status: the row
	// stays open until reconciliation observes a merged/closed root status.
	if err := s.deps.Store.RecordDependencyUpgradePatchEvent(ctx, row.ID, result.EventID); err != nil {
		return fmt.Errorf("record patch event id: %w", err)
	}
	clog.Info("published dependency upgrade",
		"target", target, "patch_event_id", result.EventID, "degraded", resolution.Degraded)
	return nil
}

// failUpgrade marks a row failed with the error and returns it wrapped so the
// caller aggregates it. A failed row is not retried byte-identically (the unique
// identity blocks re-proposal until the dependency moves on).
func (s *Service) failUpgrade(ctx context.Context, id int64, cause error, log *slog.Logger) error {
	if markErr := s.deps.Store.MarkDependencyUpgradeStatus(ctx, id, db.DependencyUpgradeFailed, cause.Error()); markErr != nil {
		log.Error("failed to mark upgrade failed", "error", markErr, "cause", cause)
		return fmt.Errorf("%w (and failed to record failure: %v)", cause, markErr)
	}
	log.Warn("dependency upgrade failed", "error", cause)
	return cause
}

// gatherManifests reads the ecosystem's manifest set from the worktree, relative
// to the directory of the finding's manifest path. Every path is confined to the
// worktree via safepath. The primary manifest must be present.
func (s *Service) gatherManifests(worktreeRoot, ecosystem, manifestPath string) ([]deprunner.ManifestFile, error) {
	dir := ""
	if manifestPath != "" {
		rel, err := safepath.Normalize(manifestPath, true)
		if err != nil {
			return nil, fmt.Errorf("normalize manifest path %q: %w", manifestPath, err)
		}
		dir = path.Dir(rel)
		if dir == "." {
			dir = ""
		}
	}
	names := deprunner.ManifestFiles(ecosystem)
	if len(names) == 0 {
		return nil, fmt.Errorf("unsupported ecosystem %q", ecosystem)
	}
	primary := deprunner.PrimaryManifest(ecosystem)

	var manifests []deprunner.ManifestFile
	primaryFound := false
	for _, name := range names {
		rel := name
		if dir != "" {
			rel = path.Join(dir, name)
		}
		normalized, err := safepath.Normalize(rel, false)
		if err != nil {
			return nil, fmt.Errorf("normalize manifest %q: %w", rel, err)
		}
		abs := filepath.Join(worktreeRoot, filepath.FromSlash(normalized))
		data, err := os.ReadFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read manifest %q: %w", normalized, err)
		}
		if name == primary {
			primaryFound = true
		}
		manifests = append(manifests, deprunner.ManifestFile{Path: normalized, Content: string(data)})
	}
	if !primaryFound {
		return nil, fmt.Errorf("primary manifest %q not found for %s under %q", primary, ecosystem, dirOrRoot(dir))
	}
	return manifests, nil
}

// applyManifestChanges writes the sidecar's changed manifest bytes into the
// worktree. Every path is confined via safepath and its base name must be in the
// ecosystem's allowed set — defense in depth against a toolchain (via a lifecycle
// script) writing outside the manifest set.
func applyManifestChanges(worktreeRoot, ecosystem string, changed []deprunner.ManifestFile) error {
	for _, cf := range changed {
		rel, err := safepath.Normalize(cf.Path, false)
		if err != nil {
			return fmt.Errorf("normalize changed file %q: %w", cf.Path, err)
		}
		if !allowedManifestBase(ecosystem, rel) {
			return fmt.Errorf("sidecar returned disallowed file %q", rel)
		}
		abs := filepath.Join(worktreeRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fmt.Errorf("create dir for %q: %w", rel, err)
		}
		if err := os.WriteFile(abs, []byte(cf.Content), 0o644); err != nil {
			return fmt.Errorf("write manifest %q: %w", rel, err)
		}
	}
	return nil
}

// ensureAllowedChangedFiles verifies the authoritative git diff only touched
// files whose base names are in the ecosystem's manifest set.
func ensureAllowedChangedFiles(ecosystem string, changedFiles []string) error {
	for _, f := range changedFiles {
		if !allowedManifestBase(ecosystem, f) {
			return fmt.Errorf("upgrade diff touched unexpected file %q", f)
		}
	}
	return nil
}

// errNonLocalReplace signals that a go.mod carries a replace directive pointing
// at a module path rather than a local filesystem path.
var errNonLocalReplace = errors.New("go.mod contains a non-local replace directive")

// refuseNonLocalGoReplace reads the go.mod at the candidate's manifest directory
// and returns errNonLocalReplace if any replace directive targets a module path
// (a VCS-fetchable replacement) rather than a local filesystem path. A missing
// go.mod is not an error here — the later manifest gather reports that.
func (s *Service) refuseNonLocalGoReplace(worktreeRoot, manifestPath string) error {
	dir := ""
	if manifestPath != "" {
		rel, err := safepath.Normalize(manifestPath, true)
		if err != nil {
			return fmt.Errorf("normalize manifest path %q: %w", manifestPath, err)
		}
		if d := path.Dir(rel); d != "." {
			dir = d
		}
	}
	rel := "go.mod"
	if dir != "" {
		rel = path.Join(dir, "go.mod")
	}
	normalized, err := safepath.Normalize(rel, false)
	if err != nil {
		return fmt.Errorf("normalize go.mod path %q: %w", rel, err)
	}
	abs := filepath.Join(worktreeRoot, filepath.FromSlash(normalized))
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read go.mod %q: %w", normalized, err)
	}
	parsed, err := modfile.Parse(normalized, data, nil)
	if err != nil {
		return fmt.Errorf("parse go.mod %q: %w", normalized, err)
	}
	for _, r := range parsed.Replace {
		if !isLocalReplacement(r.New.Path, r.New.Version) {
			return fmt.Errorf("%w: %s => %s", errNonLocalReplace, r.Old.Path, r.New.Path)
		}
	}
	return nil
}

// isLocalReplacement reports whether a go.mod replace target is a local
// filesystem path. A module-path replacement carries a version or a bare module
// path (not prefixed with ./ ../ or an absolute path) and is treated as remote.
func isLocalReplacement(newPath, newVersion string) bool {
	if strings.TrimSpace(newVersion) != "" {
		return false
	}
	return strings.HasPrefix(newPath, "./") || strings.HasPrefix(newPath, "../") ||
		strings.HasPrefix(newPath, "/") || filepath.IsAbs(newPath)
}

func allowedManifestBase(ecosystem, relPath string) bool {
	base := path.Base(filepath.ToSlash(relPath))
	for _, name := range deprunner.ManifestFiles(ecosystem) {
		if base == name {
			return true
		}
	}
	return false
}

// maxFixedVersion returns the higher of two fixed versions for an ecosystem so a
// package with several advisories is bumped past all of them. Invalid or empty
// inputs never win over a valid one. Go versions are canonicalized to the
// v-prefixed spelling first (mirroring the resolver) so the comparison and the
// stored value are both valid module versions.
func maxFixedVersion(ecosystem, a, b string) string {
	a = canonicalFixedVersion(ecosystem, a)
	b = canonicalFixedVersion(ecosystem, b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	if !validVersion(ecosystem, b) {
		return a
	}
	if !validVersion(ecosystem, a) {
		return b
	}
	if cmp, err := compareVersion(ecosystem, b, a); err == nil && cmp > 0 {
		return b
	}
	return a
}

// canonicalFixedVersion normalizes a scanner-reported fixed version. Go module
// versions are reported both with and without the leading v; the canonical
// v-prefixed form is required for valid semver comparison and by the resolver.
func canonicalFixedVersion(ecosystem, v string) string {
	v = strings.TrimSpace(v)
	if ecosystem == "go" && v != "" && !strings.HasPrefix(v, "v") && gomodsemver.IsValid("v"+v) {
		return "v" + v
	}
	return v
}

func mergeAdvisories(existing []string, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	out := make([]string, 0, len(existing)+len(incoming))
	for _, group := range [][]string{existing, incoming} {
		for _, a := range group {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			key := strings.ToUpper(a)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

func dirOrRoot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// repositoryAddress builds the canonical repository announcement address from a
// "<pubkey>:<identifier>" repo id, matching the monitoring registry's membership
// key and the publisher's tag form.
func repositoryAddress(repoID string) string {
	return fmt.Sprintf("%d:%s", int(eventkind.RepositoryAnnouncement), repoID)
}
