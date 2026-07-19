package reviewengine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
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
	PlannerTemp  float64
	ReviewerTemp float64
}

type RunInput struct {
	ContextBundle string
	ChangedFiles  []string
	FewShot       []string
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
}

type RunOutput struct {
	Planner           PlannerOutput
	Review            ReviewerOutput
	Route             ModelRoute
	// ServedModel is the model identifier the reviewer endpoint reported
	// serving for this specific review. Empty when the provider omitted it;
	// callers should fall back to ModelForRoute(Route).
	ServedModel       string
	Checklist         []string
	Walkthrough       WalkthroughOutput
	WalkthroughStatus StepStatus
	EnsembleStatus    EnsembleStatus
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

func (e *Engine) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	planner, err := e.completeStructured(ctx, ChatRequest{
		BaseURL:     e.cfg.Planner.BaseURL,
		APIKey:      e.cfg.Planner.APIKey,
		Model:       e.cfg.Planner.Model,
		Temperature: e.cfg.PlannerTemp,
		System:      plannerSystemPrompt(),
		User:        plannerUserPrompt(in.ContextBundle, in.ChangedFiles),
	}, "planner", ParsePlannerOutput)
	if err != nil {
		return RunOutput{}, err
	}

	checklist := BuildChecklist(in.ChangedFiles)
	if len(in.TestCoverageGaps) > 0 {
		checklist = append(checklist,
			fmt.Sprintf("Missing test coverage: symbols %s have no test references — consider flagging as a finding",
				strings.Join(in.TestCoverageGaps, ", ")))
	}
	system := reviewerSystemPrompt(in.ReviewerSystemPromptOverride, in.AdditionalInstructions, checklist, IsSecuritySensitive(in.ChangedFiles), in.FewShot)
	user := reviewerUserPrompt(in.ContextBundle, planner)

	endpoint, err := e.routeEndpoint(planner.ModelRoute)
	if err != nil {
		return RunOutput{}, err
	}
	review, servedModel, err := e.completeStructuredReviewer(ctx, ChatRequest{
		BaseURL:     endpoint.BaseURL,
		APIKey:      endpoint.APIKey,
		Model:       endpoint.Model,
		Temperature: e.cfg.ReviewerTemp,
		System:      system,
		User:        user,
	}, "reviewer")
	if err != nil {
		return RunOutput{}, err
	}
	review.Findings = filterFindingsToChangedFiles(review.Findings, in.ChangedFiles, e.logger, "reviewer")

	// Generate walkthrough (using planner model — lightweight 14B)
	walkthrough, walkthroughStatus := e.generateWalkthrough(ctx, in)

	e.logger.Info("review engine completed", "route", planner.ModelRoute, "findings", len(review.Findings), "checklist_items", len(checklist), "walkthrough_status", walkthroughStatus.State, "has_walkthrough", walkthrough.Walkthrough != "")
	return RunOutput{
		Planner:           planner,
		Review:            review,
		Route:             planner.ModelRoute,
		ServedModel:       servedModel,
		Checklist:         checklist,
		Walkthrough:       walkthrough,
		WalkthroughStatus: walkthroughStatus,
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
	return e.identity.Load().Resolve(endpoint.BaseURL, endpoint.APIKey, endpoint.Model)
}

func (e *Engine) routeEndpoint(route ModelRoute) (ModelEndpoint, error) {
	switch route {
	case RouteCoder32B:
		return e.cfg.Coder32B, nil
	case RouteLLM70B:
		return e.cfg.LLM70B, nil
	case RouteCoder14B:
		return e.cfg.Coder14B, nil
	default:
		return ModelEndpoint{}, fmt.Errorf("unsupported model route %q", route)
	}
}
