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

// CounterVec2 is a set of counters keyed by two label values.
type CounterVec2 struct {
	mu sync.RWMutex
	m  map[[2]string]*Counter
}

func NewCounterVec2() *CounterVec2 {
	return &CounterVec2{m: make(map[[2]string]*Counter)}
}

func (v *CounterVec2) With(label1, label2 string) *Counter {
	key := [2]string{label1, label2}
	v.mu.RLock()
	c, ok := v.m[key]
	v.mu.RUnlock()
	if ok {
		return c
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok = v.m[key]; ok {
		return c
	}
	c = &Counter{}
	v.m[key] = c
	return c
}

// Snapshot returns a copy of label-pair→value pairs.
func (v *CounterVec2) Snapshot() map[[2]string]int64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[[2]string]int64, len(v.m))
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
	EventsRejected = &Counter{}      // invalid ID, signature, or timestamp

	// Review queue
	ReviewQueueDepth  = &Gauge{} // approximate current depth
	ReviewQueuePushed = &Counter{}
	ReviewQueueFull   = &Counter{}

	// Pipeline
	ReviewsStarted        = &Counter{}
	ReviewPrepareAttempts = &Counter{}
	ReviewApplyFailures   = &Counter{}
	ReviewPrepareFailures = NewCounterVec() // label: apply, fetch, checkout, invalid_target
	ContextLayersByStatus = NewCounterVec() // label: used, degraded, truncated, dropped
	ReviewsFinished       = NewCounterVec() // label: "published", "failed"
	ReviewPublications    = NewCounterVec() // label: model identity
	ReviewDuration        = &Summary{}      // seconds, end-to-end

	// Meta-review
	MetaReviewAttempts  = &Counter{}
	MetaReviewSuccesses = &Counter{}
	MetaReviewFailures  = NewCounterVec() // label: failure stage
	MetaReviewOutcomes  = NewCounterVec() // label: success, failed

	// Workers
	WorkersActive = &Gauge{}

	// LLM
	LLMRequests = NewCounterVec() // label: model
	LLMErrors   = NewCounterVec() // label: model
	LLMDuration = NewSummaryVec() // label: model (seconds)

	// Git operations
	GitOpDuration = NewSummaryVec() // label: "clone", "fetch", "apply"

	// Publisher
	PublishAttempts          = &Counter{}
	PublishSuccesses         = &Counter{}
	PublishFailures          = &Counter{}
	FailureNoticeAttempts    = &Counter{}
	FailureNoticeSuccesses   = &Counter{}
	FailureNoticeFailures    = &Counter{}
	FailureNoticesSuppressed = &Counter{}

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

	// Agentic review
	AgenticLoopTurns               = &Counter{}
	AgenticToolCalls               = NewCounterVec2() // labels: tool, outcome
	AgenticBudgetUtilization       = NewSummaryVec()  // label: turns, tool_calls, cumulative_tokens, context_package
	AgenticFinalizationFailures    = NewCounterVec()  // label: reason
	AgenticLoopExhaustionFallbacks = &Counter{}
	AgenticSessionConflicts        = NewCounterVec() // label: version, idempotency, active
	AgenticSnapshotCorruption      = &Counter{}
	AgenticStopReasons             = NewCounterVec() // label: stop reason

	// Ensemble mode
	EnsembleReviewsRun     = &Counter{}
	EnsembleModelsUsed     = NewCounterVec() // label: model route
	EnsembleFindingsMerged = &Counter{}
	EnsembleConsensusBoost = &Counter{} // findings boosted by consensus

	// Codechat (DM codebase Q&A)
	CodeChatDMsReceived       = &Counter{}
	CodeChatResponsesSent     = &Counter{}
	CodeChatRateLimited       = &Counter{}
	CodeChatRateLimitFailures = &Counter{}
	CodeChatErrors            = &Counter{}

	// IDE Gateway
	IDESessionsActive         = &Gauge{}
	IDEReviewRequestsReceived = &Counter{}
	IDEReviewResponsesSent    = &Counter{}
	IDEReviewErrors           = &Counter{}
	IDEFixRequestsReceived    = &Counter{}
	IDEFixResponsesSent       = &Counter{}

	// Marketplace
	MarketplaceRoutingAttempts      = &Counter{}
	MarketplaceRoutingSuccesses     = &Counter{}
	MarketplaceRoutingFailures      = &Counter{}
	MarketplaceNoReviewersFound     = &Counter{}
	MarketplaceAssignmentsCreated   = &Counter{}
	MarketplaceAssignmentsAccepted  = &Counter{}
	MarketplaceAssignmentsRejected  = &Counter{}
	MarketplaceAssignmentsExpired   = &Counter{}
	MarketplaceReviewersActive      = &Gauge{}
	MarketplaceFeedbackReceived     = &Counter{}
	MarketplaceFeedbackAccepted     = &Counter{}
	MarketplaceFeedbackDuplicate    = &Counter{}
	MarketplaceFeedbackUnauthorized = &Counter{}
	MarketplaceFeedbackMalformed    = &Counter{}
	FeedbackRateLimited             = &Counter{}
	FeedbackRateLimitFailures       = &Counter{}
	MarketplaceReputationUpdates    = &Counter{}

	// Security review
	SecurityAuditsRun                         = NewCounterVec2() // labels: depth, state
	SecurityFindings                          = NewCounterVec2() // labels: CWE, severity
	SecurityVerifyOutcomes                    = NewCounterVec()  // label: refuted, survived
	SecurityFalsePositives                    = &Counter{}
	SecurityBaselineSuppressed                = &Counter{}
	SecurityAuditProgressNotificationFailures = &Counter{}

	// Security scan and context extraction capabilities
	SecurityScanFindings = &Counter{}
	TreeSitterAvailable  = &Gauge{}

	// Management endpoints
	DashboardFailures         = NewCounterVec() // label: endpoint
	WorkloadInactivitySeconds = &Gauge{}

	// Conversations
	ConversationRepliesReceived = &Counter{}
	ConversationResponsesSent   = &Counter{}
	ConversationRateLimited     = &Counter{}
	ConversationErrors          = &Counter{}

	// Circuit breakers
	CircuitBreakerOpened   = NewCounterVec() // label: service (embedding, vectorstore, etc)
	CircuitBreakerClosed   = NewCounterVec() // label: service
	CircuitBreakerRejected = NewCounterVec() // label: service (requests rejected due to open circuit)

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
		"Events rejected due to invalid ID, signature, or timestamp.", EventsRejected)

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
	writeCounter(w, "drydock_review_prepare_attempts_total",
		"Repository and patch preparation attempts.", ReviewPrepareAttempts)
	writeCounter(w, "drydock_review_apply_failures_total",
		"Review preparations that failed before model review and formerly produced model-none summaries.", ReviewApplyFailures)
	writeCounterVec(w, "drydock_review_prepare_failures_total",
		"Review preparation failures by stage.", "stage", ReviewPrepareFailures)
	writeCounterVec(w, "drydock_reviews_finished_total",
		"Reviews finished by outcome.", "outcome", ReviewsFinished)
	writeCounterVec(w, "drydock_review_publications_total",
		"Ordinary review summary publications by model identity.", "model", ReviewPublications)
	writeRatio(w, "drydock_review_apply_failure_ratio",
		"Ratio of review preparation attempts that failed while fetching or applying the target.",
		ReviewApplyFailures.Value(), ReviewPrepareAttempts.Value())
	publications := ReviewPublications.Snapshot()
	writeRatio(w, "drydock_review_model_none_publication_ratio",
		"Ratio of ordinary review summary publications labeled with model none.",
		publications["none"], sumCounters(publications))
	writeCounterVec(w, "drydock_context_layers_total",
		"Context layers by build status.", "status", ContextLayersByStatus)
	writeSummary(w, "drydock_review_duration_seconds",
		"End-to-end review duration.", ReviewDuration)

	// Meta-review
	writeCounter(w, "drydock_meta_review_attempts_total",
		"Triggered meta-review attempts.", MetaReviewAttempts)
	writeCounter(w, "drydock_meta_review_successes_total",
		"Meta-review attempts that completed and were audited.", MetaReviewSuccesses)
	writeCounterVec(w, "drydock_meta_review_failures_total",
		"Meta-review failures by processing stage.", "stage", MetaReviewFailures)
	writeCounterVec(w, "drydock_meta_review_outcomes_total",
		"Completed meta-review attempts by final outcome.", "outcome", MetaReviewOutcomes)
	metaOutcomes := MetaReviewOutcomes.Snapshot()
	writeRatio(w, "drydock_meta_review_failure_ratio",
		"Ratio of completed meta-review attempts that failed.",
		metaOutcomes["failed"], sumCounters(metaOutcomes))

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
	writeCounter(w, "drydock_failure_notice_publish_attempts_total",
		"Operational apply-failure notice publish attempts.", FailureNoticeAttempts)
	writeCounter(w, "drydock_failure_notice_publish_successes_total",
		"Successfully published operational apply-failure notices.", FailureNoticeSuccesses)
	writeCounter(w, "drydock_failure_notice_publish_failures_total",
		"Failed operational apply-failure notice publishes.", FailureNoticeFailures)
	writeCounter(w, "drydock_failure_notices_suppressed_total",
		"Apply-failure notices suppressed by configuration.", FailureNoticesSuppressed)

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

	// Agentic review
	writeCounter(w, "drydock_agentic_loop_turns_total",
		"Total model turns executed by agentic discovery and reviewer loops.", AgenticLoopTurns)
	writeCounterVec2(w, "drydock_agentic_tool_calls_total",
		"Agent tool calls by canonical tool name and outcome.", "tool", "outcome", AgenticToolCalls)
	writeSummaryVec(w, "drydock_agentic_budget_utilization_ratio",
		"Agentic budget utilization ratio by budget dimension.", "budget", AgenticBudgetUtilization)
	writeCounterVec(w, "drydock_agentic_finalization_failures_total",
		"Context finalization failures by reason.", "reason", AgenticFinalizationFailures)
	writeCounter(w, "drydock_agentic_loop_exhaustion_fallbacks_total",
		"Discovery loop exhaustions that invoked deterministic fallback.", AgenticLoopExhaustionFallbacks)
	writeCounterVec(w, "drydock_agentic_session_conflicts_total",
		"Review session continuation conflicts by type.", "type", AgenticSessionConflicts)
	writeCounter(w, "drydock_agentic_snapshot_corruption_total",
		"Frozen snapshot integrity failures.", AgenticSnapshotCorruption)
	writeCounterVec(w, "drydock_agentic_stop_reasons_total",
		"Agentic loop completions by stop reason.", "reason", AgenticStopReasons)

	// Ensemble mode
	writeCounter(w, "drydock_ensemble_reviews_run_total",
		"Reviews run in ensemble mode.", EnsembleReviewsRun)
	writeCounterVec(w, "drydock_ensemble_models_used_total",
		"Models used in ensemble reviews.", "model", EnsembleModelsUsed)
	writeCounter(w, "drydock_ensemble_findings_merged_total",
		"Findings merged from multiple models.", EnsembleFindingsMerged)
	writeCounter(w, "drydock_ensemble_consensus_boost_total",
		"Findings that received consensus boost.", EnsembleConsensusBoost)

	// Codechat (DM codebase Q&A)
	writeCounter(w, "drydock_codechat_dms_received_total",
		"Encrypted DMs received for codebase chat.", CodeChatDMsReceived)
	writeCounter(w, "drydock_codechat_responses_sent_total",
		"Codechat responses published.", CodeChatResponsesSent)
	writeCounter(w, "drydock_codechat_rate_limited_total",
		"Codechat requests dropped due to rate limit.", CodeChatRateLimited)
	writeCounter(w, "drydock_codechat_rate_limit_failures_total",
		"Codechat requests denied because the rate-limit backend failed.", CodeChatRateLimitFailures)
	writeCounter(w, "drydock_codechat_errors_total",
		"Codechat processing errors.", CodeChatErrors)

	// IDE Gateway
	writeGauge(w, "drydock_ide_sessions_active",
		"Active IDE sessions.", IDESessionsActive)
	writeCounter(w, "drydock_ide_review_requests_received_total",
		"IDE review requests received.", IDEReviewRequestsReceived)
	writeCounter(w, "drydock_ide_review_responses_sent_total",
		"IDE review responses sent.", IDEReviewResponsesSent)
	writeCounter(w, "drydock_ide_review_errors_total",
		"IDE review processing errors.", IDEReviewErrors)
	writeCounter(w, "drydock_ide_fix_requests_received_total",
		"IDE fix requests received.", IDEFixRequestsReceived)
	writeCounter(w, "drydock_ide_fix_responses_sent_total",
		"IDE fix responses sent.", IDEFixResponsesSent)

	// Marketplace
	writeCounter(w, "drydock_marketplace_routing_attempts_total",
		"Patch routing attempts to community reviewers.", MarketplaceRoutingAttempts)
	writeCounter(w, "drydock_marketplace_routing_successes_total",
		"Successful patch routings to reviewers.", MarketplaceRoutingSuccesses)
	writeCounter(w, "drydock_marketplace_routing_failures_total",
		"Failed patch routing attempts.", MarketplaceRoutingFailures)
	writeCounter(w, "drydock_marketplace_no_reviewers_found_total",
		"Routing attempts with no matching reviewers.", MarketplaceNoReviewersFound)
	writeCounter(w, "drydock_marketplace_assignments_created_total",
		"Review assignments created.", MarketplaceAssignmentsCreated)
	writeCounter(w, "drydock_marketplace_assignments_accepted_total",
		"Review assignments accepted by reviewers.", MarketplaceAssignmentsAccepted)
	writeCounter(w, "drydock_marketplace_assignments_rejected_total",
		"Review assignments rejected by reviewers.", MarketplaceAssignmentsRejected)
	writeCounter(w, "drydock_marketplace_assignments_expired_total",
		"Review assignments that expired without response.", MarketplaceAssignmentsExpired)
	writeGauge(w, "drydock_marketplace_reviewers_active",
		"Number of active community reviewers.", MarketplaceReviewersActive)
	writeCounter(w, "drydock_marketplace_feedback_received_total",
		"Review feedback/ratings received.", MarketplaceFeedbackReceived)
	writeCounter(w, "drydock_marketplace_feedback_notifications_accepted_total",
		"Marketplace feedback notifications durably inserted.", MarketplaceFeedbackAccepted)
	writeCounter(w, "drydock_marketplace_feedback_notifications_duplicate_total",
		"Idempotent duplicate marketplace feedback notifications.", MarketplaceFeedbackDuplicate)
	writeCounter(w, "drydock_marketplace_feedback_notifications_unauthorized_total",
		"Marketplace feedback notifications rejected by sender authorization.", MarketplaceFeedbackUnauthorized)
	writeCounter(w, "drydock_marketplace_feedback_notifications_malformed_total",
		"Malformed marketplace feedback notifications rejected.", MarketplaceFeedbackMalformed)
	writeCounter(w, "drydock_marketplace_feedback_rate_limited_total",
		"Marketplace feedback dropped due to rate limit.", FeedbackRateLimited)
	writeCounter(w, "drydock_marketplace_feedback_rate_limit_failures_total",
		"Marketplace feedback denied because the rate-limit backend failed.", FeedbackRateLimitFailures)
	writeCounter(w, "drydock_marketplace_reputation_updates_total",
		"Reviewer reputation recalculations.", MarketplaceReputationUpdates)

	// Security review
	writeCounterVec2(w, "drydock_security_audits_run_total",
		"Security audit runs by depth and final state.", "depth", "state", SecurityAuditsRun)
	writeCounterVec2(w, "drydock_security_findings_total",
		"Verified security findings by CWE and severity.", "cwe", "severity", SecurityFindings)
	writeCounterVec(w, "drydock_security_verify_outcomes_total",
		"Adversarial security verification outcomes.", "outcome", SecurityVerifyOutcomes)
	writeCounter(w, "drydock_security_false_positives_total",
		"Candidate security findings refuted as estimated false positives.", SecurityFalsePositives)
	writeCounter(w, "drydock_security_baseline_suppressed_total",
		"Verified security findings suppressed by the audit baseline.", SecurityBaselineSuppressed)
	writeCounter(w, "drydock_security_audit_progress_notification_failures_total",
		"Security audit progress notifications that failed to publish.", SecurityAuditProgressNotificationFailures)

	// Security scan
	writeCounter(w, "drydock_security_scan_findings_total",
		"Security findings from deterministic SAST scanner.", SecurityScanFindings)
	writeGauge(w, "drydock_tree_sitter_available",
		"Whether this build includes CGO tree-sitter symbol extraction (1=yes, 0=no).", TreeSitterAvailable)

	// Management endpoints
	writeCounterVec(w, "drydock_dashboard_failures_total",
		"Dashboard API failures by endpoint.", "endpoint", DashboardFailures)
	writeGauge(w, "drydock_workload_inactivity_seconds",
		"Seconds since the last workload activity.", WorkloadInactivitySeconds)

	// Conversations
	writeCounter(w, "drydock_conversation_replies_received_total",
		"Reply events received targeting Drydock reviews.", ConversationRepliesReceived)
	writeCounter(w, "drydock_conversation_responses_sent_total",
		"Conversation responses published.", ConversationResponsesSent)
	writeCounter(w, "drydock_conversation_rate_limited_total",
		"Replies dropped due to per-review turn limit.", ConversationRateLimited)
	writeCounter(w, "drydock_conversation_errors_total",
		"Conversation processing errors.", ConversationErrors)

	// Circuit breakers
	writeCounterVec(w, "drydock_circuit_breaker_opened_total",
		"Times circuit breaker opened due to failures.", "service", CircuitBreakerOpened)
	writeCounterVec(w, "drydock_circuit_breaker_closed_total",
		"Times circuit breaker closed after recovery.", "service", CircuitBreakerClosed)
	writeCounterVec(w, "drydock_circuit_breaker_rejected_total",
		"Requests rejected due to open circuit breaker.", "service", CircuitBreakerRejected)
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

func writeRatio(w io.Writer, name, help string, numerator, denominator int64) {
	ratio := 0.0
	if denominator > 0 {
		ratio = float64(numerator) / float64(denominator)
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %.6f\n\n",
		name, help, name, name, ratio)
}

func sumCounters(values map[string]int64) int64 {
	var total int64
	for _, value := range values {
		total += value
	}
	return total
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

func writeCounterVec2(w io.Writer, name, help, label1, label2 string, cv *CounterVec2) {
	snap := cv.Snapshot()
	if len(snap) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([][2]string, 0, len(snap))
	for key := range snap {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, key := range keys {
		fmt.Fprintf(w, "%s{%s=%q,%s=%q} %d\n", name, label1, key[0], label2, key[1], snap[key])
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
