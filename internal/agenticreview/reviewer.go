package agenticreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"drydock/internal/agenttools"
	"drydock/internal/contextbuilder"
	"drydock/internal/reviewengine"
	"drydock/internal/workspacesnapshot"
)

const (
	DefaultReviewerMaxTurns            = 20
	DefaultReviewerMaxToolCalls        = 96
	DefaultReviewerMaxCumulativeTokens = 384_000
)

var (
	ErrReviewSubmitMissing       = errors.New("agentic review: review.submit was not called successfully")
	ErrReviewerEmptyResponse     = errors.New("agentic review: repeated assistant response without tool calls")
	ErrInvalidReviewSubmission   = errors.New("agentic review: invalid review submission")
	ErrUnknownEvidence           = errors.New("agentic review: unknown current-run evidence tool call")
	ErrFindingOutsideReviewScope = errors.New("agentic review: finding is outside review scope")
	ErrCoverageIncomplete        = errors.New("agentic review: examined-file coverage is incomplete")
	ErrReviewerTargetMismatch    = errors.New("agentic review: reviewer target materials do not match snapshot")
)

const (
	StopReviewSubmitted StopReason = "review_submitted"
	StopEmptyAssistant  StopReason = "empty_assistant"
	StopCancelled       StopReason = "cancelled"
	StopTargetMismatch  StopReason = "target_mismatch"
)

type FindingScope = reviewengine.FindingScope

const (
	FindingScopePatch    = reviewengine.FindingScopePatch
	FindingScopeSnapshot = reviewengine.FindingScopeSnapshot
)

const ReviewerSystemPrompt = `You are Drydock's iterative code reviewer.
Use the read-only frozen-snapshot tools to inspect and verify the change. Every
finding must cite at least one successful evidence tool-call ID from this run.
The only successful completion is a schema-valid review.submit call. Do not
return the review as assistant prose or raw JSON. Include explicit examined-file
coverage, and when there are no findings submit outcome "no_findings".
The engine-owned policy below may describe a legacy JSON response; follow its
review rubric, but submit exclusively through review.submit.`

const reviewerCorrectiveNudge = `You did not call a tool. Continue the review with the provided tools. You can succeed only by calling review.submit with evidence-backed findings and explicit coverage.`

type ReviewerConfig struct {
	Client   reviewengine.CompletionClient
	Registry *agenttools.Registry
	Counter  contextbuilder.TokenCounter
	Snapshot *workspacesnapshot.Snapshot
	Scope    FindingScope
	Limits   LoopLimits
}

type Reviewer struct {
	config     ReviewerConfig
	patchFiles []string
	patchLines map[string]map[int]struct{}
	instanceID uint64
}

var _ reviewengine.ReviewerExecutor = (*Reviewer)(nil)

var reviewerInstanceSequence atomic.Uint64
var reviewerRunSequence atomic.Uint64

func DefaultReviewerLoopLimits() LoopLimits {
	defaults := DefaultLoopLimits()
	defaults.MaxTurns = DefaultReviewerMaxTurns
	defaults.MaxToolCalls = DefaultReviewerMaxToolCalls
	defaults.MaxCumulativeTokens = DefaultReviewerMaxCumulativeTokens
	return defaults
}

func NewReviewer(config ReviewerConfig) (*Reviewer, error) {
	if config.Client == nil || config.Registry == nil || config.Counter == nil || config.Snapshot == nil {
		return nil, fmt.Errorf("agentic review: reviewer client, registry, counter, and snapshot are required")
	}
	if err := config.Snapshot.Verify(); err != nil {
		return nil, fmt.Errorf("agentic review: verify reviewer snapshot: %w", err)
	}
	if config.Scope == "" {
		config.Scope = FindingScopePatch
	}
	if config.Scope != FindingScopePatch && config.Scope != FindingScopeSnapshot {
		return nil, fmt.Errorf("agentic review: unsupported finding scope %q", config.Scope)
	}
	config.Limits = normalizeReviewerLimits(config.Limits)
	reviewer := &Reviewer{
		config: config, instanceID: reviewerInstanceSequence.Add(1),
		patchLines: make(map[string]map[int]struct{}),
	}
	if config.Scope == FindingScopePatch {
		analysis, err := contextbuilder.NewPatchFacade().Analyze(contextbuilder.PatchAnalysisRequest{
			Diff: string(config.Snapshot.PatchContent()),
		})
		if err != nil {
			return nil, fmt.Errorf("agentic review: analyze reviewer patch scope: %w", err)
		}
		if len(analysis.ChangedFiles) == 0 {
			return nil, fmt.Errorf("agentic review: patch-scoped reviewer requires changed files")
		}
		changed := make(map[string]struct{}, len(analysis.ChangedFiles))
		for _, file := range analysis.ChangedFiles {
			normalized, err := normalizeSubmissionPath(file)
			if err != nil {
				return nil, fmt.Errorf("agentic review: invalid patch path %q: %w", file, err)
			}
			changed[normalized] = struct{}{}
			reviewer.patchFiles = append(reviewer.patchFiles, normalized)
			reviewer.patchLines[normalized] = make(map[int]struct{})
		}
		for _, file := range analysis.Files {
			normalized, err := normalizeSubmissionPath(file.Path)
			if err != nil {
				continue
			}
			if _, ok := changed[normalized]; !ok {
				continue
			}
			for _, zeroBased := range file.AddedLines {
				reviewer.patchLines[normalized][int(zeroBased)+1] = struct{}{}
			}
		}
		sort.Strings(reviewer.patchFiles)
	}
	return reviewer, nil
}

func normalizeReviewerLimits(limits LoopLimits) LoopLimits {
	defaults := DefaultReviewerLoopLimits()
	if limits.MaxTurns <= 0 {
		limits.MaxTurns = defaults.MaxTurns
	}
	if limits.MaxToolCalls <= 0 {
		limits.MaxToolCalls = defaults.MaxToolCalls
	}
	if limits.MaxCumulativeTokens <= 0 {
		limits.MaxCumulativeTokens = defaults.MaxCumulativeTokens
	}
	if limits.MaxToolResultBytes <= 0 {
		limits.MaxToolResultBytes = defaults.MaxToolResultBytes
	}
	if limits.MaxModelContext <= 0 {
		limits.MaxModelContext = defaults.MaxModelContext
	}
	return limits
}

type EvidenceRecord struct {
	ToolCallID string
	Tool       string
	Arguments  json.RawMessage
	Result     agenttools.Result
}

type EvidenceLedger struct {
	runID   string
	mu      sync.RWMutex
	records map[string]EvidenceRecord
}

func NewEvidenceLedger(runID string) *EvidenceLedger {
	return &EvidenceLedger{runID: runID, records: make(map[string]EvidenceRecord)}
}

func (l *EvidenceLedger) RunID() string {
	if l == nil {
		return ""
	}
	return l.runID
}

func (l *EvidenceLedger) Record(call reviewengine.ToolCall, result agenttools.Result) {
	if l == nil || strings.TrimSpace(call.ID) == "" || result.IsError {
		return
	}
	record := EvidenceRecord{
		ToolCallID: call.ID,
		Tool:       call.Function.Name,
		Arguments:  json.RawMessage(call.Function.Arguments),
		Result:     cloneToolResult(result),
	}
	l.mu.Lock()
	l.records[call.ID] = record
	l.mu.Unlock()
}

func (l *EvidenceLedger) Lookup(toolCallID string) (EvidenceRecord, bool) {
	if l == nil {
		return EvidenceRecord{}, false
	}
	l.mu.RLock()
	record, ok := l.records[toolCallID]
	l.mu.RUnlock()
	if !ok {
		return EvidenceRecord{}, false
	}
	record.Arguments = append(json.RawMessage(nil), record.Arguments...)
	record.Result = cloneToolResult(record.Result)
	return record, true
}

func (l *EvidenceLedger) IDs() []string {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	ids := make([]string, 0, len(l.records))
	for id := range l.records {
		ids = append(ids, id)
	}
	l.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

func cloneToolResult(result agenttools.Result) agenttools.Result {
	result.Structured = append(json.RawMessage(nil), result.Structured...)
	return result
}

type submittedFinding struct {
	Priority            string   `json:"priority"`
	Category            string   `json:"category"`
	File                string   `json:"file"`
	Line                int      `json:"line"`
	Explanation         string   `json:"explanation"`
	Suggestion          string   `json:"suggestion,omitempty"`
	SuggestedDiff       string   `json:"suggested_diff,omitempty"`
	SuggestedCode       string   `json:"suggested_code,omitempty"`
	Confidence          *float64 `json:"confidence"`
	EvidenceToolCallIDs []string `json:"evidence_tool_call_ids"`
}

type submittedCoverage struct {
	ExaminedFiles []string `json:"examined_files"`
	Outcome       string   `json:"outcome"`
	Summary       string   `json:"summary"`
}

type reviewSubmission struct {
	Summary          string             `json:"summary"`
	Findings         []submittedFinding `json:"findings"`
	NeedsMoreContext []string           `json:"needs_more_context,omitempty"`
	Coverage         submittedCoverage  `json:"coverage"`
}

type acceptedSubmission struct {
	review      reviewengine.ReviewerOutput
	coverage    submittedCoverage
	evidenceIDs []string
}

type reviewSubmitter struct {
	snapshot   *workspacesnapshot.Snapshot
	scope      FindingScope
	patchFiles []string
	patchLines map[string]map[int]struct{}
	ledger     *EvidenceLedger

	mu       sync.RWMutex
	accepted *acceptedSubmission
}

func (s *reviewSubmitter) HandleReviewSubmit(ctx context.Context, arguments json.RawMessage, _ string) (agenttools.Result, error) {
	submission, err := decodeReviewSubmission(arguments)
	if err == nil {
		var accepted acceptedSubmission
		accepted, err = s.validate(ctx, submission)
		if err == nil {
			s.mu.Lock()
			s.accepted = &accepted
			s.mu.Unlock()
			payload, marshalErr := json.Marshal(map[string]any{
				"accepted": true, "findings": len(accepted.review.Findings),
				"coverage_outcome": accepted.coverage.Outcome,
			})
			if marshalErr != nil {
				return agenttools.Result{}, marshalErr
			}
			return agenttools.Result{Content: string(payload), Structured: payload}, nil
		}
	}
	payload, marshalErr := json.Marshal(map[string]any{
		"accepted": false, "error": err.Error(),
	})
	if marshalErr != nil {
		return agenttools.Result{}, marshalErr
	}
	return agenttools.Result{Content: err.Error(), Structured: payload, IsError: true}, nil
}

func decodeReviewSubmission(arguments json.RawMessage) (reviewSubmission, error) {
	var submission reviewSubmission
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&submission); err != nil {
		return reviewSubmission{}, fmt.Errorf("%w: decode review.submit: %v", ErrInvalidReviewSubmission, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return reviewSubmission{}, fmt.Errorf("%w: %v", ErrInvalidReviewSubmission, err)
	}
	return submission, nil
}

func (s *reviewSubmitter) validate(ctx context.Context, submission reviewSubmission) (acceptedSubmission, error) {
	submission.Summary = strings.TrimSpace(submission.Summary)
	submission.Coverage.Summary = strings.TrimSpace(submission.Coverage.Summary)
	if submission.Summary == "" {
		return acceptedSubmission{}, fmt.Errorf("%w: summary is required", ErrInvalidReviewSubmission)
	}
	if submission.Findings == nil {
		return acceptedSubmission{}, fmt.Errorf("%w: findings array is required", ErrInvalidReviewSubmission)
	}
	if submission.Coverage.Summary == "" {
		return acceptedSubmission{}, fmt.Errorf("%w: coverage summary is required", ErrCoverageIncomplete)
	}
	switch {
	case len(submission.Findings) == 0 && submission.Coverage.Outcome != "no_findings":
		return acceptedSubmission{}, fmt.Errorf("%w: zero findings require explicit no_findings outcome", ErrCoverageIncomplete)
	case len(submission.Findings) > 0 && submission.Coverage.Outcome != "findings":
		return acceptedSubmission{}, fmt.Errorf("%w: non-empty findings require findings outcome", ErrCoverageIncomplete)
	}

	examined := make(map[string]struct{}, len(submission.Coverage.ExaminedFiles))
	normalizedCoverage := make([]string, 0, len(submission.Coverage.ExaminedFiles))
	for _, file := range submission.Coverage.ExaminedFiles {
		normalized, err := normalizeSubmissionPath(file)
		if err != nil {
			return acceptedSubmission{}, fmt.Errorf("%w: invalid examined file %q: %v", ErrCoverageIncomplete, file, err)
		}
		if _, duplicate := examined[normalized]; duplicate {
			return acceptedSubmission{}, fmt.Errorf("%w: duplicate examined file %s", ErrCoverageIncomplete, normalized)
		}
		if err := s.validateCoveragePath(ctx, normalized); err != nil {
			return acceptedSubmission{}, err
		}
		examined[normalized] = struct{}{}
		normalizedCoverage = append(normalizedCoverage, normalized)
	}
	if len(examined) == 0 {
		return acceptedSubmission{}, fmt.Errorf("%w: at least one examined file is required", ErrCoverageIncomplete)
	}
	if s.scope == FindingScopePatch {
		for _, required := range s.patchFiles {
			if _, ok := examined[required]; !ok {
				return acceptedSubmission{}, fmt.Errorf("%w: changed file %s was not examined", ErrCoverageIncomplete, required)
			}
		}
	}
	sort.Strings(normalizedCoverage)
	submission.Coverage.ExaminedFiles = normalizedCoverage

	output := reviewengine.ReviewerOutput{
		Summary:          submission.Summary,
		NeedsMoreContext: cleanNonEmptyStrings(submission.NeedsMoreContext),
	}
	evidenceSet := make(map[string]struct{})
	for i, finding := range submission.Findings {
		converted, evidenceIDs, err := s.validateFinding(ctx, finding)
		if err != nil {
			return acceptedSubmission{}, fmt.Errorf("finding[%d]: %w", i, err)
		}
		output.Findings = append(output.Findings, converted)
		for _, id := range evidenceIDs {
			evidenceSet[id] = struct{}{}
		}
	}
	if err := output.Validate(); err != nil {
		return acceptedSubmission{}, fmt.Errorf("%w: %v", ErrInvalidReviewSubmission, err)
	}
	evidenceIDs := make([]string, 0, len(evidenceSet))
	for id := range evidenceSet {
		evidenceIDs = append(evidenceIDs, id)
	}
	sort.Strings(evidenceIDs)
	return acceptedSubmission{review: output, coverage: submission.Coverage, evidenceIDs: evidenceIDs}, nil
}

func (s *reviewSubmitter) validateCoveragePath(ctx context.Context, file string) error {
	if s.scope == FindingScopePatch {
		if _, ok := s.patchLines[file]; !ok {
			return fmt.Errorf("%w: examined file %s is outside the patch", ErrCoverageIncomplete, file)
		}
		return nil
	}
	if _, err := s.snapshot.ReadFile(ctx, file); err != nil {
		return fmt.Errorf("%w: examined file %s: %v", ErrCoverageIncomplete, file, err)
	}
	return nil
}

func (s *reviewSubmitter) validateFinding(ctx context.Context, finding submittedFinding) (reviewengine.Finding, []string, error) {
	priority, ok := reviewengine.NormalizePriority(finding.Priority)
	if !ok || strings.TrimSpace(finding.Priority) != string(priority) {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: priority must be P0, P1, or P2", ErrInvalidReviewSubmission)
	}
	category := strings.ToLower(strings.TrimSpace(finding.Category))
	if !reviewengine.IsValidCategory(category) {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: invalid category %q", ErrInvalidReviewSubmission, finding.Category)
	}
	file, err := normalizeSubmissionPath(finding.File)
	if err != nil {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: invalid file %q: %v", ErrFindingOutsideReviewScope, finding.File, err)
	}
	if finding.Line <= 0 {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: line must be > 0", ErrInvalidReviewSubmission)
	}
	if err := s.validateFindingLocation(ctx, file, finding.Line); err != nil {
		return reviewengine.Finding{}, nil, err
	}
	explanation := strings.TrimSpace(finding.Explanation)
	if explanation == "" {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: explanation is required", ErrInvalidReviewSubmission)
	}
	if finding.Confidence == nil {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: confidence is required", ErrInvalidReviewSubmission)
	}
	if *finding.Confidence < 0 || *finding.Confidence > 1 {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: confidence must be in [0,1]", ErrInvalidReviewSubmission)
	}
	if len(finding.EvidenceToolCallIDs) == 0 {
		return reviewengine.Finding{}, nil, fmt.Errorf("%w: at least one evidence tool-call ID is required", ErrUnknownEvidence)
	}
	seenEvidence := make(map[string]struct{}, len(finding.EvidenceToolCallIDs))
	evidenceIDs := make([]string, 0, len(finding.EvidenceToolCallIDs))
	for _, rawID := range finding.EvidenceToolCallIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return reviewengine.Finding{}, nil, fmt.Errorf("%w: evidence tool-call ID is empty", ErrUnknownEvidence)
		}
		if _, duplicate := seenEvidence[id]; duplicate {
			return reviewengine.Finding{}, nil, fmt.Errorf("%w: duplicate evidence tool-call ID %s", ErrUnknownEvidence, id)
		}
		if _, exists := s.ledger.Lookup(id); !exists {
			return reviewengine.Finding{}, nil, fmt.Errorf("%w: %s", ErrUnknownEvidence, id)
		}
		seenEvidence[id] = struct{}{}
		evidenceIDs = append(evidenceIDs, id)
	}
	severity, _ := reviewengine.SeverityFromPriority(priority)
	return reviewengine.Finding{
		Priority: priority, Severity: severity, Category: category,
		File: file, Line: finding.Line,
		Evidence:    "tool calls: " + strings.Join(evidenceIDs, ", "),
		Explanation: explanation, Suggestion: strings.TrimSpace(finding.Suggestion),
		SuggestedDiff: strings.TrimSpace(finding.SuggestedDiff),
		SuggestedCode: strings.TrimSpace(finding.SuggestedCode),
		Confidence:    *finding.Confidence,
	}, evidenceIDs, nil
}

func (s *reviewSubmitter) validateFindingLocation(ctx context.Context, file string, line int) error {
	if s.scope == FindingScopePatch {
		addedLines, ok := s.patchLines[file]
		if !ok {
			return fmt.Errorf("%w: %s is outside the patch", ErrFindingOutsideReviewScope, file)
		}
		if len(addedLines) > 0 {
			if _, ok := addedLines[line]; !ok {
				return fmt.Errorf("%w: %s:%d is not an added patch line", ErrFindingOutsideReviewScope, file, line)
			}
		}
	}
	content, err := s.snapshot.ReadFile(ctx, file)
	if err != nil {
		if s.scope == FindingScopePatch && errors.Is(err, workspacesnapshot.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("%w: %s: %v", ErrFindingOutsideReviewScope, file, err)
	}
	lineCount := bytes.Count(content, []byte{'\n'}) + 1
	if len(content) == 0 {
		lineCount = 0
	}
	if line > lineCount {
		return fmt.Errorf("%w: %s:%d exceeds file length %d", ErrFindingOutsideReviewScope, file, line, lineCount)
	}
	return nil
}

func (s *reviewSubmitter) Accepted() (acceptedSubmission, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.accepted == nil {
		return acceptedSubmission{}, false
	}
	accepted := *s.accepted
	accepted.review.Findings = append([]reviewengine.Finding(nil), accepted.review.Findings...)
	accepted.review.NeedsMoreContext = append([]string(nil), accepted.review.NeedsMoreContext...)
	accepted.coverage.ExaminedFiles = append([]string(nil), accepted.coverage.ExaminedFiles...)
	accepted.evidenceIDs = append([]string(nil), accepted.evidenceIDs...)
	return accepted, true
}

func cloneReviewerMessages(messages []reviewengine.CompletionMessage) []reviewengine.CompletionMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]reviewengine.CompletionMessage, len(messages))
	for i, message := range messages {
		out[i] = message
		out[i].ToolCalls = append([]reviewengine.ToolCall(nil), message.ToolCalls...)
	}
	return out
}

func cleanNonEmptyStrings(values []string) []string {
	var cleaned []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}

func normalizeSubmissionPath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) {
		return "", errors.New("relative path is required")
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return "", errors.New("parent traversal is not allowed")
		}
	}
	normalized := path.Clean(value)
	normalized = strings.TrimPrefix(normalized, "./")
	if normalized == "." || normalized == "" || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", errors.New("invalid relative path")
	}
	return normalized, nil
}

func (r *Reviewer) validateExecutionBinding(request reviewengine.ReviewerExecutionRequest) error {
	if err := r.config.Snapshot.Verify(); err != nil {
		return fmt.Errorf("%w: verify snapshot: %v", ErrReviewerTargetMismatch, err)
	}
	if !bytes.Equal(r.config.Snapshot.PatchContent(), []byte(request.PatchDiff)) {
		return fmt.Errorf("%w: authoritative patch differs", ErrReviewerTargetMismatch)
	}
	if err := request.TargetEnvelope.VerifyMaterials(request.PatchDiff, request.ContextBundle); err != nil {
		return fmt.Errorf("%w: %v", ErrReviewerTargetMismatch, err)
	}
	analysis, err := contextbuilder.NewPatchFacade().Analyze(contextbuilder.PatchAnalysisRequest{Diff: request.PatchDiff})
	if err != nil {
		return fmt.Errorf("%w: analyze request patch: %v", ErrReviewerTargetMismatch, err)
	}
	expected := make([]string, 0, len(analysis.ChangedFiles))
	for _, file := range analysis.ChangedFiles {
		normalized, err := normalizeSubmissionPath(file)
		if err != nil {
			return fmt.Errorf("%w: invalid patch path %q", ErrReviewerTargetMismatch, file)
		}
		expected = append(expected, normalized)
	}
	supplied := make([]string, 0, len(request.ChangedFiles))
	for _, file := range request.ChangedFiles {
		normalized, err := normalizeSubmissionPath(file)
		if err != nil {
			return fmt.Errorf("%w: invalid changed file %q", ErrReviewerTargetMismatch, file)
		}
		supplied = append(supplied, normalized)
	}
	if !sameStrings(expected, supplied) {
		return fmt.Errorf("%w: changed files disagree with authoritative patch", ErrReviewerTargetMismatch)
	}
	if r.config.Scope == FindingScopePatch && !sameStrings(expected, r.patchFiles) {
		return fmt.Errorf("%w: configured patch scope differs", ErrReviewerTargetMismatch)
	}
	return nil
}

func (r *Reviewer) ExecuteReviewer(ctx context.Context, request reviewengine.ReviewerExecutionRequest) (reviewengine.ReviewerExecutionResult, error) {
	if r == nil {
		return reviewengine.ReviewerExecutionResult{}, fmt.Errorf("agentic review: nil reviewer")
	}
	if err := ctx.Err(); err != nil {
		return reviewengine.ReviewerExecutionResult{
			Trace: reviewengine.ReviewerTrace{StopReason: string(StopCancelled)},
		}, err
	}
	requestScope := request.FindingScope
	if requestScope == "" {
		requestScope = r.config.Scope
	}
	if requestScope != r.config.Scope {
		return reviewengine.ReviewerExecutionResult{}, fmt.Errorf("agentic review: reviewer scope %q does not match request %q", r.config.Scope, requestScope)
	}
	if err := r.validateExecutionBinding(request); err != nil {
		return reviewengine.ReviewerExecutionResult{
			Trace: reviewengine.ReviewerTrace{StopReason: string(StopTargetMismatch)},
		}, err
	}
	runID := fmt.Sprintf("review:%s:%d:%d", r.config.Snapshot.ID, r.instanceID, reviewerRunSequence.Add(1))
	ledger := NewEvidenceLedger(runID)
	submitter := &reviewSubmitter{
		snapshot: r.config.Snapshot, scope: r.config.Scope,
		patchFiles: append([]string(nil), r.patchFiles...),
		patchLines: r.patchLines, ledger: ledger,
	}
	role := agenttools.RoleCodeReviewer
	if r.config.Scope == FindingScopeSnapshot {
		role = agenttools.RoleSecurityAuditor
	}
	scope := agenttools.NewScope(runID, r.config.Snapshot, role)
	scope.Review = submitter
	scope.MaxResultBytes = r.config.Limits.MaxToolResultBytes
	defer r.config.Registry.ClearScopeReplay(runID)

	definitions := r.config.Registry.List(role)
	tools := make([]reviewengine.ToolSchema, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, reviewengine.ToolSchema{
			Name: definition.Name, Description: definition.Description,
			Parameters: append(json.RawMessage(nil), definition.InputSchema...),
		})
	}
	baseMessages := []reviewengine.CompletionMessage{
		{Role: reviewengine.MessageRoleSystem, Content: ReviewerSystemPrompt + "\n\nEngine-owned review policy:\n" + request.System},
		{Role: reviewengine.MessageRoleUser, Content: request.User},
	}
	baseMessages = append(baseMessages, cloneReviewerMessages(request.Conversation.History)...)
	if strings.TrimSpace(request.Conversation.Message) != "" {
		baseMessages = append(baseMessages, reviewengine.CompletionMessage{Role: reviewengine.MessageRoleUser, Content: request.Conversation.Message})
	}
	completionRequest := reviewengine.CompletionRequest{
		BaseURL: request.Endpoint.BaseURL, APIKey: request.Endpoint.APIKey,
		Model: request.Endpoint.Model, Temperature: request.Temperature,
		Messages: baseMessages, Tools: tools,
	}

	limits := r.config.Limits
	trace := LoopTrace{}
	emptyNudged := false
	servedModel := ""
	var transcript []reviewengine.CompletionMessage
	appendTranscript := func(messages ...reviewengine.CompletionMessage) error {
		cloned := cloneReviewerMessages(messages)
		if request.Conversation.Sink != nil {
			if err := request.Conversation.Sink.AppendReviewerMessages(ctx, cloned); err != nil {
				return fmt.Errorf("agentic review: persist reviewer transcript: %w", err)
			}
		}
		transcript = append(transcript, cloned...)
		return nil
	}
	fail := func(reason StopReason, err error) (reviewengine.ReviewerExecutionResult, error) {
		trace.StopReason = reason
		return reviewengine.ReviewerExecutionResult{
			Trace: reviewerTrace(trace, acceptedSubmission{}), ValidatedScope: requestScope,
			Transcript: cloneReviewerMessages(transcript),
		}, err
	}

	for trace.Turns < limits.MaxTurns {
		if err := ctx.Err(); err != nil {
			return fail(StopCancelled, err)
		}
		preflight, err := serializedRequestTokens(completionRequest, r.config.Counter)
		if err != nil {
			return fail(StopContextExceeded, err)
		}
		if preflight > limits.MaxModelContext {
			return fail(StopContextExceeded, fmt.Errorf("%w: tokens=%d limit=%d", ErrModelContext, preflight, limits.MaxModelContext))
		}
		if trace.CumulativeTokens+preflight > limits.MaxCumulativeTokens {
			return fail(StopTokensExhausted, ErrTokenLimit)
		}

		completion, err := r.config.Client.Complete(ctx, completionRequest)
		trace.Turns++
		if err != nil {
			return fail(StopTransportError, err)
		}
		if model := strings.TrimSpace(completion.Model); model != "" {
			servedModel = model
		}
		completion.Message.PromptTokens = completion.Usage.PromptTokens
		completion.Message.CompletionTokens = completion.Usage.CompletionTokens
		used := completion.Usage.TotalTokens
		if used <= 0 {
			used = preflight + r.config.Counter.Count(completion.Message.Content)
			for _, call := range completion.Message.ToolCalls {
				used += r.config.Counter.Count(call.Function.Name)
				used += r.config.Counter.Count(call.Function.Arguments)
			}
		}
		trace.CumulativeTokens += used
		if trace.CumulativeTokens > limits.MaxCumulativeTokens {
			return fail(StopTokensExhausted, ErrTokenLimit)
		}

		completionRequest.Messages = append(completionRequest.Messages, completion.Message)
		if err := appendTranscript(completion.Message); err != nil {
			return fail(StopTransportError, err)
		}
		if len(completion.Message.ToolCalls) == 0 {
			if emptyNudged {
				return fail(StopEmptyAssistant, ErrReviewerEmptyResponse)
			}
			emptyNudged = true
			completionRequest.Messages = append(completionRequest.Messages, reviewengine.CompletionMessage{
				Role: reviewengine.MessageRoleUser, Content: reviewerCorrectiveNudge,
			})
			continue
		}

		for _, call := range completion.Message.ToolCalls {
			if trace.ToolCalls >= limits.MaxToolCalls {
				return fail(StopToolsExhausted, fmt.Errorf("%w: %w", ErrReviewSubmitMissing, ErrToolCallLimit))
			}
			trace.ToolCalls++
			trace.ToolCallIDs = append(trace.ToolCallIDs, call.ID)
			toolResult, dispatchErr := r.config.Registry.Dispatch(ctx, agenttools.Invocation{
				ToolCallID: call.ID, Name: call.Function.Name,
				Arguments: json.RawMessage(call.Function.Arguments), Scope: scope,
			})
			if dispatchErr != nil {
				toolResult = agenttools.Result{Content: dispatchErr.Error(), IsError: true}
			}
			if dispatchErr == nil && !toolResult.IsError && call.Function.Name != agenttools.ToolReviewSubmit {
				ledger.Record(call, toolResult)
			}
			encoded, err := json.Marshal(toolResult)
			if err != nil {
				return fail(StopTransportError, err)
			}
			toolMessage := reviewengine.CompletionMessage{
				Role: reviewengine.MessageRoleTool, ToolCallID: call.ID,
				Name: call.Function.Name, Content: string(encoded),
			}
			completionRequest.Messages = append(completionRequest.Messages, toolMessage)
			if err := appendTranscript(toolMessage); err != nil {
				return fail(StopTransportError, err)
			}
			if call.Function.Name == agenttools.ToolReviewSubmit && dispatchErr == nil && !toolResult.IsError {
				accepted, ok := submitter.Accepted()
				if !ok {
					return fail(StopTransportError, fmt.Errorf("%w: submit handler accepted without a review", ErrReviewSubmitMissing))
				}
				trace.StopReason = StopReviewSubmitted
				return reviewengine.ReviewerExecutionResult{
					Review: accepted.review, ServedModel: servedModel,
					Trace: reviewerTrace(trace, accepted), ValidatedScope: requestScope,
					Transcript: cloneReviewerMessages(transcript),
				}, nil
			}
		}
	}
	return fail(StopTurnsExhausted, fmt.Errorf("%w: %w", ErrReviewSubmitMissing, ErrTurnLimit))
}

func reviewerTrace(trace LoopTrace, accepted acceptedSubmission) reviewengine.ReviewerTrace {
	return reviewengine.ReviewerTrace{
		Turns: trace.Turns, ToolCalls: trace.ToolCalls,
		CumulativeTokens:    trace.CumulativeTokens,
		ToolCallIDs:         append([]string(nil), trace.ToolCallIDs...),
		EvidenceToolCallIDs: append([]string(nil), accepted.evidenceIDs...),
		StopReason:          string(trace.StopReason),
		ExaminedFiles:       append([]string(nil), accepted.coverage.ExaminedFiles...),
		CoverageOutcome:     accepted.coverage.Outcome,
		CoverageSummary:     accepted.coverage.Summary,
	}
}
