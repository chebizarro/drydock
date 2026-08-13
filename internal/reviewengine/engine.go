package reviewengine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"drydock/internal/targetidentity"
)

type ModelEndpoint struct {
	BaseURL string
	APIKey  string
	Model   string
}

type Config struct {
	Planner      ModelEndpoint
	Coder32B     ModelEndpoint
	LLM70B       ModelEndpoint
	Coder14B     ModelEndpoint
	Sec70B       ModelEndpoint
	SecClassify  ModelEndpoint
	SecLocalize  ModelEndpoint
	PlannerTemp  float64
	ReviewerTemp float64
}

type RunInput struct {
	ContextBundle  string
	PatchDiff      string
	TargetEnvelope targetidentity.Envelope
	ChangedFiles   []string
	FewShot        []string
	// ReviewerRoute, when set, overrides the planner-selected reviewer route.
	ReviewerRoute ModelRoute
	// ReviewerSystemPromptOverride, if non-empty, replaces the default base
	// reviewer system prompt. Checklist, security preamble, and few-shot
	// examples are still appended.
	ReviewerSystemPromptOverride string
	// AdditionalInstructions contains repo-specific instructions that are
	// appended to the reviewer system prompt without replacing the base.
	AdditionalInstructions string
	// TestCoverageGaps lists modified symbols that lack test references.
	// When non-empty, an extra checklist item is appended reminding the
	// reviewer to consider flagging absent test coverage.
	TestCoverageGaps []string
	// SkipWalkthrough disables the walkthrough generation step.
	SkipWalkthrough bool
	// FindingScope defaults to patch-only validation. Snapshot scope is accepted
	// only from an executor that explicitly proves it validated that scope.
	FindingScope             FindingScope
	Conversation             ReviewerConversation
	SnapshotFindingValidator func(context.Context, Finding) error
}

type RunOutput struct {
	Planner PlannerOutput
	Review  ReviewerOutput
	Route   ModelRoute
	// ServedModel is the model identifier the reviewer endpoint reported
	// serving for this specific review. Empty when the provider omitted it;
	// callers should fall back to ModelForRoute(Route).
	ServedModel        string
	Checklist          []string
	Walkthrough        WalkthroughOutput
	WalkthroughStatus  StepStatus
	ReviewerTrace      ReviewerTrace
	EnsembleStatus     EnsembleStatus
	ReviewerTranscript []CompletionMessage
}

type Engine struct {
	cfg      Config
	client   LLMClient
	logger   *slog.Logger
	identity atomic.Pointer[ModelIdentity]
}

func New(cfg Config, client LLMClient, logger *slog.Logger) *Engine {
	return &Engine{cfg: cfg, client: client, logger: logger}
}

type reviewerPreparation struct {
	planner   PlannerOutput
	checklist []string
	system    string
	user      string
}

func (e *Engine) prepareReviewer(ctx context.Context, in RunInput) (reviewerPreparation, error) {
	planner, err := e.completeStructured(ctx, ChatRequest{
		BaseURL:     e.cfg.Planner.BaseURL,
		APIKey:      e.cfg.Planner.APIKey,
		Model:       e.cfg.Planner.Model,
		Temperature: e.cfg.PlannerTemp,
		System:      plannerSystemPrompt(),
		User:        plannerUserPrompt(in.ContextBundle, in.ChangedFiles),
	}, "planner", ParsePlannerOutput)
	if err != nil {
		return reviewerPreparation{}, err
	}

	checklist := BuildChecklist(in.ChangedFiles)
	if len(in.TestCoverageGaps) > 0 {
		checklist = append(checklist,
			fmt.Sprintf("Missing test coverage: symbols %s have no test references — consider flagging as a finding",
				strings.Join(in.TestCoverageGaps, ", ")))
	}
	return reviewerPreparation{
		planner:   planner,
		checklist: checklist,
		system: reviewerSystemPrompt(
			in.ReviewerSystemPromptOverride,
			in.AdditionalInstructions,
			checklist,
			IsSecuritySensitive(in.ChangedFiles),
			in.FewShot,
		),
		user: reviewerUserPrompt(in.ContextBundle, planner),
	}, nil
}

// Run preserves the legacy single-shot reviewer behavior through the
// ReviewerExecutor seam.
func (e *Engine) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	return e.RunWithExecutor(ctx, in, e.singleShotExecutor())
}

// RunWithExecutor runs planning and prompt assembly, delegates only the
// reviewer stage, then applies engine-owned finding filtering and walkthrough.
func (e *Engine) RunWithExecutor(ctx context.Context, in RunInput, executor ReviewerExecutor) (RunOutput, error) {
	if executor == nil {
		return RunOutput{}, fmt.Errorf("review engine: reviewer executor is required")
	}
	scope := in.FindingScope
	if scope == "" {
		scope = FindingScopePatch
	}
	if scope != FindingScopePatch && scope != FindingScopeSnapshot {
		return RunOutput{}, fmt.Errorf("review engine: unsupported finding scope %q", scope)
	}
	prepared, err := e.prepareReviewer(ctx, in)
	if err != nil {
		return RunOutput{}, err
	}

	reviewerRoute := prepared.planner.ModelRoute
	if in.ReviewerRoute != "" {
		reviewerRoute = in.ReviewerRoute
	}
	endpoint, err := e.routeEndpoint(reviewerRoute)
	if err != nil {
		return RunOutput{}, err
	}
	executed, err := executor.ExecuteReviewer(ctx, ReviewerExecutionRequest{
		Route: reviewerRoute, Endpoint: endpoint, Temperature: e.cfg.ReviewerTemp,
		System: prepared.system, User: prepared.user, Label: "reviewer",
		ContextBundle: in.ContextBundle, PatchDiff: in.PatchDiff,
		ChangedFiles:   append([]string(nil), in.ChangedFiles...),
		TargetEnvelope: in.TargetEnvelope, FindingScope: scope,
		Conversation: ReviewerConversation{History: cloneCompletionMessages(in.Conversation.History), Message: in.Conversation.Message, Sink: in.Conversation.Sink},
	})
	if err != nil {
		return RunOutput{ReviewerTrace: executed.Trace, ReviewerTranscript: cloneCompletionMessages(executed.Transcript)}, err
	}
	rawTranscript := cloneCompletionMessages(executed.Transcript)
	executed, err = normalizeReviewerExecution(executed, scope)
	if err != nil {
		return RunOutput{ReviewerTranscript: rawTranscript}, err
	}
	if scope == FindingScopePatch {
		executed.Review.Findings, err = filterFindingsToChangedFiles(
			executed.Review.Findings, in.ChangedFiles, in.TargetEnvelope,
			in.PatchDiff, in.ContextBundle, e.logger, "reviewer")
	} else {
		err = in.TargetEnvelope.VerifyMaterials(in.PatchDiff, in.ContextBundle)
		if err == nil && in.SnapshotFindingValidator == nil {
			err = fmt.Errorf("review engine: snapshot finding validator is required")
		}
		if err == nil {
			for _, finding := range executed.Review.Findings {
				if validateErr := in.SnapshotFindingValidator(ctx, finding); validateErr != nil {
					err = fmt.Errorf("review engine: validate snapshot finding: %w", validateErr)
					break
				}
			}
		}
	}
	if err != nil {
		return RunOutput{}, err
	}

	walkthrough, walkthroughStatus := e.generateWalkthrough(ctx, in)
	if e.logger != nil {
		e.logger.Info("review engine completed",
			"route", reviewerRoute,
			"findings", len(executed.Review.Findings),
			"checklist_items", len(prepared.checklist),
			"walkthrough_status", walkthroughStatus.State,
			"has_walkthrough", walkthrough.Walkthrough != "",
		)
	}
	return RunOutput{
		Planner:            prepared.planner,
		Review:             executed.Review,
		Route:              reviewerRoute,
		ServedModel:        executed.ServedModel,
		Checklist:          prepared.checklist,
		Walkthrough:        walkthrough,
		WalkthroughStatus:  walkthroughStatus,
		ReviewerTrace:      executed.Trace,
		ReviewerTranscript: cloneCompletionMessages(executed.Transcript),
	}, nil
}

func (e *Engine) generateWalkthrough(ctx context.Context, in RunInput) (WalkthroughOutput, StepStatus) {
	if in.SkipWalkthrough {
		return WalkthroughOutput{}, StepStatus{State: StepStateSkipped}
	}

	walkthrough, err := e.completeStructuredWalkthrough(ctx, ChatRequest{
		BaseURL:     e.cfg.Planner.BaseURL,
		APIKey:      e.cfg.Planner.APIKey,
		Model:       e.cfg.Planner.Model,
		Temperature: e.cfg.PlannerTemp,
		System:      walkthroughSystemPrompt(),
		User:        walkthroughUserPrompt(in.ContextBundle, in.ChangedFiles),
	}, "walkthrough")
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("walkthrough failed after repair attempts, continuing with failed status", "error", err)
		}
		return WalkthroughOutput{}, StepStatus{State: StepStateFailed, Error: err.Error()}
	}
	// The walkthrough prompt asks for changed files, but the model sees
	// contextual layers too — only summaries for deterministically changed
	// files are trustworthy.
	walkthrough = filterWalkthroughToChangedFiles(walkthrough, in.ChangedFiles, e.logger)
	return walkthrough, StepStatus{State: StepStateSucceeded}
}

// UseModelIdentity attaches a served-model registry. When set, ModelForRoute
// prefers the model identifier actually observed from the endpoint over the
// configured deployment name. Safe to call concurrently with ModelForRoute,
// though it is intended to be wired once at startup.
func (e *Engine) UseModelIdentity(mi *ModelIdentity) {
	e.identity.Store(mi)
}

// ModelForRoute returns the model identifier for the given route: the served
// model observed from the route's endpoint when known, otherwise the
// configured endpoint model, otherwise the route alias.
func (e *Engine) ModelForRoute(route ModelRoute) string {
	endpoint, err := e.routeEndpoint(route)
	if err != nil || strings.TrimSpace(endpoint.Model) == "" {
		return string(route)
	}
	if identity := e.identity.Load(); identity != nil {
		return identity.Resolve(endpoint.BaseURL, endpoint.APIKey, endpoint.Model)
	}
	return endpoint.Model
}

func (e *Engine) routeEndpoint(route ModelRoute) (ModelEndpoint, error) {
	switch route {
	case RouteCoder32B:
		return e.cfg.Coder32B, nil
	case RouteLLM70B:
		return e.cfg.LLM70B, nil
	case RouteCoder14B:
		return e.cfg.Coder14B, nil
	case RouteSec70B:
		if strings.TrimSpace(e.cfg.Sec70B.BaseURL) == "" {
			return e.cfg.LLM70B, nil
		}
		return e.cfg.Sec70B, nil
	case RouteSecClassify:
		return e.cfg.SecClassify, nil
	case RouteSecLocalize:
		return e.cfg.SecLocalize, nil
	default:
		return ModelEndpoint{}, fmt.Errorf("unsupported model route %q", route)
	}
}
