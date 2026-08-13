package agenticreview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"drydock/internal/agenttools"
	"drydock/internal/contextbuilder"
	"drydock/internal/reviewengine"
	"drydock/internal/workspacesnapshot"
)

type DiscoveryFallbackReason string

const (
	FallbackNone           DiscoveryFallbackReason = ""
	FallbackLoopExhaustion DiscoveryFallbackReason = "loop_exhaustion"
)

type BundleBuilder interface {
	Build(context.Context, contextbuilder.BuildInput) (contextbuilder.ContextBundle, error)
}

type DiscoveryConfig struct {
	Client      reviewengine.CompletionClient
	Registry    *agenttools.Registry
	Counter     contextbuilder.TokenCounter
	Builder     BundleBuilder
	Limits      LoopLimits
	TokenBudget int
	Headroom    float64
	Model       string
	BaseURL     string
	APIKey      string
	Temperature float64
}

type DiscoveryInput struct {
	Snapshot     *workspacesnapshot.Snapshot
	Patch        string
	ChangedFiles []string
	BuildInput   contextbuilder.BuildInput
}

type DiscoveryTrace struct {
	Loop           LoopTrace               `json:"loop"`
	FallbackReason DiscoveryFallbackReason `json:"fallback_reason,omitempty"`
	FallbackError  string                  `json:"fallback_error,omitempty"`
}

type DiscoveryResult struct {
	Bundle         contextbuilder.ContextBundle
	Trace          DiscoveryTrace
	Artifacts      []agenttools.SelectionArtifact
	SessionCapable bool
}

type Discovery struct {
	config DiscoveryConfig
}

func NewDiscovery(config DiscoveryConfig) (*Discovery, error) {
	if config.Client == nil || config.Registry == nil || config.Counter == nil {
		return nil, fmt.Errorf("agentic review: discovery client, registry, and counter are required")
	}
	if config.TokenBudget <= 0 {
		config.TokenBudget = contextbuilder.DefaultTokenBudget
	}
	if config.Headroom == 0 {
		config.Headroom = agenttools.DefaultTokenHeadroom
	}
	config.Limits = normalizeLoopLimits(config.Limits)
	return &Discovery{config: config}, nil
}

func (d *Discovery) Run(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if input.Snapshot == nil {
		return DiscoveryResult{}, fmt.Errorf("agentic review: discovery snapshot is required")
	}
	analysis, err := contextbuilder.NewPatchFacade().Analyze(contextbuilder.PatchAnalysisRequest{
		Diff: input.Patch, ExcludePaths: input.BuildInput.ExcludePaths,
	})
	if err != nil {
		return DiscoveryResult{}, fmt.Errorf("agentic review: analyze authoritative patch: %w", err)
	}
	if !bytes.Equal(input.Snapshot.PatchContent(), []byte(analysis.FilteredDiff)) {
		return DiscoveryResult{}, fmt.Errorf("agentic review: snapshot patch does not match authoritative filtered patch")
	}
	changedFiles := analysis.ChangedFiles
	if len(input.ChangedFiles) > 0 && !sameStrings(input.ChangedFiles, changedFiles) {
		return DiscoveryResult{}, fmt.Errorf("agentic review: supplied changed files disagree with authoritative patch")
	}

	selection, err := agenttools.NewSelection(agenttools.SelectionConfig{
		Snapshot: input.Snapshot, ChangedFiles: changedFiles, Counter: d.config.Counter,
		TokenBudget: d.config.TokenBudget, Headroom: d.config.Headroom,
	})
	if err != nil {
		return DiscoveryResult{}, err
	}
	scope := agenttools.NewScope("discovery:"+input.Snapshot.ID, input.Snapshot, agenttools.RoleContextDiscovery)
	scope.Selection = selection
	user := fmt.Sprintf(discoveryUserPrompt, strings.Join(changedFiles, "\n"),
		d.config.TokenBudget, d.config.Headroom*100, analysis.FilteredDiff)
	loopResult, loopErr := (&LoopRunner{Client: d.config.Client}).Run(ctx, LoopRequest{
		Completion: reviewengine.CompletionRequest{
			BaseURL: d.config.BaseURL, APIKey: d.config.APIKey, Model: d.config.Model,
			Temperature: d.config.Temperature,
			Messages: []reviewengine.CompletionMessage{
				{Role: reviewengine.MessageRoleSystem, Content: DiscoverySystemPrompt},
				{Role: reviewengine.MessageRoleUser, Content: user},
			},
		},
		Registry: d.config.Registry, Scope: scope, Selection: selection,
		Counter: d.config.Counter, Limits: d.config.Limits,
	})
	if loopErr == nil {
		artifacts, err := selection.Artifacts()
		if err != nil {
			return DiscoveryResult{Trace: DiscoveryTrace{Loop: loopResult.Trace}}, err
		}
		return DiscoveryResult{Bundle: loopResult.Bundle, Trace: DiscoveryTrace{Loop: loopResult.Trace},
			Artifacts: artifacts, SessionCapable: true}, nil
	}
	if !isExhaustion(loopErr) {
		return DiscoveryResult{Trace: DiscoveryTrace{Loop: loopResult.Trace}}, loopErr
	}
	trace := DiscoveryTrace{
		Loop: loopResult.Trace, FallbackReason: FallbackLoopExhaustion,
		FallbackError: loopErr.Error(),
	}
	if d.config.Builder == nil {
		return DiscoveryResult{Trace: trace}, fmt.Errorf("agentic review: discovery exhausted and deterministic builder is unavailable: %w", loopErr)
	}

	materialized, err := os.MkdirTemp("", "drydock-discovery-*")
	if err != nil {
		return DiscoveryResult{Trace: trace}, fmt.Errorf("agentic review: create fallback snapshot root: %w", err)
	}
	defer os.RemoveAll(materialized)
	if err := input.Snapshot.Materialize(ctx, materialized); err != nil {
		return DiscoveryResult{Trace: trace}, fmt.Errorf("agentic review: materialize fallback snapshot: %w", err)
	}
	buildInput := input.BuildInput
	buildInput.RepoPath = materialized
	buildInput.PatchEventContent = analysis.FilteredDiff
	buildInput.TokenBudgetOverride = d.config.TokenBudget
	fallback, err := d.config.Builder.Build(ctx, buildInput)
	if err != nil {
		return DiscoveryResult{Trace: trace}, fmt.Errorf("agentic review: deterministic exhaustion fallback: %w", err)
	}
	fallback.ChangedFiles = append([]string(nil), changedFiles...)
	gated, err := agenttools.GateBundle(fallback, d.config.Counter, d.config.TokenBudget, d.config.Headroom)
	if err != nil {
		return DiscoveryResult{Trace: trace}, fmt.Errorf("agentic review: deterministic exhaustion fallback gate: %w", err)
	}
	return DiscoveryResult{Bundle: gated, Trace: trace}, nil
}

func isExhaustion(err error) bool {
	return errors.Is(err, ErrTurnLimit) || errors.Is(err, ErrToolCallLimit) ||
		errors.Is(err, ErrTokenLimit) || errors.Is(err, ErrModelContext)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, value := range a {
		counts[value]++
	}
	for _, value := range b {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}
