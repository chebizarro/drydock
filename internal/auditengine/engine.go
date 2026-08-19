// Package auditengine orchestrates bounded whole-repository security audits.
package auditengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"drydock/internal/agenticreview"
	"drydock/internal/betterleaks"
	"drydock/internal/codemap"
	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/metrics"
	"drydock/internal/nostrprobe"
	"drydock/internal/nostrscan"
	"drydock/internal/nostrscan/knowledge"
	"drydock/internal/publisher"
	"drydock/internal/repoconfig"
	"drydock/internal/reviewengine"
	"drydock/internal/reviewsession"
	"drydock/internal/securityscan"
	"drydock/internal/securityscan/surface"
	"drydock/internal/securityverify"
	"drydock/internal/targetidentity"
	"drydock/internal/workspacesnapshot"

	"fiatjaf.com/nostr"
)

type Depth string

const (
	DepthQuick    Depth = "quick"
	DepthStandard Depth = "standard"
	DepthDeep     Depth = "deep"
)

type Budget struct {
	MaxUnits    int
	TokenBudget int
	VerifyVotes int
	ModelReview bool
	MinSeverity string
}

func BudgetForDepth(depth Depth) (Budget, error) {
	switch depth {
	case DepthQuick:
		return Budget{20, 16_000, 1, false, "high"}, nil
	case DepthStandard:
		return Budget{50, 32_000, 1, true, "info"}, nil
	case DepthDeep:
		return Budget{100, 64_000, securityverify.DeepAuditVerifyVotes, true, "info"}, nil
	default:
		return Budget{}, fmt.Errorf("auditengine: invalid depth %q", depth)
	}
}

type RepositoryManager interface {
	EnsureCanonicalRepo(context.Context, string, []string) (string, error)
	EnsureCommitAvailable(context.Context, string, string, string, []string) error
	CheckoutCommitOnBranch(context.Context, string, string, string) error
}

type CodeMapBuilder interface {
	Build(context.Context, string, string) (*codemap.Map, error)
}
type Scanner interface {
	ScanFiles(context.Context, string, []string, string) securityscan.ScanResult
	LocateSurface(context.Context, string, []string) surface.Result
}
type AgenticReviewer interface {
	Prepare(context.Context, agenticreview.PrepareInput) (*agenticreview.PreparedReview, error)
	ReviewPrepared(context.Context, *agenticreview.PreparedReview, agenticreview.ReviewOptions) (reviewengine.RunOutput, error)
	ReleasePrepared(*agenticreview.PreparedReview)
}
type Verifier interface {
	Run(context.Context, []reviewengine.Finding) ([]reviewengine.Finding, error)
}
type VerifierFactory func(votes int) Verifier

type AuditStore interface {
	CreateSecurityAudit(context.Context, string, string, string, string) (int64, error)
	StartSecurityAudit(context.Context, int64) error
	UpdateSecurityAuditCoverage(context.Context, int64, db.SecurityAuditCoverage) error
	PublishSecurityAudit(context.Context, int64, string, string) error
	FailSecurityAudit(context.Context, int64) error
	ReplaceSecurityAuditFindings(context.Context, int64, []db.SecurityAuditFinding) error
	SecurityBaselineFingerprints(context.Context, string) (map[string]struct{}, error)
}
type AuditPublisher interface {
	PublishSecurityAudit(context.Context, publisher.PublishSecurityAuditInput) (publisher.PublishSecurityAuditResult, error)
}
type ProgressReporter interface {
	ReportAuditProgress(context.Context, int64, string) error
}
type ToolRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) ([]byte, error)
}

type NostrProber interface {
	Run(context.Context, nostrprobe.Config) ([]nostrprobe.SecurityEvidence, error)
}

type osToolRunner struct{}

func (osToolRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (osToolRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Dependencies struct {
	Repos           RepositoryManager
	Store           AuditStore
	CodeMap         CodeMapBuilder
	Scanner         Scanner
	SecretScanner   betterleaks.Scanner
	AgenticReview   AgenticReviewer
	VerifierFactory VerifierFactory
	Publisher       AuditPublisher
	Progress        ProgressReporter
	Tools           ToolRunner
	Localizer       Localizer
	NostrProber     NostrProber
}
type Config struct {
	Workers           int
	NostrEnabled      string
	NostrProbeTargets []string
	NostrProbeActive  bool
}
type Engine struct {
	cfg    Config
	deps   Dependencies
	logger *slog.Logger
}

func New(cfg Config, deps Dependencies, logger *slog.Logger) *Engine {
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if deps.CodeMap == nil {
		deps.CodeMap = codemap.New()
	}
	if deps.Scanner == nil {
		deps.Scanner = securityscan.New()
	}
	if deps.Tools == nil {
		deps.Tools = osToolRunner{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	if deps.NostrProber == nil {
		deps.NostrProber = nostrprobe.New(nostrprobe.NewLibraryBackend(logger), nostrprobe.NewBinaryBackend(deps.Tools, logger), logger)
	}
	return &Engine{cfg: cfg, deps: deps, logger: logger}
}

type Request struct {
	RepoID        string
	CloneURLs     []string
	Ref           string
	Depth         Depth
	RequestedBy   string
	Subtree       string
	SinceCommit   string
	EnableSCA     bool
	EnableSecrets bool
	Localizer     string
	Nostr         repoconfig.NostrConfig
	Announcement  nostr.Event
	Requester     nostr.PubKey
	Relays        []string
}
type Result struct {
	AuditID       int64
	RepoPath      string
	Commit        string
	ScannedFiles  int
	ReviewedUnits int
	Coverage      db.SecurityAuditCoverage
	DroppedUnits  []string
	Findings      []reviewengine.Finding
	NewFindings   []reviewengine.Finding
	ProbeEvidence []nostrprobe.SecurityEvidence
	Published     publisher.PublishSecurityAuditResult
}
type candidateUnit struct {
	File     string
	Score    int
	Packet   string
	Findings []reviewengine.Finding
}
type unitResult struct {
	findings []reviewengine.Finding
	err      error
}

func (e *Engine) Run(ctx context.Context, req Request) (result Result, runErr error) {
	if e == nil {
		return result, errors.New("auditengine: nil engine")
	}
	if strings.TrimSpace(req.RepoID) == "" {
		return result, errors.New("auditengine: repo id is required")
	}
	if len(req.CloneURLs) == 0 {
		return result, errors.New("auditengine: clone urls are required")
	}
	if req.Depth == "" {
		req.Depth = DepthStandard
	}
	budget, err := BudgetForDepth(req.Depth)
	if err != nil {
		return result, err
	}
	if e.deps.Repos == nil || e.deps.Store == nil || e.deps.VerifierFactory == nil || e.deps.Publisher == nil ||
		(budget.ModelReview && e.deps.AgenticReview == nil) {
		return result, errors.New("auditengine: incomplete dependencies")
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	auditID, err := e.deps.Store.CreateSecurityAudit(ctx, req.RepoID, ref, string(req.Depth), req.RequestedBy)
	if err != nil {
		return result, err
	}
	result.AuditID = auditID
	if err := e.deps.Store.StartSecurityAudit(ctx, auditID); err != nil {
		return result, err
	}
	running := true
	defer func() {
		state := "published"
		if runErr != nil {
			state = "failed"
			if running {
				if err := e.deps.Store.FailSecurityAudit(context.WithoutCancel(ctx), auditID); err != nil {
					e.logger.Error("failed to mark security audit failed", "audit_id", auditID, "error", err)
				}
			}
		}
		metrics.SecurityAuditsRun.With(string(req.Depth), state).Inc()
	}()

	e.progress(ctx, auditID, "prepare")
	repoPath, err := e.prepare(ctx, req, ref)
	if err != nil {
		return result, fmt.Errorf("prepare checkout: %w", err)
	}
	result.RepoPath = repoPath
	result.Commit, err = gitOutput(ctx, repoPath, "rev-parse", "HEAD")
	if err != nil {
		return result, fmt.Errorf("resolve audit commit: %w", err)
	}

	var nostrProfile nostrscan.NostrProfile
	nostrActive := e.cfg.NostrEnabled != "false" && e.cfg.NostrEnabled != "" && req.Nostr.Enabled != "false" && req.Nostr.Enabled != ""
	if nostrActive {
		nostrProfile, err = nostrscan.Detect(ctx, repoPath, "HEAD", nostrscan.WithMinConfidence(req.Nostr.MinDetectConfidence), nostrscan.WithLogger(e.logger))
		if err != nil {
			return result, fmt.Errorf("detect nostr project: %w", err)
		}
		nostrActive = nostrProfile.IsNostr
	}

	e.progress(ctx, auditID, "codemap")
	codeMap, err := e.deps.CodeMap.Build(ctx, repoPath, "HEAD")
	if err != nil {
		return result, fmt.Errorf("build code map: %w", err)
	}
	files := codeMapFiles(codeMap, req.Subtree)

	e.progress(ctx, auditID, "deterministic-sweep")
	scan := e.deps.Scanner.ScanFiles(ctx, repoPath, files, "")
	surfaceResult := e.deps.Scanner.LocateSurface(ctx, repoPath, files)
	coverage := db.SecurityAuditCoverage{
		ScanOperationsScanned: scan.FilesScanned + surfaceResult.FilesScanned,
		ScanOperationsSkipped: scan.FilesSkipped + surfaceResult.FilesSkipped,
		ScanOperationsErrored: scan.FilesErrored + surfaceResult.FilesErrored,
	}
	result.ScannedFiles = scan.FilesScanned
	result.Coverage = coverage
	allDeterministic := scanFindings(scan.Findings)
	allDeterministic = append(allDeterministic, e.runSCATools(ctx, repoPath, req.EnableSCA)...)
	betterleaksRan := false
	if req.EnableSecrets {
		if e.deps.SecretScanner == nil {
			return result, errors.New("auditengine: betterleaks secret scanner is not configured")
		}
		secretScan, scanErr := e.deps.SecretScanner.Scan(ctx, betterleaks.ScanRequest{
			RepoPath:     repoPath,
			PolicyRef:    result.Commit,
			AllowedFiles: files,
		})
		if scanErr != nil {
			return result, fmt.Errorf("scan secrets with betterleaks: %w", scanErr)
		}
		allDeterministic = append(allDeterministic, scanFindings(secretScan.Findings)...)
		betterleaksRan = true
	}

	var nostrContext, nostrPreamble string
	var probeEvidence []nostrprobe.SecurityEvidence
	if nostrActive {
		roles := auditNostrRoles(nostrProfile.Roles, req.Nostr)
		rules := auditNostrRules(nostrscan.PresenceRulesForRoles(roles), req.Nostr)
		nostrScanner := securityscan.NewWithRuleSets(rules, nostrscan.SurfaceRules())
		nostrPresence := nostrScanner.ScanFiles(ctx, repoPath, files, "")
		nostrSurfaces := nostrScanner.LocateSurface(ctx, repoPath, files)
		coverage.ScanOperationsScanned += nostrPresence.FilesScanned + nostrSurfaces.FilesScanned
		coverage.ScanOperationsSkipped += nostrPresence.FilesSkipped + nostrSurfaces.FilesSkipped
		coverage.ScanOperationsErrored += nostrPresence.FilesErrored + nostrSurfaces.FilesErrored
		result.Coverage = coverage
		surfaceResult.Locations = append(surfaceResult.Locations, nostrSurfaces.Locations...)
		nostrFindings := auditNostrFindings(nostrPresence.Findings, files, roles, req.Nostr)
		if req.Nostr.AbsenceAnalysis {
			absence := nostrscan.AnalyzeAbsences(ctx, repoPath, codeMap, nostrSurfaces)
			nostrFindings = append(nostrFindings, auditNostrFindings(absence.Findings, files, roles, req.Nostr)...)
		}
		allDeterministic = append(allDeterministic, scanFindings(nostrFindings)...)
		if req.Nostr.KnowledgePack {
			nostrContext, err = knowledge.Context()
			if err != nil {
				return result, fmt.Errorf("load nostr knowledge context: %w", err)
			}
			nostrPreamble, err = knowledge.ReviewerSystemPreamble()
			if err != nil {
				return result, fmt.Errorf("load nostr reviewer preamble: %w", err)
			}
		}
		if req.Nostr.VerifyVotes > budget.VerifyVotes {
			budget.VerifyVotes = req.Nostr.VerifyVotes
		}
		probeEvidence, err = e.deps.NostrProber.Run(ctx, nostrprobe.Config{
			Enabled:           req.Nostr.Probe.Enabled,
			Active:            req.Nostr.Probe.Active && e.cfg.NostrProbeActive,
			IUnderstand:       req.Nostr.Probe.IUnderstand,
			Targets:           append([]string(nil), e.cfg.NostrProbeTargets...),
			AuthorizedTargets: append([]string(nil), e.cfg.NostrProbeTargets...),
			Timeout:           req.Nostr.Probe.Timeout,
		})
		if err != nil {
			e.logger.Warn("optional nostr dynamic probing failed; continuing audit", "error", err)
			probeEvidence = nil
		}
		result.ProbeEvidence = append([]nostrprobe.SecurityEvidence(nil), probeEvidence...)
	}

	if err := e.deps.Store.UpdateSecurityAuditCoverage(ctx, auditID, coverage); err != nil {
		return result, err
	}
	if coverage.ScanOperationsErrored > 0 {
		return result, fmt.Errorf("security audit incomplete: %d file scan operation(s) errored", coverage.ScanOperationsErrored)
	}

	e.progress(ctx, auditID, "localize")
	accepted := append([]reviewengine.Finding(nil), allDeterministic...)
	units := e.localize(ctx, repoPath, codeMap, files, accepted, surfaceResult, req.SinceCommit)

	e.progress(ctx, auditID, "review")
	if budget.ModelReview {
		modelFindings, reviewErr := e.reviewAgentically(ctx, req, repoPath, result.Commit, files, units, accepted, surfaceResult, budget, nostrContext, nostrPreamble)
		if reviewErr != nil {
			return result, reviewErr
		}
		accepted = append(accepted, modelFindings...)
		// Re-localize after candidate acceptance so findings outside the initial
		// heuristic units and CWE hypotheses from the shared loop reach the
		// existing localizer and verifier.
		units = e.localize(ctx, repoPath, codeMap, files, accepted, surfaceResult, req.SinceCommit)
	}
	units = e.applyModelLocalization(ctx, req.Localizer, codeMap, files, accepted, units)
	if len(units) > budget.MaxUnits {
		for _, unit := range units[budget.MaxUnits:] {
			result.DroppedUnits = append(result.DroppedUnits, unit.File)
		}
		e.logger.Info("audit localization dropped units due to depth budget", "audit_id", auditID, "depth", req.Depth, "kept", budget.MaxUnits, "dropped", len(result.DroppedUnits), "files", result.DroppedUnits)
		units = units[:budget.MaxUnits]
	}
	coverage.UnitsDropped = len(result.DroppedUnits)
	result.Coverage = coverage
	if err := e.deps.Store.UpdateSecurityAuditCoverage(ctx, auditID, coverage); err != nil {
		return result, err
	}
	for i := range units {
		units[i].Findings = findingsForFile(accepted, units[i].File)
	}
	verified, err := e.reviewUnits(ctx, units, budget)
	if err != nil {
		return result, err
	}
	result.ReviewedUnits = len(units)
	verified = reviewengine.DeduplicateFindings(verified)
	verified = nostrprobe.Corroborate(verified, probeEvidence)
	result.Findings = verified
	for _, finding := range verified {
		metrics.SecurityFindings.With(findingCWE(finding), finding.Severity).Inc()
	}

	e.progress(ctx, auditID, "aggregate")
	persisted := make([]db.SecurityAuditFinding, 0, len(verified))
	fingerprints := make(map[string]string, len(verified))
	for _, finding := range verified {
		cwe := findingCWE(finding)
		fingerprint := db.SecurityFindingFingerprint(finding.File, cwe, nearbyCode(repoPath, finding))
		fingerprints[findingKey(finding)] = fingerprint
		persisted = append(persisted, db.SecurityAuditFinding{File: finding.File, Line: finding.Line, CWE: cwe, Severity: finding.Severity, Confidence: finding.Confidence, Verified: true, RefuteVotes: budget.VerifyVotes, Fingerprint: fingerprint})
	}
	if err := e.deps.Store.ReplaceSecurityAuditFindings(ctx, auditID, persisted); err != nil {
		return result, err
	}
	baseline, err := e.deps.Store.SecurityBaselineFingerprints(ctx, req.RepoID)
	if err != nil {
		return result, err
	}
	for _, finding := range verified {
		if _, known := baseline[fingerprints[findingKey(finding)]]; !known {
			result.NewFindings = append(result.NewFindings, finding)
		}
	}
	metrics.SecurityBaselineSuppressed.Add(int64(len(verified) - len(result.NewFindings)))

	e.progress(ctx, auditID, "publish")
	pubFindings := make([]publisher.SecurityAuditFinding, 0, len(result.NewFindings))
	for _, finding := range result.NewFindings {
		pubFindings = append(pubFindings, publisher.SecurityAuditFinding{CWE: findingCWE(finding), Severity: finding.Severity, Message: finding.Explanation, File: finding.File, Line: finding.Line, Evidence: finding.Evidence, Remediation: finding.Suggestion, Confidence: finding.Confidence})
	}
	tools := []publisher.AuditTool{{Name: "drydock-securityscan"}, {Name: "drydock-sec70b"}}
	if betterleaksRan {
		tools = append(tools, publisher.AuditTool{Name: betterleaks.BinaryName})
	}
	if len(probeEvidence) > 0 {
		tools = append(tools, publisher.AuditTool{Name: "nostr-secprobe"})
	}
	complete := coverage.ScanOperationsSkipped == 0 && coverage.ScanOperationsErrored == 0 && coverage.UnitsDropped == 0
	summary := fmt.Sprintf("Security audit completed with %d new verified finding(s).", len(pubFindings))
	if !complete {
		summary = fmt.Sprintf(
			"Security audit produced %d new verified finding(s) with incomplete coverage: %d scan operation(s) skipped and %d candidate review unit(s) omitted by the %s depth budget.",
			len(pubFindings), coverage.ScanOperationsSkipped, coverage.UnitsDropped, req.Depth,
		)
	}
	result.Published, err = e.deps.Publisher.PublishSecurityAudit(ctx, publisher.PublishSecurityAuditInput{
		AuditID: auditID, Announcement: req.Announcement, Ref: ref, Commit: result.Commit,
		Summary: summary, Depth: string(req.Depth), Complete: complete, Verified: true,
		Coverage: publisher.SecurityAuditCoverage{
			ScanOperationsScanned: coverage.ScanOperationsScanned, ScanOperationsSkipped: coverage.ScanOperationsSkipped,
			ScanOperationsErrored: coverage.ScanOperationsErrored, UnitsDropped: coverage.UnitsDropped,
		},
		Findings: pubFindings, ProbeEvidence: probeEvidence, Tools: tools,
		Requester: req.Requester, Relays: req.Relays,
	})
	if err != nil {
		return result, fmt.Errorf("publish security audit: %w", err)
	}
	if err := e.deps.Store.PublishSecurityAudit(ctx, auditID, result.Published.ReportEventID, result.Published.SARIFSHA256); err != nil {
		return result, err
	}
	running = false
	e.progress(ctx, auditID, "published")
	return result, nil
}

func (e *Engine) progress(ctx context.Context, auditID int64, phase string) {
	e.logger.Info("security audit progress", "audit_id", auditID, "phase", phase)
	if e.deps.Progress != nil {
		if err := e.deps.Progress.ReportAuditProgress(ctx, auditID, phase); err != nil {
			e.logger.Warn("security audit progress publish failed", "audit_id", auditID, "phase", phase, "error", err)
		}
	}
}
func (e *Engine) prepare(ctx context.Context, req Request, ref string) (string, error) {
	repoPath, err := e.deps.Repos.EnsureCanonicalRepo(ctx, req.RepoID, req.CloneURLs)
	if err != nil {
		return "", err
	}
	if ref == "HEAD" {
		return repoPath, nil
	}
	if err := e.deps.Repos.EnsureCommitAvailable(ctx, repoPath, "", ref, req.CloneURLs); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(ref))
	if err := e.deps.Repos.CheckoutCommitOnBranch(ctx, repoPath, "audit/"+hex.EncodeToString(sum[:6]), ref); err != nil {
		return "", err
	}
	return repoPath, nil
}
func codeMapFiles(m *codemap.Map, subtree string) []string {
	subtree = strings.Trim(strings.TrimSpace(filepath.ToSlash(subtree)), "/")
	files := make([]string, 0, len(m.Files))
	for file := range m.Files {
		if subtree == "" || file == subtree || strings.HasPrefix(file, subtree+"/") {
			files = append(files, file)
		}
	}
	slices.Sort(files)
	return files
}
func scanFindings(findings []securityscan.SecurityFinding) []reviewengine.Finding {
	out := make([]reviewengine.Finding, 0, len(findings))
	for _, finding := range findings {
		cwe, evidence := securityscan.SASTRuleCWE[finding.RuleID], finding.Evidence
		if strings.HasPrefix(finding.RuleID, "NOSTR-") {
			evidence = "[" + finding.RuleID + "] " + evidence
		}
		if cwe != "" {
			evidence = "[" + cwe + "] " + evidence
		}
		out = append(out, reviewengine.Finding{Severity: finding.Severity, Category: "security", File: finding.File, Line: finding.Line, Evidence: evidence, Explanation: finding.Description, Suggestion: finding.Suggestion, Sensitive: finding.Sensitive, Confidence: finding.Confidence})
	}
	return out
}
func auditNostrRoles(detected []nostrscan.Role, cfg repoconfig.NostrConfig) []nostrscan.Role {
	detectedStrings := make([]string, 0, len(detected))
	for _, role := range detected {
		detectedStrings = append(detectedStrings, string(role))
	}
	configured := cfg.EffectiveRoles(detectedStrings)
	roles := make([]nostrscan.Role, 0, len(configured))
	for _, role := range configured {
		roles = append(roles, nostrscan.Role(role))
	}
	return roles
}

func auditNostrRules(rules []securityscan.Rule, cfg repoconfig.NostrConfig) []securityscan.Rule {
	out := make([]securityscan.Rule, 0, len(rules))
	for _, rule := range rules {
		if cfg.AllowsRule(rule.ID) {
			out = append(out, rule)
		}
	}
	return out
}

func auditNostrFindings(findings []securityscan.SecurityFinding, files []string, roles []nostrscan.Role, cfg repoconfig.NostrConfig) []securityscan.SecurityFinding {
	allowedFiles := make(map[string]struct{}, len(files))
	for _, file := range files {
		allowedFiles[file] = struct{}{}
	}
	out := make([]securityscan.SecurityFinding, 0, len(findings))
	for _, finding := range findings {
		if _, ok := allowedFiles[finding.File]; !ok {
			continue
		}
		if cfg.AllowsRule(finding.RuleID) && nostrscan.RuleAppliesToRoles(finding.RuleID, roles) {
			out = append(out, finding)
		}
	}
	return out
}

func (e *Engine) localize(ctx context.Context, repoPath string, codeMap *codemap.Map, files []string, deterministic []reviewengine.Finding, surfaces surface.Result, sinceCommit string) []candidateUnit {
	scores, allowed := make(map[string]int), make(map[string]struct{}, len(files))
	for _, file := range files {
		allowed[file] = struct{}{}
	}
	for _, finding := range deterministic {
		if _, ok := allowed[finding.File]; ok {
			scores[finding.File] += 100
		}
	}
	for _, location := range surfaces.Locations {
		if _, ok := allowed[location.File]; ok {
			scores[location.File] += 50
		}
	}
	for i, file := range recentFiles(ctx, repoPath, sinceCommit) {
		if _, ok := allowed[file]; ok {
			score := 20 - i
			if score < 1 {
				score = 1
			}
			scores[file] += score
		}
	}
	units := make([]candidateUnit, 0, len(scores))
	for file, score := range scores {
		units = append(units, candidateUnit{File: file, Score: score})
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].Score != units[j].Score {
			return units[i].Score > units[j].Score
		}
		left, right := topRank(codeMap, units[i].File), topRank(codeMap, units[j].File)
		if left != right {
			return left > right
		}
		return units[i].File < units[j].File
	})
	return units
}
func (e *Engine) applyModelLocalization(ctx context.Context, strategy string, codeMap *codemap.Map, files []string, deterministic []reviewengine.Finding, heuristic []candidateUnit) []candidateUnit {
	if !strings.EqualFold(strings.TrimSpace(strategy), "antares") {
		return heuristic
	}
	if e.deps.Localizer == nil {
		e.logger.Info("seclocalize unconfigured; falling back to heuristic localization")
		return heuristic
	}
	cweSet := make(map[string]struct{})
	for _, finding := range deterministic {
		if cwe := findingCWE(finding); cwe != "CWE-000" {
			cweSet[cwe] = struct{}{}
		}
	}
	cwes := make([]string, 0, len(cweSet))
	for cwe := range cweSet {
		cwes = append(cwes, cwe)
	}
	slices.Sort(cwes)
	if len(cwes) == 0 {
		e.logger.Info("seclocalize has no CWE hypotheses; falling back to heuristic localization")
		return heuristic
	}
	allowed := make(map[string]struct{}, len(files))
	for _, file := range files {
		allowed[file] = struct{}{}
	}
	scores := make(map[string]int, len(heuristic))
	for _, unit := range heuristic {
		scores[unit.File] = unit.Score
	}
	for _, cwe := range cwes {
		localized, err := e.deps.Localizer.Localize(ctx, cwe, codeMap.Top(500))
		if err != nil {
			e.logger.Warn("seclocalize failed; falling back to heuristic localization", "cwe", cwe, "error", err)
			return heuristic
		}
		for i, file := range localized {
			if _, ok := allowed[file]; !ok {
				continue
			}
			score := 1000 - i
			if score < 1 {
				score = 1
			}
			scores[file] += score
		}
	}
	units := make([]candidateUnit, 0, len(scores))
	for file, score := range scores {
		units = append(units, candidateUnit{File: file, Score: score})
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].Score != units[j].Score {
			return units[i].Score > units[j].Score
		}
		return units[i].File < units[j].File
	})
	return units
}

func recentFiles(ctx context.Context, repoPath, sinceCommit string) []string {
	args := []string{"log", "--name-only", "--pretty=format:"}
	if strings.TrimSpace(sinceCommit) != "" {
		args = append(args, strings.TrimSpace(sinceCommit)+"..HEAD")
	} else {
		args = append(args, "-n", "50")
	}
	out, err := gitOutput(ctx, repoPath, args...)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var files []string
	for _, line := range strings.Split(out, "\n") {
		file := strings.TrimSpace(filepath.ToSlash(line))
		if file == "" {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	return files
}
func topRank(m *codemap.Map, file string) float64 {
	for _, ranked := range m.RepoMap {
		if ranked.Path == file {
			return ranked.Score
		}
	}
	return 0
}

func (e *Engine) reviewAgentically(ctx context.Context, req Request, repoPath, commit string, files []string, units []candidateUnit, deterministic []reviewengine.Finding, surfaces surface.Result, budget Budget, nostrContext, nostrPreamble string) ([]reviewengine.Finding, error) {
	rootID := targetidentity.SHA256(fmt.Sprintf("security-audit:%s:%s", req.RepoID, commit))
	prepared, err := e.deps.AgenticReview.Prepare(ctx, agenticreview.PrepareInput{
		Mode: reviewsession.ModeSecurityAudit,
		Snapshot: agenticreview.SnapshotSpec{
			Kind: workspacesnapshot.KindPinnedGit, RepoPath: repoPath, Ref: commit,
			Allowlist: append([]string(nil), files...),
		},
		BuildInput: contextbuilder.BuildInput{
			RepoID: req.RepoID, TokenBudgetOverride: budget.TokenBudget, DisableDocs: true,
		},
		Target: agenticreview.TargetInput{
			RepoID: req.RepoID, RootID: rootID, PatchEventID: rootID,
			CanonicalRemoteIdentity: targetidentity.RemoteIdentity(req.CloneURLs),
			BaseCommit:              commit, TipCommit: commit, PreparedDiffSHA256: targetidentity.SHA256(""),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("prepare agentic security audit: %w", err)
	}
	defer e.deps.AgenticReview.ReleasePrepared(prepared)

	systemPrompt := securityAuditPrompt()
	if nostrPreamble != "" {
		systemPrompt += "\n\n" + nostrPreamble
	}
	output, err := e.deps.AgenticReview.ReviewPrepared(ctx, prepared, agenticreview.ReviewOptions{
		ReviewerRoute: reviewengine.RouteSec70B, ReviewerSystemPromptOverride: systemPrompt,
		AdditionalInstructions: auditReviewInstructions(units, deterministic, surfaces, nostrContext),
		SkipWalkthrough:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("agentic security audit review: %w", err)
	}
	findings := append([]reviewengine.Finding(nil), output.Review.Findings...)
	for i := range findings {
		findings[i].Category = "security"
	}
	return findings, nil
}

func auditReviewInstructions(units []candidateUnit, findings []reviewengine.Finding, surfaces surface.Result, nostrContext string) string {
	payload := struct {
		CandidateFiles []string               `json:"candidate_files"`
		Findings       []reviewengine.Finding `json:"deterministic_findings"`
		Surfaces       []surface.Location     `json:"security_surfaces"`
		NostrContext   string                 `json:"nostr_context,omitempty"`
	}{Findings: findings, Surfaces: surfaces.Locations, NostrContext: nostrContext}
	for _, unit := range units {
		payload.CandidateFiles = append(payload.CandidateFiles, unit.File)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "Review the frozen snapshot for reachable security findings."
	}
	return "Treat deterministic findings as hypotheses, inspect beyond the candidate files when tracing reachability, and submit only accepted findings.\n" + string(encoded)
}

func (e *Engine) reviewUnits(ctx context.Context, units []candidateUnit, budget Budget) ([]reviewengine.Finding, error) {
	if len(units) == 0 {
		return nil, nil
	}
	workers := e.cfg.Workers
	if workers > len(units) {
		workers = len(units)
	}
	jobs, results := make(chan candidateUnit), make(chan unitResult, len(units))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verifier := e.deps.VerifierFactory(budget.VerifyVotes)
			for unit := range jobs {
				if err := ctx.Err(); err != nil {
					results <- unitResult{err: err}
					continue
				}
				candidates := append([]reviewengine.Finding(nil), unit.Findings...)
				if !budget.ModelReview {
					candidates = filterSeverity(candidates, budget.MinSeverity)
				}
				verified, err := verifier.Run(ctx, reviewengine.DeduplicateFindings(candidates))
				if err != nil {
					results <- unitResult{err: fmt.Errorf("verify %s: %w", unit.File, err)}
					continue
				}
				for i := range verified {
					if strings.Contains(strings.ToUpper(verified[i].Evidence), "NOSTR-") {
						cwe := strings.ToUpper(strings.TrimSpace(verified[i].Category))
						if strings.HasPrefix(cwe, "CWE-") && !strings.Contains(verified[i].Evidence, "["+cwe+"]") {
							verified[i].Evidence = "[" + cwe + "] " + verified[i].Evidence
						}
						verified[i].Category = "security"
					}
				}
				results <- unitResult{findings: verified}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, unit := range units {
			select {
			case jobs <- unit:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	var findings []reviewengine.Finding
	var errs []error
	for result := range results {
		findings = append(findings, result.findings...)
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return findings, nil
}
func securityAuditPrompt() string {
	return reviewengine.DefaultReviewerSystemPrompt() + "\n\nPerform a focused whole-repository security audit of the supplied review unit. Trace attacker-controlled sources through the blast radius to concrete sinks, account for existing mitigations, map candidates to the most specific CWE, and report only reachable security findings."
}
func filterSeverity(findings []reviewengine.Finding, minimum string) []reviewengine.Finding {
	out := findings[:0]
	for _, finding := range findings {
		if reviewengine.IsAtOrAboveSeverity(finding.Severity, minimum) {
			out = append(out, finding)
		}
	}
	return out
}
func findingsForFile(findings []reviewengine.Finding, file string) []reviewengine.Finding {
	var out []reviewengine.Finding
	for _, finding := range findings {
		if finding.File == file {
			out = append(out, finding)
		}
	}
	return out
}
func findingCWE(finding reviewengine.Finding) string {
	category := strings.ToUpper(strings.TrimSpace(finding.Category))
	if strings.HasPrefix(category, "CWE-") {
		return category
	}
	evidence := strings.ToUpper(finding.Evidence)
	if start := strings.Index(evidence, "[CWE-"); start >= 0 {
		if end := strings.Index(evidence[start:], "]"); end > 0 {
			return evidence[start+1 : start+end]
		}
	}
	return "CWE-000"
}
func findingKey(finding reviewengine.Finding) string {
	return strings.ToLower(finding.File) + ":" + strconv.Itoa(finding.Line) + ":" + strings.ToLower(finding.Category)
}
func nearbyCode(repoPath string, finding reviewengine.Finding) string {
	data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(finding.File)))
	if err != nil {
		return finding.Evidence
	}
	lines := strings.Split(string(data), "\n")
	start := finding.Line - 3
	if start < 0 {
		start = 0
	}
	end := finding.Line + 2
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return finding.Evidence
	}
	return strings.Join(lines[start:end], "\n")
}
func gitOutput(ctx context.Context, repoPath string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (e *Engine) runSCATools(ctx context.Context, repoPath string, enabled bool) []reviewengine.Finding {
	var findings []reviewengine.Finding
	if enabled {
		tools := []struct {
			names []string
			args  func(string) []string
		}{{[]string{"trivy"}, func(repo string) []string { return []string{"fs", "--format", "json", repo} }}, {[]string{"grype"}, func(repo string) []string { return []string{"dir:" + repo, "-o", "json"} }}, {[]string{"osv-scanner", "osv"}, func(repo string) []string { return []string{"--format", "json", "-r", repo} }}}
		for _, tool := range tools {
			name, ok := e.availableTool(tool.names...)
			if !ok {
				e.logger.Info("optional security scanner unavailable; skipping", "tools", tool.names)
				continue
			}
			out, err := e.deps.Tools.Run(ctx, name, tool.args(repoPath)...)
			if err != nil {
				e.logger.Warn("optional security scanner failed", "tool", name, "error", err)
				continue
			}
			findings = append(findings, parseExternalFindings(name, out)...)
		}
	}
	return findings
}
func (e *Engine) availableTool(names ...string) (string, bool) {
	for _, name := range names {
		if path, err := e.deps.Tools.LookPath(name); err == nil {
			return path, true
		}
	}
	return "", false
}
func parseExternalFindings(tool string, data []byte) []reviewengine.Finding {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	var findings []reviewengine.Finding
	walkJSON(value, func(item map[string]any) {
		file := firstString(item, "Target", "file", "File", "path", "Path")
		line := firstInt(item, "StartLine", "line", "Line")
		message := firstString(item, "Description", "description", "message", "Message", "Title", "RuleID")
		severity := strings.ToLower(firstString(item, "Severity", "severity"))
		if severity == "" {
			severity = "high"
		}
		if !reviewengine.IsValidSeverity(severity) {
			severity = "medium"
		}
		if file == "" || message == "" {
			return
		}
		if line <= 0 {
			line = 1
		}
		findings = append(findings, reviewengine.Finding{Severity: severity, Category: "security", File: filepath.ToSlash(file), Line: line, Evidence: "[" + tool + "] " + message, Explanation: message, Confidence: .9})
	})
	return reviewengine.DeduplicateFindings(findings)
}
func walkJSON(value any, visit func(map[string]any)) {
	switch typed := value.(type) {
	case map[string]any:
		visit(typed)
		for _, child := range typed {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkJSON(child, visit)
		}
	}
}
func firstString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := item[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func firstInt(item map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := item[key].(type) {
		case float64:
			return int(value)
		case json.Number:
			number, _ := strconv.Atoi(value.String())
			return number
		}
	}
	return 0
}
