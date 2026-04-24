// Package metrics provides lightweight, zero-dependency application metrics
// with Prometheus-compatible text format output.
//
// All metrics are safe for concurrent use. Use the package-level variables
// for instrumentation and call Handler() for the HTTP handler.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Counter is a monotonically increasing int64 metric.
type Counter struct{ v atomic.Int64 }

func (c *Counter) Inc()         { c.v.Add(1) }
func (c *Counter) Add(n int64)  { c.v.Add(n) }
func (c *Counter) Value() int64 { return c.v.Load() }

// Gauge is a metric that can go up and down.
type Gauge struct{ v atomic.Int64 }

func (g *Gauge) Set(n int64)  { g.v.Store(n) }
func (g *Gauge) Inc()         { g.v.Add(1) }
func (g *Gauge) Dec()         { g.v.Add(-1) }
func (g *Gauge) Value() int64 { return g.v.Load() }
func (g *Gauge) Add(n int64)  { g.v.Add(n) }

// CounterVec is a set of counters keyed by a single label value.
type CounterVec struct {
	mu sync.RWMutex
	m  map[string]*Counter
}

func NewCounterVec() *CounterVec { return &CounterVec{m: make(map[string]*Counter)} }

func (v *CounterVec) With(label string) *Counter {
	v.mu.RLock()
	c, ok := v.m[label]
	v.mu.RUnlock()
	if ok {
		return c
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok = v.m[label]; ok {
		return c
	}
	c = &Counter{}
	v.m[label] = c
	return c
}

// Snapshot returns a copy of label→value pairs.
func (v *CounterVec) Snapshot() map[string]int64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[string]int64, len(v.m))
	for k, c := range v.m {
		out[k] = c.Value()
	}
	return out
}

// Summary tracks count and sum for computing averages of observed values.
type Summary struct {
	mu    sync.Mutex
	count int64
	sum   float64
}

func (s *Summary) Observe(v float64) {
	s.mu.Lock()
	s.count++
	s.sum += v
	s.mu.Unlock()
}

func (s *Summary) snapshot() (count int64, sum float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.sum
}

// SummaryVec is a set of summaries keyed by a single label value.
type SummaryVec struct {
	mu sync.RWMutex
	m  map[string]*Summary
}

func NewSummaryVec() *SummaryVec { return &SummaryVec{m: make(map[string]*Summary)} }

func (v *SummaryVec) With(label string) *Summary {
	v.mu.RLock()
	s, ok := v.m[label]
	v.mu.RUnlock()
	if ok {
		return s
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok = v.m[label]; ok {
		return s
	}
	s = &Summary{}
	v.m[label] = s
	return s
}

// ---------------------------------------------------------------------------
// Application metrics
// ---------------------------------------------------------------------------

var (
	// Ingest
	EventsIngested = NewCounterVec() // label: kind (e.g. "1617", "30617")
	EventsRejected = &Counter{}      // invalid signature

	// Review queue
	ReviewQueueDepth  = &Gauge{} // approximate current depth
	ReviewQueuePushed = &Counter{}
	ReviewQueueFull   = &Counter{}

	// Pipeline
	ReviewsStarted  = &Counter{}
	ReviewsFinished = NewCounterVec() // label: "published", "failed"
	ReviewDuration  = &Summary{}      // seconds, end-to-end

	// Workers
	WorkersActive = &Gauge{}

	// LLM
	LLMRequests = NewCounterVec() // label: model
	LLMErrors   = NewCounterVec() // label: model
	LLMDuration = NewSummaryVec() // label: model (seconds)

	// Git operations
	GitOpDuration = NewSummaryVec() // label: "clone", "fetch", "apply"

	// Publisher
	PublishAttempts  = &Counter{}
	PublishSuccesses = &Counter{}
	PublishFailures  = &Counter{}

	// NIP-34 Status
	StatusPublishAttempts  = &Counter{}
	StatusPublishSuccesses = &Counter{}
	StatusPublishSkipped   = &Counter{}
	StatusPublishFailures  = &Counter{}

	// Requeue
	ReviewsRequeued = &Counter{}

	// Auto-fix
	AutoFixPublishAttempts  = &Counter{}
	AutoFixPublishSuccesses = &Counter{}
	AutoFixPublishFailures  = &Counter{}
	AutoFixSkipped          = &Counter{}

	// Ensemble mode
	EnsembleReviewsRun     = &Counter{}
	EnsembleModelsUsed     = NewCounterVec() // label: model route
	EnsembleFindingsMerged = &Counter{}
	EnsembleConsensusBoost = &Counter{} // findings boosted by consensus

	// Security scan
	SecurityScanFindings = &Counter{}

	// Conversations
	ConversationRepliesReceived = &Counter{}
	ConversationResponsesSent   = &Counter{}
	ConversationRateLimited     = &Counter{}
	ConversationErrors          = &Counter{}

	// Uptime
	startTime = time.Now()
)

// Timer is a convenience for timing operations. Usage:
//
//	done := metrics.Timer(metrics.ReviewDuration)
//	defer done()
func Timer(s *Summary) func() {
	start := time.Now()
	return func() { s.Observe(time.Since(start).Seconds()) }
}

// TimerVec is like Timer but for a SummaryVec.
func TimerVec(sv *SummaryVec, label string) func() {
	start := time.Now()
	return func() { sv.With(label).Observe(time.Since(start).Seconds()) }
}

// Handler returns an HTTP handler that writes all metrics in Prometheus
// text exposition format.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w)
	})
}

func writeMetrics(w io.Writer) {
	// Uptime
	fmt.Fprintf(w, "# HELP drydock_uptime_seconds Time since process start.\n")
	fmt.Fprintf(w, "# TYPE drydock_uptime_seconds gauge\n")
	fmt.Fprintf(w, "drydock_uptime_seconds %.1f\n\n", time.Since(startTime).Seconds())

	// Events ingested
	writeCounterVec(w, "drydock_events_ingested_total",
		"Total events ingested by Nostr kind.", "kind", EventsIngested)

	// Events rejected
	writeCounter(w, "drydock_events_rejected_total",
		"Events rejected due to invalid signature.", EventsRejected)

	// Queue
	writeGauge(w, "drydock_review_queue_depth",
		"Approximate review queue depth.", ReviewQueueDepth)
	writeCounter(w, "drydock_review_queue_pushed_total",
		"Tasks pushed to review queue.", ReviewQueuePushed)
	writeCounter(w, "drydock_review_queue_full_total",
		"Tasks dropped because review queue was full.", ReviewQueueFull)

	// Pipeline
	writeCounter(w, "drydock_reviews_started_total",
		"Reviews started by pipeline workers.", ReviewsStarted)
	writeCounterVec(w, "drydock_reviews_finished_total",
		"Reviews finished by outcome.", "outcome", ReviewsFinished)
	writeSummary(w, "drydock_review_duration_seconds",
		"End-to-end review duration.", ReviewDuration)

	// Workers
	writeGauge(w, "drydock_pipeline_workers_active",
		"Number of pipeline workers currently processing.", WorkersActive)

	// LLM
	writeCounterVec(w, "drydock_llm_requests_total",
		"LLM requests by model.", "model", LLMRequests)
	writeCounterVec(w, "drydock_llm_errors_total",
		"LLM errors by model.", "model", LLMErrors)
	writeSummaryVec(w, "drydock_llm_duration_seconds",
		"LLM request duration by model.", "model", LLMDuration)

	// Git
	writeSummaryVec(w, "drydock_git_operation_duration_seconds",
		"Git operation duration by type.", "op", GitOpDuration)

	// Publish
	writeCounter(w, "drydock_publish_attempts_total",
		"Review publish attempts.", PublishAttempts)
	writeCounter(w, "drydock_publish_successes_total",
		"Successful review publishes.", PublishSuccesses)
	writeCounter(w, "drydock_publish_failures_total",
		"Failed review publishes.", PublishFailures)

	// NIP-34 Status
	writeCounter(w, "drydock_status_publish_attempts_total",
		"NIP-34 status publish attempts.", StatusPublishAttempts)
	writeCounter(w, "drydock_status_publish_successes_total",
		"Successful NIP-34 status publishes.", StatusPublishSuccesses)
	writeCounter(w, "drydock_status_publish_skipped_total",
		"NIP-34 status publishes skipped (policy, auth, etc).", StatusPublishSkipped)
	writeCounter(w, "drydock_status_publish_failures_total",
		"Failed NIP-34 status publishes.", StatusPublishFailures)

	// Requeue
	writeCounter(w, "drydock_reviews_requeued_total",
		"Reviews requeued from failed state.", ReviewsRequeued)

	// Auto-fix
	writeCounter(w, "drydock_autofix_publish_attempts_total",
		"Auto-fix patch publish attempts.", AutoFixPublishAttempts)
	writeCounter(w, "drydock_autofix_publish_successes_total",
		"Successful auto-fix patch publishes.", AutoFixPublishSuccesses)
	writeCounter(w, "drydock_autofix_publish_failures_total",
		"Failed auto-fix patch publishes.", AutoFixPublishFailures)
	writeCounter(w, "drydock_autofix_skipped_total",
		"Auto-fix skipped (disabled, no eligible findings, etc).", AutoFixSkipped)

	// Ensemble mode
	writeCounter(w, "drydock_ensemble_reviews_run_total",
		"Reviews run in ensemble mode.", EnsembleReviewsRun)
	writeCounterVec(w, "drydock_ensemble_models_used_total",
		"Models used in ensemble reviews.", "model", EnsembleModelsUsed)
	writeCounter(w, "drydock_ensemble_findings_merged_total",
		"Findings merged from multiple models.", EnsembleFindingsMerged)
	writeCounter(w, "drydock_ensemble_consensus_boost_total",
		"Findings that received consensus boost.", EnsembleConsensusBoost)

	// Security scan
	writeCounter(w, "drydock_security_scan_findings_total",
		"Security findings from deterministic SAST scanner.", SecurityScanFindings)

	// Conversations
	writeCounter(w, "drydock_conversation_replies_received_total",
		"Reply events received targeting Drydock reviews.", ConversationRepliesReceived)
	writeCounter(w, "drydock_conversation_responses_sent_total",
		"Conversation responses published.", ConversationResponsesSent)
	writeCounter(w, "drydock_conversation_rate_limited_total",
		"Replies dropped due to per-review turn limit.", ConversationRateLimited)
	writeCounter(w, "drydock_conversation_errors_total",
		"Conversation processing errors.", ConversationErrors)
}

// --- Prometheus text format helpers ---

func writeCounter(w io.Writer, name, help string, c *Counter) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n\n",
		name, help, name, name, c.Value())
}

func writeGauge(w io.Writer, name, help string, g *Gauge) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n\n",
		name, help, name, name, g.Value())
}

func writeCounterVec(w io.Writer, name, help, label string, cv *CounterVec) {
	snap := cv.Snapshot()
	if len(snap) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := sortedKeys(snap)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, snap[k])
	}
	fmt.Fprintln(w)
}

func writeSummary(w io.Writer, name, help string, s *Summary) {
	count, sum := s.snapshot()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s summary\n", name, help, name)
	fmt.Fprintf(w, "%s_count %d\n", name, count)
	fmt.Fprintf(w, "%s_sum %.6f\n\n", name, sum)
}

func writeSummaryVec(w io.Writer, name, help, label string, sv *SummaryVec) {
	sv.mu.RLock()
	defer sv.mu.RUnlock()
	if len(sv.m) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s summary\n", name, help, name)
	keys := make([]string, 0, len(sv.m))
	for k := range sv.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		count, sum := sv.m[k].snapshot()
		fmt.Fprintf(w, "%s_count{%s=%q} %d\n", name, label, k, count)
		fmt.Fprintf(w, "%s_sum{%s=%q} %.6f\n", name, label, k, sum)
	}
	fmt.Fprintln(w)
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// String returns all metrics as a string (useful for tests).
func String() string {
	var b strings.Builder
	writeMetrics(&b)
	return b.String()
}
