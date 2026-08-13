package agenticreview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"drydock/internal/agenttools"
	"drydock/internal/contextbuilder"
	"drydock/internal/reviewengine"
	"drydock/internal/reviewsession"
	"drydock/internal/targetidentity"
	"drydock/internal/workspacesnapshot"
)

var (
	ErrInvalidPrepared            = errors.New("agentic review: invalid prepared review")
	ErrPreparedNotSessionCapable  = errors.New("agentic review: prepared fallback context cannot start a session")
	ErrSessionEnsembleUnsupported = errors.New("agentic review: ensemble sessions are not supported")
)

type snapshotManager interface {
	CreatePinned(context.Context, workspacesnapshot.PinnedGitOptions) (*workspacesnapshot.Snapshot, error)
	CreateMutable(context.Context, workspacesnapshot.MutableCopyOptions) (*workspacesnapshot.Snapshot, error)
	Get(string) (*workspacesnapshot.Snapshot, error)
	Restore(context.Context, string, string, string, string) (*workspacesnapshot.Snapshot, error)
	Acquire(string, string, time.Duration) (workspacesnapshot.Lease, error)
	Renew(string, time.Duration) (workspacesnapshot.Lease, error)
	Release(string)
}

type ServiceConfig struct {
	Snapshots      snapshotManager
	Sessions       reviewsession.Store
	Discovery      *Discovery
	Engine         *reviewengine.Engine
	Client         reviewengine.CompletionClient
	Registry       *agenttools.Registry
	Counter        contextbuilder.TokenCounter
	ReviewerLimits LoopLimits
	HistoryTokens  int
}

type Service struct {
	snapshots      snapshotManager
	sessions       reviewsession.Store
	discovery      *Discovery
	engine         *reviewengine.Engine
	client         reviewengine.CompletionClient
	registry       *agenttools.Registry
	counter        contextbuilder.TokenCounter
	reviewerLimits LoopLimits
	historyTokens  int
	nonce          string
}

func NewService(config ServiceConfig) (*Service, error) {
	if config.Snapshots == nil || config.Sessions == nil || config.Discovery == nil ||
		config.Engine == nil || config.Client == nil || config.Registry == nil || config.Counter == nil {
		return nil, fmt.Errorf("agentic review: service dependencies are required")
	}
	nonce, err := reviewsession.NewChatID()
	if err != nil {
		return nil, err
	}
	if config.HistoryTokens <= 0 {
		config.HistoryTokens = 64_000
	}
	service := &Service{
		snapshots: config.Snapshots, sessions: config.Sessions, discovery: config.Discovery,
		engine: config.Engine, client: config.Client, registry: config.Registry,
		counter: config.Counter, reviewerLimits: normalizeReviewerLimits(config.ReviewerLimits),
		historyTokens: config.HistoryTokens, nonce: nonce,
	}
	if err := service.recoverActiveSessions(context.Background()); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) recoverActiveSessions(ctx context.Context) error {
	active, err := s.sessions.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("agentic review: list active sessions: %w", err)
	}
	for _, session := range active {
		ttl := time.Until(session.ExpiresAt)
		if ttl <= 0 {
			if _, err := s.sessions.Expire(ctx, session.ChatID); err != nil {
				return err
			}
			continue
		}
		snapshot, err := s.snapshots.Restore(ctx, session.Snapshot.StoragePath, session.Snapshot.ID,
			session.Snapshot.ManifestHash, session.Snapshot.DiffHash)
		if err != nil {
			_ = s.sessions.MarkBroken(ctx, session.ChatID, err)
			continue
		}
		lease, err := s.snapshots.Acquire(snapshot.ID, session.ChatID, ttl)
		if err != nil {
			_ = s.sessions.MarkBroken(ctx, session.ChatID, err)
			continue
		}
		if err := s.sessions.BindLease(ctx, session.ChatID, lease.ID); err != nil {
			s.snapshots.Release(lease.ID)
			return err
		}
	}
	return nil
}

type SnapshotSpec struct {
	Kind          workspacesnapshot.Kind
	RepoPath      string
	Ref           string
	WorkspacePath string
	PatchRef      string
	Allowlist     []string
	TTL           time.Duration
}

type TargetInput struct {
	RepoID                  string
	RootID                  string
	PatchEventID            string
	CanonicalRemoteIdentity string
	BaseCommit              string
	TipCommit               string
	PreparedDiffSHA256      string
}

type PrepareInput struct {
	Mode       reviewsession.Mode
	Snapshot   SnapshotSpec
	Patch      string
	BuildInput contextbuilder.BuildInput
	Target     TargetInput
}

type preparedState struct {
	serviceNonce   string
	mode           reviewsession.Mode
	snapshot       *workspacesnapshot.Snapshot
	bundle         contextbuilder.ContextBundle
	patch          string
	envelope       targetidentity.Envelope
	trace          DiscoveryTrace
	artifacts      []agenttools.SelectionArtifact
	sessionCapable bool
	prepareLeaseID string
	releaseOnce    sync.Once
}

type PreparedReview struct{ state *preparedState }

type SnapshotHandle struct {
	ID           string
	Kind         workspacesnapshot.Kind
	ManifestHash string
	DiffHash     string
	ExpiresAt    time.Time
}

func (p *PreparedReview) Bundle() contextbuilder.ContextBundle {
	if p == nil || p.state == nil {
		return contextbuilder.ContextBundle{}
	}
	return cloneContextBundle(p.state.bundle)
}

func (p *PreparedReview) SnapshotHandle() SnapshotHandle {
	if p == nil || p.state == nil || p.state.snapshot == nil {
		return SnapshotHandle{}
	}
	snapshot := p.state.snapshot
	return SnapshotHandle{
		ID: snapshot.ID, Kind: snapshot.SnapshotKind(), ManifestHash: snapshot.ManifestDigest(),
		DiffHash: snapshot.PatchDigest(), ExpiresAt: snapshot.ExpiresAt,
	}
}

func (p *PreparedReview) DiscoveryTrace() DiscoveryTrace {
	if p == nil || p.state == nil {
		return DiscoveryTrace{}
	}
	return p.state.trace
}

func (s *Service) Prepare(ctx context.Context, input PrepareInput) (*PreparedReview, error) {
	if input.Mode == "" {
		input.Mode = reviewsession.ModePatch
	}
	if input.Mode != reviewsession.ModePatch && input.Mode != reviewsession.ModeInlinePatch &&
		input.Mode != reviewsession.ModeSecurityAudit {
		return nil, fmt.Errorf("agentic review: unsupported mode %q", input.Mode)
	}
	analysis, err := contextbuilder.NewPatchFacade().Analyze(contextbuilder.PatchAnalysisRequest{
		Diff: input.Patch, ExcludePaths: input.BuildInput.ExcludePaths,
	})
	if err != nil {
		return nil, fmt.Errorf("agentic review: prepare patch: %w", err)
	}
	filteredPatch := analysis.FilteredDiff
	var snapshot *workspacesnapshot.Snapshot
	switch input.Snapshot.Kind {
	case workspacesnapshot.KindPinnedGit:
		if input.Snapshot.WorkspacePath != "" {
			return nil, fmt.Errorf("agentic review: pinned snapshot cannot specify workspace path")
		}
		snapshot, err = s.snapshots.CreatePinned(ctx, workspacesnapshot.PinnedGitOptions{
			RepoPath: input.Snapshot.RepoPath, Ref: input.Snapshot.Ref,
			PatchRef: input.Snapshot.PatchRef, Patch: []byte(filteredPatch),
			Allowlist: append([]string(nil), input.Snapshot.Allowlist...), TTL: input.Snapshot.TTL,
		})
	case workspacesnapshot.KindMutableCopy:
		if input.Snapshot.RepoPath != "" || input.Snapshot.Ref != "" {
			return nil, fmt.Errorf("agentic review: mutable snapshot cannot specify git source")
		}
		snapshot, err = s.snapshots.CreateMutable(ctx, workspacesnapshot.MutableCopyOptions{
			WorkspacePath: input.Snapshot.WorkspacePath, PatchRef: input.Snapshot.PatchRef,
			Patch: []byte(filteredPatch), Allowlist: append([]string(nil), input.Snapshot.Allowlist...),
			TTL: input.Snapshot.TTL,
		})
	default:
		return nil, fmt.Errorf("agentic review: unsupported snapshot kind %q", input.Snapshot.Kind)
	}
	if err != nil {
		return nil, err
	}
	operationID, err := reviewsession.NewChatID()
	if err != nil {
		return nil, err
	}
	prepareLease, err := s.snapshots.Acquire(snapshot.ID, "prepare:"+operationID, input.Snapshot.TTL)
	if err != nil {
		return nil, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			s.snapshots.Release(prepareLease.ID)
		}
	}()
	discovered, err := s.discovery.Run(ctx, DiscoveryInput{
		Snapshot: snapshot, Patch: filteredPatch, ChangedFiles: analysis.ChangedFiles,
		BuildInput: input.BuildInput,
	})
	if err != nil {
		return nil, err
	}
	envelope := targetidentity.New(
		input.Target.RepoID, input.Target.RootID, input.Target.PatchEventID,
		input.Target.CanonicalRemoteIdentity, input.Target.BaseCommit, input.Target.TipCommit,
		input.Target.PreparedDiffSHA256, filteredPatch, discovered.Bundle.Content,
	)
	if err := envelope.VerifyMaterials(filteredPatch, discovered.Bundle.Content); err != nil {
		return nil, fmt.Errorf("agentic review: bind prepared target: %w", err)
	}
	prepared := &PreparedReview{state: &preparedState{
		serviceNonce: s.nonce, mode: input.Mode, snapshot: snapshot,
		bundle: cloneContextBundle(discovered.Bundle), patch: filteredPatch, envelope: envelope,
		trace: discovered.Trace, artifacts: append([]agenttools.SelectionArtifact(nil), discovered.Artifacts...),
		sessionCapable: discovered.SessionCapable, prepareLeaseID: prepareLease.ID,
	}}
	keepLease = true
	return prepared, nil
}

// ReleasePrepared releases the orchestration lease retained by Prepare. It is
// idempotent; active sessions retain their own independent snapshot leases.
func (s *Service) ReleasePrepared(prepared *PreparedReview) {
	if prepared == nil || prepared.state == nil || prepared.state.serviceNonce != s.nonce {
		return
	}
	state := prepared.state
	state.releaseOnce.Do(func() {
		if state.prepareLeaseID != "" {
			s.snapshots.Release(state.prepareLeaseID)
		}
	})
}

type ReviewOptions struct {
	ReviewerRoute                reviewengine.ModelRoute
	ReviewerSystemPromptOverride string
	AdditionalInstructions       string
	FewShot                      []string
	SkipWalkthrough              bool
	Ensemble                     *reviewengine.EnsembleConfig
}

func (s *Service) ReviewPrepared(ctx context.Context, prepared *PreparedReview, options ReviewOptions) (reviewengine.RunOutput, error) {
	state, err := s.validatePrepared(prepared)
	if err != nil {
		return reviewengine.RunOutput{}, err
	}
	operationID, err := reviewsession.NewChatID()
	if err != nil {
		return reviewengine.RunOutput{}, err
	}
	lease, err := s.snapshots.Acquire(state.snapshot.ID, "review:"+operationID, 0)
	if err != nil {
		return reviewengine.RunOutput{}, err
	}
	defer s.snapshots.Release(lease.ID)
	return s.runPrepared(ctx, state, options, reviewengine.ReviewerConversation{})
}

func (s *Service) runPrepared(ctx context.Context, state *preparedState, options ReviewOptions, conversation reviewengine.ReviewerConversation) (reviewengine.RunOutput, error) {
	scope := reviewengine.FindingScopePatch
	if state.mode == reviewsession.ModeSecurityAudit {
		scope = reviewengine.FindingScopeSnapshot
	}
	runInput := reviewengine.RunInput{
		ContextBundle: state.bundle.Content, PatchDiff: state.patch,
		TargetEnvelope: state.envelope, ChangedFiles: append([]string(nil), state.bundle.ChangedFiles...),
		FewShot: append([]string(nil), options.FewShot...), ReviewerRoute: options.ReviewerRoute,
		ReviewerSystemPromptOverride: options.ReviewerSystemPromptOverride,
		AdditionalInstructions:       options.AdditionalInstructions,
		TestCoverageGaps:             append([]string(nil), state.bundle.TestCoverageGaps...),
		SkipWalkthrough:              options.SkipWalkthrough, FindingScope: scope, Conversation: conversation,
	}
	if scope == reviewengine.FindingScopeSnapshot {
		runInput.SnapshotFindingValidator = func(ctx context.Context, finding reviewengine.Finding) error {
			path, err := normalizeSubmissionPath(finding.File)
			if err != nil {
				return err
			}
			content, err := state.snapshot.ReadFile(ctx, path)
			if err != nil {
				return err
			}
			lineCount := bytes.Count(content, []byte{'\n'}) + 1
			if len(content) == 0 {
				lineCount = 0
			}
			if finding.Line <= 0 || finding.Line > lineCount {
				return fmt.Errorf("%s:%d is outside file bounds", path, finding.Line)
			}
			return nil
		}
	}
	if options.Ensemble != nil && options.Ensemble.Enabled {
		return s.engine.RunEnsembleWithExecutors(ctx, runInput, *options.Ensemble,
			func(reviewengine.ModelRoute) reviewengine.ReviewerExecutor {
				reviewer, err := NewReviewer(ReviewerConfig{
					Client: s.client, Registry: s.registry, Counter: s.counter,
					Snapshot: state.snapshot, Scope: scope, Limits: s.reviewerLimits,
				})
				if err != nil {
					return &errorReviewer{err: err}
				}
				return reviewer
			})
	}
	reviewer, err := NewReviewer(ReviewerConfig{
		Client: s.client, Registry: s.registry, Counter: s.counter,
		Snapshot: state.snapshot, Scope: scope, Limits: s.reviewerLimits,
	})
	if err != nil {
		return reviewengine.RunOutput{}, err
	}
	return s.engine.RunWithExecutor(ctx, runInput, reviewer)
}

type errorReviewer struct{ err error }

func (e *errorReviewer) ExecuteReviewer(context.Context, reviewengine.ReviewerExecutionRequest) (reviewengine.ReviewerExecutionResult, error) {
	return reviewengine.ReviewerExecutionResult{}, e.err
}

type StartSessionInput struct {
	Prepared  *PreparedReview
	Owner     reviewsession.Owner
	RequestID string
	Options   ReviewOptions
	Lifetime  time.Duration
}

type SessionResult struct {
	ChatID  string
	Version int
	Output  reviewengine.RunOutput
	Replay  bool
}

func (s *Service) StartSession(ctx context.Context, input StartSessionInput) (SessionResult, error) {
	state, err := s.validatePrepared(input.Prepared)
	if err != nil {
		return SessionResult{}, err
	}
	if !state.sessionCapable {
		return SessionResult{}, ErrPreparedNotSessionCapable
	}
	if input.Options.Ensemble != nil && input.Options.Ensemble.Enabled {
		return SessionResult{}, ErrSessionEnsembleUnsupported
	}
	if err := input.Owner.Validate(); err != nil || strings.TrimSpace(input.RequestID) == "" {
		return SessionResult{}, fmt.Errorf("agentic review: session owner and request ID are required")
	}
	chatID, err := reviewsession.NewChatID()
	if err != nil {
		return SessionResult{}, err
	}
	lease, err := s.snapshots.Acquire(state.snapshot.ID, chatID, input.Lifetime)
	if err != nil {
		return SessionResult{}, err
	}
	expiresAt := lease.ExpiresAt
	artifacts := make([]reviewsession.Artifact, 0, len(state.artifacts))
	for i, artifact := range state.artifacts {
		artifacts = append(artifacts, reviewsession.Artifact{
			Ordinal: i, Kind: string(artifact.Kind), Path: artifact.Path,
			StartLine: artifact.StartLine, EndLine: artifact.EndLine,
			Hash: artifact.Hash, Mandatory: artifact.Mandatory,
		})
	}
	target, _ := json.Marshal(state.envelope)
	_, err = s.sessions.Create(ctx, reviewsession.CreateParams{
		ChatID: chatID, Owner: input.Owner, Mode: state.mode,
		Snapshot: reviewsession.Snapshot{
			ID: state.snapshot.ID, Kind: string(state.snapshot.SnapshotKind()),
			StoragePath: state.snapshot.StoragePath(), ManifestHash: state.snapshot.ManifestDigest(),
			DiffHash: state.snapshot.PatchDigest(), ExpiresAt: state.snapshot.ExpiresAt,
		},
		TargetEnvelope: target, BundleHash: targetidentity.SHA256(state.bundle.Content),
		LeaseID: lease.ID, Artifacts: artifacts, RequestID: input.RequestID, ExpiresAt: expiresAt,
	})
	if err != nil {
		s.snapshots.Release(lease.ID)
		return SessionResult{}, err
	}
	sink := &sessionTranscriptSink{store: s.sessions, chatID: chatID, requestID: input.RequestID}
	output, runErr := s.runPrepared(ctx, state, input.Options, reviewengine.ReviewerConversation{Sink: sink})
	if runErr != nil {
		_ = s.sessions.FailTurn(context.WithoutCancel(ctx), chatID, input.RequestID, runErr)
		s.snapshots.Release(lease.ID)
		return SessionResult{ChatID: chatID, Version: 0, Output: output}, runErr
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		_ = s.sessions.FailTurn(context.WithoutCancel(ctx), chatID, input.RequestID, err)
		s.snapshots.Release(lease.ID)
		return SessionResult{}, err
	}
	if err := s.sessions.CompleteTurn(ctx, chatID, input.RequestID, encoded); err != nil {
		s.snapshots.Release(lease.ID)
		return SessionResult{}, err
	}
	return SessionResult{ChatID: chatID, Version: 0, Output: output}, nil
}

type ContinueInput struct {
	ChatID          string
	Owner           reviewsession.Owner
	RequestID       string
	ExpectedVersion int
	Message         string
}

func (s *Service) Continue(ctx context.Context, input ContinueInput) (SessionResult, error) {
	loaded, err := s.sessions.LoadForContinuation(ctx, input.ChatID)
	if err != nil {
		return SessionResult{}, err
	}
	if loaded.Session.Owner != input.Owner {
		return SessionResult{}, reviewsession.ErrOwnerMismatch
	}
	snapshot, err := s.snapshots.Get(loaded.Session.Snapshot.ID)
	if errors.Is(err, workspacesnapshot.ErrNotFound) {
		snapshot, err = s.snapshots.Restore(ctx, loaded.Session.Snapshot.StoragePath,
			loaded.Session.Snapshot.ID, loaded.Session.Snapshot.ManifestHash, loaded.Session.Snapshot.DiffHash)
	}
	if err != nil {
		_ = s.sessions.MarkBroken(context.WithoutCancel(ctx), input.ChatID, err)
		return SessionResult{}, err
	}
	if snapshot.ManifestDigest() != loaded.Session.Snapshot.ManifestHash ||
		snapshot.PatchDigest() != loaded.Session.Snapshot.DiffHash || snapshot.Verify() != nil {
		err = workspacesnapshot.ErrHashMismatch
		_ = s.sessions.MarkBroken(context.WithoutCancel(ctx), input.ChatID, err)
		return SessionResult{}, err
	}
	lease, err := s.snapshots.Renew(loaded.Session.LeaseID, 0)
	if err != nil {
		lease, err = s.snapshots.Acquire(snapshot.ID, input.ChatID, 0)
		if err != nil {
			return SessionResult{}, err
		}
	}
	reservation, err := s.sessions.ReserveTurn(ctx, reviewsession.ReserveTurnParams{
		ChatID: input.ChatID, Owner: input.Owner, RequestID: input.RequestID,
		RequestText: input.Message, ExpectedVersion: input.ExpectedVersion,
		LeaseID: lease.ID, ExpiresAt: lease.ExpiresAt,
	})
	if err != nil {
		return SessionResult{}, err
	}
	if reservation.Replay {
		if reservation.Turn.Status == reviewsession.TurnFailed {
			return SessionResult{ChatID: input.ChatID, Version: reservation.Session.Version, Replay: true},
				fmt.Errorf("review session: stored request failed: %s", reservation.Turn.Error)
		}
		var output reviewengine.RunOutput
		if err := json.Unmarshal(reservation.Turn.Result, &output); err != nil {
			return SessionResult{}, err
		}
		return SessionResult{ChatID: input.ChatID, Version: reservation.Session.Version, Output: output, Replay: true}, nil
	}
	loaded, err = s.sessions.LoadForContinuation(ctx, input.ChatID)
	if err != nil {
		return SessionResult{}, s.failReserved(input, err)
	}
	analysis, err := contextbuilder.NewPatchFacade().Analyze(contextbuilder.PatchAnalysisRequest{Diff: string(snapshot.PatchContent())})
	if err != nil {
		return SessionResult{}, s.breakReserved(input, err)
	}
	persisted := make([]agenttools.SelectionArtifact, 0, len(loaded.Artifacts))
	for _, artifact := range loaded.Artifacts {
		persisted = append(persisted, agenttools.SelectionArtifact{
			Kind: agenttools.ArtifactKind(artifact.Kind), Path: artifact.Path,
			StartLine: artifact.StartLine, EndLine: artifact.EndLine,
			Hash: artifact.Hash, Mandatory: artifact.Mandatory,
		})
	}
	selection, err := agenttools.RestoreSelection(ctx, agenttools.SelectionConfig{
		Snapshot: snapshot, ChangedFiles: analysis.ChangedFiles, Counter: s.counter,
		TokenBudget: s.discovery.config.TokenBudget, Headroom: s.discovery.config.Headroom,
	}, persisted)
	if err != nil {
		return SessionResult{}, s.breakReserved(input, err)
	}
	bundle, ok := selection.Bundle()
	if !ok || targetidentity.SHA256(bundle.Content) != loaded.Session.BundleHash {
		return SessionResult{}, s.breakReserved(input, workspacesnapshot.ErrHashMismatch)
	}
	var envelope targetidentity.Envelope
	if err := json.Unmarshal(loaded.Session.TargetEnvelope, &envelope); err != nil {
		return SessionResult{}, s.breakReserved(input, err)
	}
	if err := envelope.VerifyMaterials(string(snapshot.PatchContent()), bundle.Content); err != nil {
		return SessionResult{}, s.breakReserved(input, err)
	}
	history, err := reviewsession.CompactHistory(loaded, s.counter, s.historyTokens)
	if err != nil {
		return SessionResult{}, s.failReserved(input, err)
	}
	state := &preparedState{
		serviceNonce: s.nonce, mode: loaded.Session.Mode, snapshot: snapshot,
		bundle: bundle, patch: string(snapshot.PatchContent()), envelope: envelope,
		artifacts: persisted, sessionCapable: true,
	}
	sink := &sessionTranscriptSink{store: s.sessions, chatID: input.ChatID, requestID: input.RequestID}
	output, runErr := s.runPrepared(ctx, state, ReviewOptions{}, reviewengine.ReviewerConversation{
		History: history, Message: input.Message, Sink: sink,
	})
	if runErr != nil {
		return SessionResult{ChatID: input.ChatID, Version: reservation.Session.Version, Output: output},
			s.failReserved(input, runErr)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return SessionResult{}, s.failReserved(input, err)
	}
	if err := s.sessions.CompleteTurn(ctx, input.ChatID, input.RequestID, encoded); err != nil {
		return SessionResult{}, err
	}
	return SessionResult{ChatID: input.ChatID, Version: reservation.Session.Version, Output: output}, nil
}

func (s *Service) ExpireSession(ctx context.Context, chatID string, owner reviewsession.Owner) error {
	loaded, err := s.sessions.LoadForContinuation(ctx, chatID)
	if err != nil {
		return err
	}
	if loaded.Session.Owner != owner {
		return reviewsession.ErrOwnerMismatch
	}
	leaseID, err := s.sessions.Expire(ctx, chatID)
	if err != nil {
		return err
	}
	s.snapshots.Release(leaseID)
	return nil
}

func (s *Service) validatePrepared(prepared *PreparedReview) (*preparedState, error) {
	if prepared == nil || prepared.state == nil || prepared.state.serviceNonce != s.nonce ||
		prepared.state.snapshot == nil || prepared.state.bundle.Content == "" {
		return nil, ErrInvalidPrepared
	}
	state := prepared.state
	if err := state.snapshot.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPrepared, err)
	}
	if err := state.envelope.VerifyMaterials(state.patch, state.bundle.Content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPrepared, err)
	}
	return state, nil
}

func (s *Service) failReserved(input ContinueInput, cause error) error {
	_ = s.sessions.FailTurn(context.Background(), input.ChatID, input.RequestID, cause)
	return cause
}

func (s *Service) breakReserved(input ContinueInput, cause error) error {
	_ = s.sessions.FailTurn(context.Background(), input.ChatID, input.RequestID, cause)
	_ = s.sessions.MarkBroken(context.Background(), input.ChatID, cause)
	return cause
}

type sessionTranscriptSink struct {
	store     reviewsession.Store
	chatID    string
	requestID string
}

func (s *sessionTranscriptSink) AppendReviewerMessages(ctx context.Context, messages []reviewengine.CompletionMessage) error {
	converted := make([]reviewsession.Message, 0, len(messages))
	for _, message := range messages {
		converted = append(converted, reviewsession.MessageFromCompletion(0, message))
	}
	return s.store.AppendMessages(ctx, s.chatID, s.requestID, converted)
}

func cloneContextBundle(bundle contextbuilder.ContextBundle) contextbuilder.ContextBundle {
	bundle.LayersUsed = append([]string(nil), bundle.LayersUsed...)
	bundle.LayersDropped = append([]string(nil), bundle.LayersDropped...)
	bundle.LayerStatuses = append([]contextbuilder.LayerStatus(nil), bundle.LayerStatuses...)
	bundle.ExcludedFiles = append([]string(nil), bundle.ExcludedFiles...)
	bundle.TestCoverageGaps = append([]string(nil), bundle.TestCoverageGaps...)
	bundle.ChangedFiles = append([]string(nil), bundle.ChangedFiles...)
	return bundle
}
