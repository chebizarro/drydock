// Command drydock-bench runs the production review engine (planner →
// reviewer → walkthrough) over an eval dataset and records, per case, the
// graded findings plus per-call latency and token accounting. It exists to
// compare model→role assignments on accuracy AND throughput; unlike
// drydock-eval it keeps going when a case fails (a timeout or unrepairable
// JSON is a measured outcome, not a reason to abort) and it never writes to
// the drydock database.
//
// Endpoints come from the same DRYDOCK_*_BASE_URL / DRYDOCK_*_MODEL variables
// as production. Output is JSONL: one header line, then one line per case.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"git.sharegap.net/cascadia/drydock/internal/config"
	"git.sharegap.net/cascadia/drydock/internal/eval"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// productionCallTimeout mirrors NewOpenAICompatClient's HTTP timeout. The
// bench runs with a longer limit so accuracy is measurable on slow hardware,
// and reports how many calls would have exceeded the production limit.
const productionCallTimeout = 120 * time.Second

type benchCase struct {
	eval.PatchCase
	Meta json.RawMessage `json:"bench_meta,omitempty"`
}

type benchDataset struct {
	ID        string      `json:"id"`
	ReposRoot string      `json:"repos_root,omitempty"` // agentic datasets: where case repos live
	Cases     []benchCase `json:"cases"`
}

type callRecord struct {
	Stage            string `json:"stage"`
	Model            string `json:"model"`
	ServedModel      string `json:"served_model,omitempty"`
	WallMS           int64  `json:"wall_ms"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	ReasoningChars   int    `json:"reasoning_chars,omitempty"`
	// llama.cpp timings. PromptN counts only uncached prompt tokens, which is
	// what PromptMS measured; usage.prompt_tokens includes cache hits.
	PromptN        int      `json:"prompt_n,omitempty"`
	PromptMS       float64  `json:"prompt_ms,omitempty"`
	PredictedN     int      `json:"predicted_n,omitempty"`
	PredictedMS    float64  `json:"predicted_ms,omitempty"`
	FinishReason   string   `json:"finish_reason,omitempty"`
	ToolCalls      int      `json:"tool_calls,omitempty"` // agentic loop turns
	ToolNames      []string `json:"tool_names,omitempty"`
	LastToolResult string   `json:"last_tool_result,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	CostUSD        float64  `json:"cost_usd,omitempty"`
	Retries        int      `json:"retries,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type caseResult struct {
	Type        string                 `json:"type"`
	CaseID      string                 `json:"case_id"`
	Meta        json.RawMessage        `json:"bench_meta,omitempty"`
	OK          bool                   `json:"ok"`
	Error       string                 `json:"error,omitempty"`
	FailedStage string                 `json:"failed_stage,omitempty"`
	Route       string                 `json:"route,omitempty"`
	ChangeType  string                 `json:"change_type,omitempty"`
	Walkthrough string                 `json:"walkthrough_status,omitempty"`
	Findings    []reviewengine.Finding `json:"findings"`
	Expected    []eval.ExpectedFinding `json:"expected"`
	Score       eval.CaseScore         `json:"score"`
	LenientTP   int                    `json:"lenient_tp"`
	// Detected: a finding on the planted file that either matches strictly
	// (category + line within tolerance) or quotes the planted code anywhere.
	// Line proximity alone is not enough: an unrelated finding two lines away
	// would count. Production anchors findings by line, so strict Score stays
	// the headline; Detected separates "found it but mis-counted diff lines"
	// from "missed it".
	Detected       int          `json:"detected"`
	LineErrors     []int        `json:"line_errors,omitempty"`
	WallMS         int64        `json:"wall_ms"`
	OverProdLimit  int          `json:"calls_over_prod_timeout"`
	RepairAttempts int          `json:"repair_attempts"`
	Calls          []callRecord `json:"calls"`
}

type header struct {
	Type          string            `json:"type"`
	Label         string            `json:"label"`
	DatasetID     string            `json:"dataset_id"`
	DatasetSHA256 string            `json:"dataset_sha256"`
	DatasetCases  int               `json:"dataset_cases"`
	StartedAt     time.Time         `json:"started_at"`
	PinnedRoute   string            `json:"pinned_route,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	ExtraBody     string            `json:"extra_body,omitempty"`
	PromptVariant string            `json:"prompt_variant,omitempty"`
	Agentic       bool              `json:"agentic,omitempty"`
	Retries       int               `json:"retries,omitempty"`
	CallTimeoutS  float64           `json:"call_timeout_s"`
	LineTolerance int               `json:"line_tolerance"`
	Endpoints     map[string]string `json:"endpoints"`
	Notes         string            `json:"notes,omitempty"`
}

func main() {
	var (
		datasetPath   = flag.String("dataset", "", "eval dataset JSON (drydock-eval format, optional bench_meta per case)")
		outPath       = flag.String("out", "", "JSONL output path (appended; existing case_ids are skipped)")
		label         = flag.String("label", "", "run label recorded in the header, e.g. qwen3-coder-30b-a3b@Q4_K_M")
		notes         = flag.String("notes", "", "free-form notes recorded in the header (serving flags, draft model, quant)")
		route         = flag.String("route", "", "pin the reviewer route (coder32b|llm70b|coder14b|sec70b); empty uses the planner's choice")
		callTimeout   = flag.Duration("call-timeout", 15*time.Minute, "per-LLM-call HTTP timeout")
		caseTimeout   = flag.Duration("case-timeout", 45*time.Minute, "whole-case timeout")
		maxCases      = flag.Int("max-cases", 0, "stop after N cases (0 = all)")
		skipWalk      = flag.Bool("skip-walkthrough", false, "skip the walkthrough stage")
		retryErrors   = flag.Bool("retry-errors", false, "re-run cases whose previous result is an error")
		lineTolerance = flag.Int("line-tolerance", eval.DefaultLineTolerance, "line tolerance for matching")
		maxTokens     = flag.Int("max-tokens", 0, "per-call max_tokens cap; 0 sends none, like production (runaway generations then run until the context fills)")
		extraBody     = flag.String("extra-body", "", `JSON object merged into every request body, e.g. '{"reasoning":{"enabled":false},"usage":{"include":true}}'`)
		retries       = flag.Int("retries", 0, "retry HTTP 429/5xx, connection errors and empty choices this many times (hosted APIs); timeouts are never retried")
		workers       = flag.Int("workers", 1, "cases run concurrently (keep 1 for a single local llama-server slot)")
		agentic       = flag.Bool("agentic", false, "run drydock's default agentic path (discovery loop + tool-using reviewer over pinned Git snapshots); cases need bench_meta.agentic")
		reposRoot     = flag.String("repos-root", "", "agentic: directory holding case repositories (default: dataset repos_root)")
		promptVariant = flag.String("prompt-variant", "", "experimental system-prompt variant: '' = production prompts, 'calibrated' = append the (rejected) severity/precision rubric to the reviewer prompt")
	)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if *datasetPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: drydock-bench -dataset cases.json -out results.jsonl [-label name] [-route coder32b]")
		os.Exit(2)
	}

	*lineTolerance = max(*lineTolerance, 0) // one effective value for every metric
	ds, digest, err := loadDataset(*datasetPath)
	if err != nil {
		fatal("load dataset", err)
	}
	done, err := completedCases(*outPath, *retryErrors, digest, *lineTolerance)
	if err != nil {
		fatal("resume", err)
	}

	cfg := config.FromEnv()
	ep := func(base, key, model string) reviewengine.ModelEndpoint {
		return reviewengine.ModelEndpoint{BaseURL: base, APIKey: cfg.EffectiveLLMAPIKey(key), Model: model}
	}
	engineCfg := reviewengine.Config{
		Planner:      ep(cfg.PlannerBaseURL, cfg.PlannerAPIKey, cfg.PlannerModel),
		Coder32B:     ep(cfg.Coder32BBaseURL, cfg.Coder32BAPIKey, cfg.Coder32BModel),
		LLM70B:       ep(cfg.LLM70BBaseURL, cfg.LLM70BAPIKey, cfg.LLM70BModel),
		Coder14B:     ep(cfg.Coder14BBaseURL, cfg.Coder14BAPIKey, cfg.Coder14BModel),
		Sec70B:       ep(cfg.Sec70BBaseURL, "", cfg.Sec70BModel),
		SecClassify:  ep(cfg.SecClassifyBaseURL, "", cfg.SecClassifyModel),
		SecLocalize:  ep(cfg.SecLocalizeBaseURL, "", cfg.SecLocalizeModel),
		PlannerTemp:  0.1, // same as drydock-eval and production
		ReviewerTemp: 0.1,
	}
	var extra map[string]any
	if *extraBody != "" {
		if err := json.Unmarshal([]byte(*extraBody), &extra); err != nil {
			fatal("parse -extra-body", err)
		}
	}
	if _, ok := promptVariants[*promptVariant]; !ok {
		fatal("prompt-variant", fmt.Errorf("unknown variant %q", *promptVariant))
	}
	client := &meteredClient{http: &http.Client{Timeout: *callTimeout}, maxTokens: *maxTokens, extra: extra,
		retries: *retries, promptSuffix: promptVariants[*promptVariant]}
	engine := reviewengine.New(engineCfg, client, logger)

	// Same bundle assembly as eval.EngineRunner for the legacy single-shot path.
	review := func(ctx context.Context, c benchCase) (reviewengine.RunOutput, error) {
		return engine.Run(ctx, reviewengine.RunInput{
			ContextBundle: c.ContextBundle + "\n\n## patch\n" + c.PatchDiff, PatchDiff: c.PatchDiff,
			ChangedFiles: c.ChangedFiles, ReviewerRoute: reviewengine.ModelRoute(*route), SkipWalkthrough: *skipWalk,
		})
	}
	if *agentic {
		root := *reposRoot
		if root == "" {
			root = ds.ReposRoot
		}
		if root == "" {
			fatal("agentic", errors.New("-repos-root or dataset repos_root is required"))
		}
		runner, err := newAgenticRunner(context.Background(), cfg, engine, client, root)
		if err != nil {
			fatal("agentic setup", err)
		}
		defer runner.Close()
		review = func(ctx context.Context, c benchCase) (reviewengine.RunOutput, error) {
			return runner.run(ctx, c, *skipWalk)
		}
	}

	out, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("open output", err)
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	if err := enc.Encode(header{
		Type: "header", Label: *label, DatasetID: ds.ID, DatasetSHA256: digest, DatasetCases: len(ds.Cases),
		StartedAt:   time.Now().UTC(),
		PinnedRoute: *route, MaxTokens: *maxTokens, ExtraBody: *extraBody, Retries: *retries, PromptVariant: *promptVariant,
		Agentic:      *agentic,
		CallTimeoutS: callTimeout.Seconds(), LineTolerance: *lineTolerance, Notes: *notes,
		Endpoints: map[string]string{
			"planner":  engineCfg.Planner.BaseURL + " " + engineCfg.Planner.Model,
			"coder32b": engineCfg.Coder32B.BaseURL + " " + engineCfg.Coder32B.Model,
			"llm70b":   engineCfg.LLM70B.BaseURL + " " + engineCfg.LLM70B.Model,
			"coder14b": engineCfg.Coder14B.BaseURL + " " + engineCfg.Coder14B.Model,
		},
	}); err != nil {
		fatal("write header", err)
	}

	var todo []benchCase
	for _, c := range ds.Cases {
		if !done[c.CaseID] && (*maxCases <= 0 || len(todo) < *maxCases) {
			todo = append(todo, c)
		}
	}
	jobs := make(chan benchCase)
	var wg sync.WaitGroup
	var mu sync.Mutex // serializes the encoder and progress lines
	for range max(*workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				res := runCase(review, c, *caseTimeout, *lineTolerance)
				status := "ok"
				if !res.OK {
					status = "ERR " + res.FailedStage + ": " + truncate(res.Error, 160)
				}
				mu.Lock()
				if err := enc.Encode(res); err != nil {
					fatal("write result", err)
				}
				fmt.Fprintf(os.Stderr, "[%s] %s tp=%d/%d fp=%d %.1fs %s\n", *label, c.CaseID,
					res.Score.TruePositives, res.Score.Expected, res.Score.FalsePositives, float64(res.WallMS)/1000, status)
				mu.Unlock()
			}
		}()
	}
	for _, c := range todo {
		jobs <- c
	}
	close(jobs)
	wg.Wait()
}

func runCase(review func(context.Context, benchCase) (reviewengine.RunOutput, error), c benchCase, timeout time.Duration, tol int) caseResult {
	rec := &recorder{}
	ctx, cancel := context.WithTimeout(withRecorder(context.Background(), rec), timeout)
	defer cancel()
	start := time.Now()
	out, err := review(ctx, c)
	res := caseResult{
		Type: "case", CaseID: c.CaseID, Meta: c.Meta, Expected: c.ExpectedFindings,
		WallMS: time.Since(start).Milliseconds(), Calls: rec.snapshot(), Findings: []reviewengine.Finding{},
	}
	for _, call := range res.Calls {
		if call.WallMS > productionCallTimeout.Milliseconds() {
			res.OverProdLimit++
		}
		if strings.HasPrefix(call.Stage, "repair:") {
			res.RepairAttempts++
		}
	}
	if err != nil {
		res.Error = err.Error()
		res.FailedStage = failedStage(res.Calls, err)
		// A failed case still owes its expected findings as false negatives.
		res.Score = eval.ScoreCase(c.ExpectedFindings, nil, tol)
		return res
	}
	res.OK = true
	res.Route = string(out.Route)
	res.ChangeType = out.Planner.ChangeType
	res.Walkthrough = string(out.WalkthroughStatus.State)
	res.Findings = out.Review.Findings
	res.Score = eval.ScoreCase(c.ExpectedFindings, out.Review.Findings, tol)
	res.LenientTP = lenientMatches(c.ExpectedFindings, out.Review.Findings, tol)
	res.Detected, res.LineErrors = detectedMatches(c.ExpectedFindings, plantedText(c.Meta), out.Review.Findings, tol)
	return res
}

// plantedText extracts bench_meta.after (the planted line) when present.
func plantedText(meta json.RawMessage) string {
	var m struct {
		After string `json:"after"`
	}
	if len(meta) == 0 || json.Unmarshal(meta, &m) != nil {
		return ""
	}
	return m.After
}

func squash(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// quotesPlanted reports whether a finding's text reproduces the planted line.
func quotesPlanted(f reviewengine.Finding, planted string) bool {
	p := squash(planted)
	if len(p) < 12 {
		return false
	}
	probe := p[:min(len(p), 40)]
	for _, s := range []string{f.Evidence, f.SuggestedDiff, f.Explanation} {
		q := squash(s)
		if strings.Contains(q, probe) || (len(q) >= 15 && strings.Contains(p, q)) {
			return true
		}
	}
	return false
}

func detectedMatches(expected []eval.ExpectedFinding, planted string, findings []reviewengine.Finding, tol int) (int, []int) {
	n := 0
	var lineErrors []int
	used := make([]bool, len(findings))
	for _, e := range expected {
		best, bestDist := -1, 1<<30
		for i, f := range findings {
			if used[i] || normPath(f.File) != normPath(e.File) {
				continue
			}
			d := f.Line - e.Line
			ad := max(d, -d)
			strict := ad <= tol && strings.EqualFold(strings.TrimSpace(f.Category), strings.TrimSpace(e.Category))
			if (strict || quotesPlanted(f, planted)) && ad < bestDist {
				best, bestDist = i, ad
			}
		}
		if best >= 0 {
			used[best] = true
			n++
			lineErrors = append(lineErrors, findings[best].Line-e.Line)
		}
	}
	return n, lineErrors
}

// lenientMatches counts expected findings located by any prediction on the
// same file within tolerance, ignoring category. It separates "found the bug
// but labelled it differently" from "missed it".
func lenientMatches(expected []eval.ExpectedFinding, findings []reviewengine.Finding, tol int) int {
	used := make([]bool, len(findings))
	n := 0
	for _, e := range expected {
		for i, f := range findings {
			if used[i] || normPath(f.File) != normPath(e.File) {
				continue
			}
			if d := f.Line - e.Line; d >= -tol && d <= tol {
				used[i] = true
				n++
				break
			}
		}
	}
	return n
}

func normPath(p string) string {
	return strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")), "./")
}

func failedStage(calls []callRecord, err error) string {
	msg := err.Error()
	for _, stage := range []string{"planner", "reviewer", "walkthrough"} {
		if strings.HasPrefix(msg, stage+" ") || strings.Contains(msg, " "+stage+" ") {
			return stage
		}
	}
	if len(calls) > 0 {
		return strings.TrimPrefix(calls[len(calls)-1].Stage, "repair:")
	}
	return "unknown"
}

// --- metered OpenAI-compatible client -------------------------------------

type recorderKey struct{}

type recorder struct {
	mu    sync.Mutex
	calls []callRecord
}

func withRecorder(ctx context.Context, r *recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

func (r *recorder) add(c callRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) snapshot() []callRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]callRecord(nil), r.calls...)
}

// meteredClient sends the same request body as reviewengine.OpenAICompatClient
// but also decodes usage and llama.cpp timings, so prefill and decode speed
// can be reported separately (prefill dominates on Pascal GPUs).
type meteredClient struct {
	http         *http.Client
	maxTokens    int
	extra        map[string]any
	retries      int
	promptSuffix map[string]string // stage -> text appended to that stage's system prompt
}

// promptVariants are candidate prompt changes, applied at the transport so an
// A/B difference is exactly the added text. The "typed" variant (field types
// and allowed values) was adopted into internal/reviewengine/prompts.go after
// it removed every planner repair call without changing accuracy. The
// "calibrated" severity/precision rubric below was measured and rejected
// (false positives rose from 25% to 38% under a frontier judge); it stays so
// that result can be reproduced.
var promptVariants = map[string]map[string]string{
	"": nil,
	"calibrated": {
		"reviewer": `

Severity rubric (when unsure between two levels, choose the lower):
- critical: exploitable security flaw, secret or key exposure, or data loss/corruption.
- high: a definite bug that breaks a normal, commonly used code path.
- medium: a definite bug limited to edge cases or error paths.
- low: minor robustness issue or hardening suggestion.
- info: observation with no defect.

Precision rules:
- Report only defects in the changed code that you can demonstrate from the diff and context shown.
- Do not report praise, or confirmations that code is correct; findings are for problems only.
- Do not speculate about code, types, configuration or callers that are not shown; ask for them in
  needs_more_context instead.
- If a finding depends on library, protocol or language behaviour you are not certain of, set
  confidence below 0.6 rather than asserting it.
- Prefer fewer, correct findings over many uncertain ones.`,
	},
}

// retryable separates provider flakiness (worth retrying on a hosted API)
// from model behaviour. Timeouts are model/serving speed and never retried.
func retryable(err error) bool {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var he *reviewengine.LLMHTTPError
	if errors.As(err, &he) {
		return he.StatusCode == 429 || he.StatusCode >= 500
	}
	return errors.Is(err, errNoChoices) || errors.As(err, &ue)
}

var errNoChoices = errors.New("llm response has no choices")

func stageOf(system string) string {
	switch {
	case strings.HasPrefix(system, "You are a code review planner."):
		return "planner"
	case strings.HasPrefix(system, "You are a code change summarizer."):
		return "walkthrough"
	case strings.HasPrefix(system, "You repair malformed"):
		label := "unknown"
		if i := strings.LastIndex(system, "for the "); i >= 0 {
			label = strings.TrimSuffix(strings.TrimPrefix(system[i:], "for the "), " schema.")
		}
		return "repair:" + label
	default:
		return "reviewer"
	}
}

func (c *meteredClient) ChatCompletion(ctx context.Context, req reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	rec := callRecord{Stage: stageOf(req.System), Model: req.Model}
	if suffix := c.promptSuffix[rec.Stage]; suffix != "" {
		req.System += suffix // repair calls keep reviewengine's own schema hint
	}
	start := time.Now()
	result, err := c.do(ctx, req, &rec)
	for attempt := 1; err != nil && attempt <= c.retries && retryable(err); attempt++ {
		rec.Retries++
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
		start = time.Now() // wall time measures the attempt that produced the result
		result, err = c.do(ctx, req, &rec)
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

func (c *meteredClient) do(ctx context.Context, req reviewengine.ChatRequest, rec *callRecord) (reviewengine.ChatResult, error) {
	if strings.TrimSpace(req.BaseURL) == "" {
		return reviewengine.ChatResult{}, fmt.Errorf("no endpoint configured for model %q (stage %s)", req.Model, rec.Stage)
	}
	payload := map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"temperature": req.Temperature,
	}
	if req.JSONMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	if c.maxTokens > 0 {
		payload["max_tokens"] = c.maxTokens
	}
	for k, v := range c.extra {
		if _, reserved := payload[k]; !reserved || k == "max_tokens" {
			payload[k] = v
		}
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return reviewengine.ChatResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	res, err := c.http.Do(httpReq)
	if err != nil {
		return reviewengine.ChatResult{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return reviewengine.ChatResult{}, &reviewengine.LLMHTTPError{StatusCode: res.StatusCode, Status: res.Status, Body: string(b)}
	}
	var decoded struct {
		Model    string `json:"model"`
		Provider string `json:"provider"` // OpenRouter: upstream that served the call
		Choices  []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"` // llama.cpp
				Reasoning        string `json:"reasoning"`         // OpenRouter
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			Cost             float64 `json:"cost"` // OpenRouter usage accounting, USD
		} `json:"usage"`
		Timings *struct {
			PromptN     int     `json:"prompt_n"`
			PromptMS    float64 `json:"prompt_ms"`
			PredictedN  int     `json:"predicted_n"`
			PredictedMS float64 `json:"predicted_ms"`
		} `json:"timings"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return reviewengine.ChatResult{}, fmt.Errorf("decode llm response: %w", err)
	}
	rec.ServedModel = decoded.Model
	rec.Provider = decoded.Provider
	rec.PromptTokens = decoded.Usage.PromptTokens
	rec.CompletionTokens = decoded.Usage.CompletionTokens
	rec.CostUSD += decoded.Usage.Cost // retries are billed too
	if decoded.Timings != nil {
		rec.PromptN, rec.PromptMS = decoded.Timings.PromptN, decoded.Timings.PromptMS
		rec.PredictedN, rec.PredictedMS = decoded.Timings.PredictedN, decoded.Timings.PredictedMS
	}
	if len(decoded.Choices) == 0 {
		return reviewengine.ChatResult{}, fmt.Errorf("%w (model=%s)", errNoChoices, req.Model)
	}
	choice := decoded.Choices[0]
	rec.FinishReason = choice.FinishReason
	rec.ReasoningChars = len(choice.Message.ReasoningContent) + len(choice.Message.Reasoning)
	return reviewengine.ChatResult{Content: choice.Message.Content, Model: strings.TrimSpace(decoded.Model)}, nil
}

// --- io helpers -------------------------------------------------------------

func loadDataset(path string) (benchDataset, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return benchDataset{}, "", err
	}
	var ds benchDataset
	if err := json.Unmarshal(b, &ds); err != nil {
		return benchDataset{}, "", err
	}
	if len(ds.Cases) == 0 {
		return benchDataset{}, "", errors.New("dataset has no cases")
	}
	sum := sha256.Sum256(b)
	return ds, hex.EncodeToString(sum[:]), nil
}

// completedCases returns case_ids already present in an existing output so
// interrupted sweeps resume where they stopped. It refuses to resume an
// output produced from a different dataset or line tolerance, which would
// otherwise mix stale scores into the new run.
func completedCases(path string, retryErrors bool, digest string, lineTolerance int) (map[string]bool, error) {
	done := map[string]bool{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return done, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var line struct {
			Type          string `json:"type"`
			CaseID        string `json:"case_id"`
			OK            bool   `json:"ok"`
			DatasetSHA256 string `json:"dataset_sha256"`
			LineTolerance int    `json:"line_tolerance"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.Type == "header" {
			if line.DatasetSHA256 != digest || line.LineTolerance != lineTolerance {
				return nil, fmt.Errorf("%s was produced from a different dataset or line tolerance; use a new -out path", path)
			}
			continue
		}
		if line.Type != "case" {
			continue
		}
		if line.OK || !retryErrors {
			done[line.CaseID] = true
		} else {
			delete(done, line.CaseID)
		}
	}
	return done, sc.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "drydock-bench: %s: %v\n", what, err)
	os.Exit(1)
}
