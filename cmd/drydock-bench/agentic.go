package main

// Agentic mode: drydock's default review path (DRYDOCK_AGENTIC_REVIEW_FALLBACK
// unset). Each case freezes a pinned Git snapshot at its base commit, applies
// the case diff, runs the budgeted discovery tool loop, then the iterative
// tool-using reviewer, exactly as internal/pipeline/runner.go does. The
// service is assembled like cmd/drydock/main.go, with three bench-local
// substitutions: snapshots and sessions live in a temporary directory, the
// tool registry has no LSP bridge (code.references reports unavailable, as in
// a deployment without the lsp-bridge sidecar), and the LLM client is the
// metered client.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/agenticreview"
	"git.sharegap.net/cascadia/drydock/internal/agenttools"
	"git.sharegap.net/cascadia/drydock/internal/config"
	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
	"git.sharegap.net/cascadia/drydock/internal/db"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
	"git.sharegap.net/cascadia/drydock/internal/reviewsession"
	"git.sharegap.net/cascadia/drydock/internal/targetidentity"
	"git.sharegap.net/cascadia/drydock/internal/workspacesnapshot"
)

type agenticMeta struct {
	Repo    string `json:"repo"`
	Agentic struct {
		RepoDir string `json:"repo_dir"`
		Base    string `json:"base"`
	} `json:"agentic"`
}

type agenticRunner struct {
	svc       *agenticreview.Service
	reposRoot string
	store     *db.Store
	workDir   string
}

func newAgenticRunner(ctx context.Context, cfg config.Config, engine *reviewengine.Engine, client *meteredClient, reposRoot string) (*agenticRunner, error) {
	counter, err := contextbuilder.NewRequiredTiktokenCounter("cl100k_base")
	if err != nil {
		return nil, fmt.Errorf("agentic tokenizer: %w", err)
	}
	work, err := os.MkdirTemp("", "drydock-bench-agentic-")
	if err != nil {
		return nil, err
	}
	snapshots, err := workspacesnapshot.NewManager(workspacesnapshot.Config{
		StorageRoot: filepath.Join(work, "snapshots"), SnapshotTTL: 2 * time.Hour,
		LeaseTTL: time.Hour, SessionLifetime: time.Hour,
	})
	if err != nil {
		return nil, err
	}
	store, err := db.Open(ctx, "file:"+filepath.Join(work, "bench.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	sessions, err := reviewsession.NewSQLiteStore(store.DB(), nil)
	if err != nil {
		return nil, err
	}
	builderOpts := contextbuilder.NewBuilderOptions(contextbuilder.WithTokenBudget(cfg.ContextTokenBudget, cfg.AgenticTokenHeadroom))
	registry := agenttools.NewRegistry(agenttools.WithLayerFacade(contextbuilder.NewLayerFacade(builderOpts)))
	limits := func(turns, calls, tokens int) agenticreview.LoopLimits {
		return agenticreview.LoopLimits{MaxTurns: turns, MaxToolCalls: calls, MaxCumulativeTokens: tokens,
			MaxToolResultBytes: cfg.AgenticMaxToolResultBytes, MaxModelContext: cfg.AgenticMaxModelContext}
	}
	discovery, err := agenticreview.NewDiscovery(agenticreview.DiscoveryConfig{
		Client: client, Registry: registry, Counter: counter, Builder: contextbuilder.NewWithOptions(builderOpts),
		BaseURL: cfg.AgenticDiscoveryBaseURL, APIKey: cfg.EffectiveLLMAPIKey(cfg.AgenticDiscoveryAPIKey),
		Model: cfg.AgenticDiscoveryModel, Temperature: 0.1, Timeout: cfg.AgenticDiscoveryTimeout,
		Limits:      limits(cfg.AgenticDiscoveryMaxTurns, cfg.AgenticDiscoveryMaxToolCalls, cfg.AgenticDiscoveryMaxCumulativeTokens),
		TokenBudget: cfg.AgenticPackageTokenBudget, Headroom: cfg.AgenticTokenHeadroom,
	})
	if err != nil {
		return nil, err
	}
	svc, err := agenticreview.NewService(agenticreview.ServiceConfig{
		Snapshots: snapshots, Sessions: sessions, Discovery: discovery, Engine: engine, Client: client,
		Registry: registry, Counter: counter,
		ReviewerLimits: limits(cfg.AgenticReviewerMaxTurns, cfg.AgenticReviewerMaxToolCalls, cfg.AgenticReviewerMaxCumulativeTokens),
		HistoryTokens:  cfg.AgenticHistoryTokenBudget,
	})
	if err != nil {
		return nil, err
	}
	return &agenticRunner{svc: svc, reposRoot: reposRoot, store: store, workDir: work}, nil
}

func (a *agenticRunner) Close() {
	if a.store != nil {
		_ = a.store.Close()
	}
	_ = os.RemoveAll(a.workDir)
}

func (a *agenticRunner) run(ctx context.Context, c benchCase, skipWalk bool) (reviewengine.RunOutput, error) {
	var meta agenticMeta
	if err := json.Unmarshal(c.Meta, &meta); err != nil || meta.Agentic.RepoDir == "" || meta.Agentic.Base == "" {
		return reviewengine.RunOutput{}, fmt.Errorf("case %s has no bench_meta.agentic {repo_dir, base}", c.CaseID)
	}
	repoPath := filepath.Join(a.reposRoot, meta.Agentic.RepoDir)
	repoID := meta.Repo
	if strings.TrimSpace(repoID) == "" {
		repoID = meta.Agentic.RepoDir
	}
	// Target identity is required by Prepare; the bench has no NIP-34 remote,
	// so the clone directory stands in for the canonical remote.
	remote := "bench:" + meta.Agentic.RepoDir
	prepared, err := a.svc.Prepare(ctx, agenticreview.PrepareInput{
		Mode: reviewsession.ModePatch,
		Snapshot: agenticreview.SnapshotSpec{
			Kind: workspacesnapshot.KindPinnedGit, RepoPath: repoPath, Ref: meta.Agentic.Base,
			PatchRef: c.CaseID, Allowlist: c.ChangedFiles, TTL: time.Hour,
		},
		Patch:      c.PatchDiff,
		BuildInput: contextbuilder.BuildInput{PatchEventContent: c.PatchDiff, RepoID: repoID},
		Target: agenticreview.TargetInput{
			RepoID: repoID, RootID: c.CaseID, PatchEventID: c.CaseID, BaseCommit: meta.Agentic.Base,
			CanonicalRemoteIdentity: remote,
			PreparedDiffSHA256:      targetidentity.SHA256(c.PatchDiff),
		},
	})
	if err != nil {
		return reviewengine.RunOutput{}, fmt.Errorf("discovery: %w", err)
	}
	defer a.svc.ReleasePrepared(prepared)
	out, err := a.svc.ReviewPrepared(ctx, prepared, agenticreview.ReviewOptions{SkipWalkthrough: skipWalk})
	if err != nil {
		return out, fmt.Errorf("reviewer: %w", err)
	}
	return out, nil
}

// --- conversational (tool-calling) completions -------------------------------

func completionStage(messages []reviewengine.CompletionMessage) string {
	if len(messages) == 0 {
		return "complete"
	}
	switch first := messages[0].Content; {
	case strings.HasPrefix(first, "You are Drydock's context discovery agent."):
		return "discovery"
	case strings.HasPrefix(first, "You are Drydock's iterative code reviewer."):
		return "reviewer"
	default:
		return "complete"
	}
}

// Complete mirrors reviewengine.OpenAICompatClient.Complete (same request
// body) and records each loop turn like ChatCompletion records single calls.
func (c *meteredClient) Complete(ctx context.Context, req reviewengine.CompletionRequest) (reviewengine.CompletionResult, error) {
	rec := callRecord{Stage: completionStage(req.Messages), Model: req.Model, ProdTimeoutMS: req.Timeout.Milliseconds()}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == reviewengine.MessageRoleTool {
		// The tool result this turn responds to (e.g. a review.submit rejection).
		rec.LastToolResult = truncate(req.Messages[n-1].Content, 300)
	}
	if suffix := c.promptSuffix[rec.Stage]; suffix != "" && len(req.Messages) > 0 {
		// The engine-owned policy is the tail of the system message; appending
		// here equals adopting the prompt change in DefaultReviewerSystemPrompt.
		req.Messages = append([]reviewengine.CompletionMessage(nil), req.Messages...)
		req.Messages[0].Content += suffix
	}
	start := time.Now()
	result, err := c.complete(ctx, req, &rec)
	for attempt := 1; err != nil && attempt <= c.retries && retryable(err); attempt++ {
		rec.Retries++
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
		start = time.Now()
		result, err = c.complete(ctx, req, &rec)
	}
	rec.WallMS = time.Since(start).Milliseconds()
	if err != nil {
		rec.Error = truncate(err.Error(), 400)
	}
	if r, ok := ctx.Value(recorderKey{}).(*recorder); ok {
		r.add(rec)
	}
	return result, err
}

func (c *meteredClient) complete(ctx context.Context, req reviewengine.CompletionRequest, rec *callRecord) (reviewengine.CompletionResult, error) {
	if strings.TrimSpace(req.BaseURL) == "" {
		return reviewengine.CompletionResult{}, fmt.Errorf("no endpoint configured for model %q (stage %s)", req.Model, rec.Stage)
	}
	tools := make([]map[string]any, 0, len(req.Tools))
	for _, tool := range req.Tools {
		params := tool.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": tool.Name, "description": tool.Description, "parameters": params}})
	}
	payload := map[string]any{"model": req.Model, "messages": req.Messages, "temperature": req.Temperature}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	if c.maxTokens > 0 {
		payload["max_tokens"] = c.maxTokens
	}
	for k, v := range c.extra {
		if _, reserved := payload[k]; !reserved || k == "max_tokens" {
			payload[k] = v
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return reviewengine.CompletionResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return reviewengine.CompletionResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	res, err := c.http.Do(httpReq)
	if err != nil {
		return reviewengine.CompletionResult{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return reviewengine.CompletionResult{}, &reviewengine.LLMHTTPError{StatusCode: res.StatusCode, Status: res.Status, Body: string(b)}
	}
	var decoded struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Choices  []struct {
			Message      reviewengine.CompletionMessage `json:"message"`
			FinishReason string                         `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			TotalTokens      int     `json:"total_tokens"`
			Cost             float64 `json:"cost"`
		} `json:"usage"`
		Timings *struct {
			PromptN     int     `json:"prompt_n"`
			PromptMS    float64 `json:"prompt_ms"`
			PredictedN  int     `json:"predicted_n"`
			PredictedMS float64 `json:"predicted_ms"`
		} `json:"timings"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return reviewengine.CompletionResult{}, fmt.Errorf("decode llm response: %w", err)
	}
	rec.ServedModel, rec.Provider = decoded.Model, decoded.Provider
	rec.PromptTokens, rec.CompletionTokens = decoded.Usage.PromptTokens, decoded.Usage.CompletionTokens
	rec.CostUSD += decoded.Usage.Cost
	if t := decoded.Timings; t != nil {
		rec.PromptN, rec.PromptMS, rec.PredictedN, rec.PredictedMS = t.PromptN, t.PromptMS, t.PredictedN, t.PredictedMS
	}
	if len(decoded.Choices) == 0 {
		return reviewengine.CompletionResult{}, fmt.Errorf("%w (model=%s)", errNoChoices, req.Model)
	}
	choice := decoded.Choices[0]
	rec.FinishReason = choice.FinishReason
	rec.ToolCalls = len(choice.Message.ToolCalls)
	for _, call := range choice.Message.ToolCalls {
		rec.ToolNames = append(rec.ToolNames, call.Function.Name)
	}
	return reviewengine.CompletionResult{
		Message: choice.Message, FinishReason: choice.FinishReason,
		Usage: reviewengine.CompletionUsage{PromptTokens: decoded.Usage.PromptTokens, CompletionTokens: decoded.Usage.CompletionTokens,
			TotalTokens: decoded.Usage.TotalTokens, CostUSD: decoded.Usage.Cost},
		Model: strings.TrimSpace(decoded.Model),
	}, nil
}

var _ reviewengine.CompletionClient = (*meteredClient)(nil)
