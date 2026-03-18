package reviewengine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

type ModelEndpoint struct {
	BaseURL string
	APIKey  string
	Model   string
}

type Config struct {
	Planner       ModelEndpoint
	Coder32B      ModelEndpoint
	LLM70B        ModelEndpoint
	Coder14B      ModelEndpoint
	PlannerTemp   float64
	ReviewerTemp  float64
}

type RunInput struct {
	ContextBundle string
	ChangedFiles  []string
	FewShot       []string
}

type RunOutput struct {
	Planner  PlannerOutput
	Review   ReviewerOutput
	Route    ModelRoute
	Checklist []string
}

type Engine struct {
	cfg    Config
	client LLMClient
	logger *slog.Logger
}

func New(cfg Config, client LLMClient, logger *slog.Logger) *Engine {
	return &Engine{cfg: cfg, client: client, logger: logger}
}

func (e *Engine) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	plannerRaw, err := e.client.ChatCompletion(ctx, ChatRequest{
		BaseURL:     e.cfg.Planner.BaseURL,
		APIKey:      e.cfg.Planner.APIKey,
		Model:       e.cfg.Planner.Model,
		Temperature: e.cfg.PlannerTemp,
		System:      plannerSystemPrompt(),
		User:        plannerUserPrompt(in.ContextBundle, in.ChangedFiles),
	})
	if err != nil {
		return RunOutput{}, fmt.Errorf("planner completion: %w", err)
	}
	planner, err := ParsePlannerOutput(extractJSON(plannerRaw))
	if err != nil {
		return RunOutput{}, fmt.Errorf("planner output invalid: %w", err)
	}

	checklist := BuildChecklist(in.ChangedFiles)
	system := reviewerSystemPrompt(checklist, IsSecuritySensitive(in.ChangedFiles), in.FewShot)
	user := reviewerUserPrompt(in.ContextBundle, planner)

	endpoint, err := e.routeEndpoint(planner.ModelRoute)
	if err != nil {
		return RunOutput{}, err
	}
	reviewerRaw, err := e.client.ChatCompletion(ctx, ChatRequest{
		BaseURL:     endpoint.BaseURL,
		APIKey:      endpoint.APIKey,
		Model:       endpoint.Model,
		Temperature: e.cfg.ReviewerTemp,
		System:      system,
		User:        user,
	})
	if err != nil {
		return RunOutput{}, fmt.Errorf("reviewer completion: %w", err)
	}
	review, err := ParseReviewerOutput(extractJSON(reviewerRaw))
	if err != nil {
		return RunOutput{}, fmt.Errorf("reviewer output invalid: %w", err)
	}

	e.logger.Info("review engine completed", "route", planner.ModelRoute, "findings", len(review.Findings), "checklist_items", len(checklist))
	return RunOutput{
		Planner:  planner,
		Review:   review,
		Route:    planner.ModelRoute,
		Checklist: checklist,
	}, nil
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

func extractJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		return raw[start : end+1]
	}
	return raw
}

