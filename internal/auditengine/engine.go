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

	"drydock/internal/codemap"
	"drydock/internal/contextbuilder"
	"drydock/internal/db"
	"drydock/internal/publisher"
	"drydock/internal/reviewengine"
	"drydock/internal/securityscan"
	"drydock/internal/securityscan/surface"
	"drydock/internal/securityverify"

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
type Reviewer interface {
	Run(context.Context, reviewengine.RunInput) (reviewengine.RunOutput, error)
}
type Verifier interface {
	Run(context.Context, []reviewengine.Finding) ([]reviewengine.Finding, error)
}
type VerifierFactory func(votes int) Verifier

type AuditStore interface {
	CreateSecurityAudit(context.Context, string, string, string, string) (int64, error)
	StartSecurityAudit(context.Context, int64) error
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
	ContextBuilder  *contextbuilder.Builder
	Reviewer        Reviewer
	VerifierFactory VerifierFactory
	Publisher       AuditPublisher
	Progress        ProgressReporter
	Tools           ToolRunner
}
type Config struct{ Workers int }
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
	if deps.ContextBuilder == nil {
		providers := []contextbuilder.Provider{contextbuilder.NewSecuritySurfaceProvider(deps.Scanner)}
		if scanner, ok := deps.Scanner.(*securityscan.Scanner); ok {
			providers = append(providers, securityscan.NewProvider(scanner))
		}
		deps.ContextBuilder = contextbuilder.NewWithOptions(contextbuilder.NewBuilderOptions(contextbuilder.WithExtraProviders(providers...)))
	}
	if deps.Tools == nil {
		deps.Tools = osToolRunner{}
	}
	if logger == nil {
		logger = slog.Default()
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
	DroppedUnits  []string
	Findings      []reviewengine.Finding
	NewFindings   []reviewengine.Finding
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
	if e.deps.Repos == nil || e.deps.Store == nil || e.deps.Reviewer == nil || e.deps.VerifierFactory == nil || e.deps.Publisher == nil {
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
		if runErr != nil && running {
			if err := e.deps.Store.FailSecurityAudit(context.WithoutCancel(ctx), auditID); err != nil {
				e.logger.Error("failed to mark security audit failed", "audit_id", auditID, "error", err)
			}
		}
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

	e.progress(ctx, auditID, "codemap")
	codeMap, err := e.deps.CodeMap.Build(ctx, repoPath, "HEAD")
	if err != nil {
		return result, fmt.Errorf("build code map: %w", err)
	}
	files := codeMapFiles(codeMap, req.Subtree)

	e.progress(ctx, auditID, "deterministic-sweep")
	scan := e.deps.Scanner.ScanFiles(ctx, repoPath, files, "")
	result.ScannedFiles = scan.FilesScanned
	surfaceResult := e.deps.Scanner.LocateSurface(ctx, repoPath, files)
	allDeterministic := append(scanFindings(scan.Findings), e.runOptionalTools(ctx, repoPath, req)...)

	e.progress(ctx, auditID, "localize")
	units := e.localize(ctx, repoPath, codeMap, files, allDeterministic, surfaceResult, req.SinceCommit)
	if len(units) > budget.MaxUnits {
		for _, unit := range units[budget.MaxUnits:] {
			result.DroppedUnits = append(result.DroppedUnits, unit.File)
		}
		e.logger.Info("audit localization dropped units due to depth budget", "audit_id", auditID, "depth", req.Depth, "kept", budget.MaxUnits, "dropped", len(result.DroppedUnits), "files", result.DroppedUnits)
		units = units[:budget.MaxUnits]
	}
	for i := range units {
		units[i].Findings = findingsForFile(allDeterministic, units[i].File)
		if budget.ModelReview {
			units[i].Packet = e.assemblePacket(ctx, repoPath, req.RepoID, codeMap, units[i].File, units[i].Findings, surfaceResult, budget.TokenBudget)
		}
	}

	e.progress(ctx, auditID, "review")
	verified, err := e.reviewUnits(ctx, units, budget)
	if err != nil {
		return result, err
	}
	result.ReviewedUnits = len(units)
	verified = reviewengine.DeduplicateFindings(verified)
	result.Findings = verified

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

	e.progress(ctx, auditID, "publish")
	pubFindings := make([]publisher.SecurityAuditFinding, 0, len(result.NewFindings))
	for _, finding := range result.NewFindings {
		pubFindings = append(pubFindings, publisher.SecurityAuditFinding{CWE: findingCWE(finding), Severity: finding.Severity, Message: finding.Explanation, File: finding.File, Line: finding.Line, Evidence: finding.Evidence, Remediation: finding.Suggestion, Confidence: finding.Confidence})
	}
	result.Published, err = e.deps.Publisher.PublishSecurityAudit(ctx, publisher.PublishSecurityAuditInput{Announcement: req.Announcement, Ref: ref, Commit: result.Commit, Summary: fmt.Sprintf("Security audit completed with %d new verified finding(s).", len(pubFindings)), Depth: string(req.Depth), Verified: true, Findings: pubFindings, Tools: []publisher.AuditTool{{Name: "drydock-securityscan"}, {Name: "drydock-sec70b"}}, Requester: req.Requester, Relays: req.Relays})
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
		if cwe != "" {
			evidence = "[" + cwe + "] " + evidence
		}
		out = append(out, reviewengine.Finding{Severity: finding.Severity, Category: "security", File: finding.File, Line: finding.Line, Evidence: evidence, Explanation: finding.Description, Suggestion: finding.Suggestion, Confidence: finding.Confidence})
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

func (e *Engine) assemblePacket(ctx context.Context, repoPath, repoID string, codeMap *codemap.Map, file string, findings []reviewengine.Finding, surfaces surface.Result, tokenBudget int) string {
	bundle, err := e.deps.ContextBuilder.Build(ctx, contextbuilder.BuildInput{PatchEventContent: syntheticPatch(repoPath, file), RepoPath: repoPath, RepoID: repoID, TokenBudgetOverride: tokenBudget, DisableDocs: true})
	if err != nil {
		e.logger.Warn("audit context packet degraded", "file", file, "error", err)
	}
	var b strings.Builder
	b.WriteString(bundle.Content)
	b.WriteString("\n\n## blast-radius\n")
	for _, symbol := range codeMap.Files[file].Symbols {
		fmt.Fprintf(&b, "%s:%d %s", file, symbol.StartLine, symbol.Name)
		if callers := codeMap.Callers(symbol.ID); len(callers) > 0 {
			fmt.Fprintf(&b, " callers=%s", strings.Join(callers, ","))
		}
		if callees := codeMap.Callees(symbol.ID); len(callees) > 0 {
			fmt.Fprintf(&b, " callees=%s", strings.Join(callees, ","))
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n## sast-and-cwe-hypotheses\n")
	for _, finding := range findings {
		fmt.Fprintf(&b, "%s:%d %s %s\n", finding.File, finding.Line, findingCWE(finding), finding.Evidence)
	}
	b.WriteString("\n## security-surface\n")
	for _, location := range surfaces.Locations {
		if location.File == file {
			fmt.Fprintf(&b, "[%s] %s:%d %s\n", location.Tag, location.File, location.Line, location.Evidence)
		}
	}
	return strings.TrimSpace(b.String())
}
func syntheticPatch(repoPath, file string) string {
	line := "audit target"
	if data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(file))); err == nil {
		first, _, _ := strings.Cut(string(data), "\n")
		if strings.TrimSpace(first) != "" {
			line = first
		}
	}
	return fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,0 +1,1 @@\n+%s\n", file, file, file, file, line)
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
				if budget.ModelReview {
					output, err := e.deps.Reviewer.Run(ctx, reviewengine.RunInput{ContextBundle: unit.Packet, ChangedFiles: []string{unit.File}, ReviewerRoute: reviewengine.RouteSec70B, ReviewerSystemPromptOverride: securityAuditPrompt(), SkipWalkthrough: true})
					if err != nil {
						results <- unitResult{err: fmt.Errorf("review %s: %w", unit.File, err)}
						continue
					}
					for _, finding := range output.Review.Findings {
						finding.Category = "security"
						candidates = append(candidates, finding)
					}
				} else {
					candidates = filterSeverity(candidates, budget.MinSeverity)
				}
				verified, err := verifier.Run(ctx, reviewengine.DeduplicateFindings(candidates))
				if err != nil {
					results <- unitResult{err: fmt.Errorf("verify %s: %w", unit.File, err)}
					continue
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

func (e *Engine) runOptionalTools(ctx context.Context, repoPath string, req Request) []reviewengine.Finding {
	var findings []reviewengine.Finding
	if req.EnableSCA {
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
	if req.EnableSecrets {
		name, ok := e.availableTool("gitleaks")
		if !ok {
			e.logger.Info("optional security scanner unavailable; skipping", "tools", []string{"gitleaks"})
		} else if out, err := e.deps.Tools.Run(ctx, name, "detect", "--source", repoPath, "--report-format", "json", "--report-path", "-"); err != nil {
			e.logger.Warn("optional security scanner failed", "tool", name, "error", err)
		} else {
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
