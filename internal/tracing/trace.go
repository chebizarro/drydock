// Package tracing provides request tracing with trace IDs that flow through
// the entire request lifecycle. It integrates with slog to automatically
// include trace context in log messages.
package tracing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// ctxKey is the context key for trace data.
type ctxKey struct{}

var fallbackTraceCounter atomic.Uint64

// TraceData holds tracing information for a request.
type TraceData struct {
	TraceID   string // Unique trace identifier
	EventID   string // Nostr event ID being processed
	RepoID    string // Repository identifier
	StartTime time.Time
}

// NewTraceID generates a new trace ID (16 hex characters).
func NewTraceID() string {
	b := make([]byte, 8)
	if n, err := io.ReadFull(rand.Reader, b); err == nil && n == len(b) {
		return hex.EncodeToString(b)
	}
	return fallbackTraceID()
}

func fallbackTraceID() string {
	counter := fallbackTraceCounter.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", time.Now().UnixNano(), os.Getpid(), counter)))
	return hex.EncodeToString(sum[:8])
}

// WithTraceData creates a new context with full trace data.
func WithTraceData(ctx context.Context, data TraceData) context.Context {
	if data.TraceID == "" {
		data.TraceID = NewTraceID()
	}
	if data.StartTime.IsZero() {
		data.StartTime = time.Now()
	}
	return context.WithValue(ctx, ctxKey{}, &data)
}

// FromContext retrieves trace data from context.
// Returns nil if no trace data is present.
func FromContext(ctx context.Context) *TraceData {
	if v := ctx.Value(ctxKey{}); v != nil {
		return v.(*TraceData)
	}
	return nil
}

// Elapsed returns the time elapsed since the trace started.
func Elapsed(ctx context.Context) time.Duration {
	if td := FromContext(ctx); td != nil {
		return time.Since(td.StartTime)
	}
	return 0
}

// Logger returns a logger with trace context attributes added.
func Logger(ctx context.Context, base *slog.Logger) *slog.Logger {
	td := FromContext(ctx)
	if td == nil {
		return base
	}

	attrs := []any{}
	if td.TraceID != "" {
		attrs = append(attrs, "trace_id", td.TraceID)
	}
	if td.EventID != "" {
		attrs = append(attrs, "event_id", td.EventID)
	}
	if td.RepoID != "" {
		attrs = append(attrs, "repo_id", td.RepoID)
	}

	if len(attrs) == 0 {
		return base
	}
	return base.With(attrs...)
}

// PipelineStages are the standard pipeline stages for timing.
const (
	StageRepoPrepare      = "repo_prepare"
	StageCodeIndex        = "code_index"
	StageFewShotRetrieval = "fewshot_retrieval"
	StageContextBuild     = "context_build"
	StageLLMReview        = "llm_review"
	StageSecurityScan     = "security_scan"
	StagePublish          = "publish"
	StageStatusPublish    = "status_publish"
)

// PipelineTimer tracks timing for all pipeline stages.
type PipelineTimer struct {
	stages map[string]time.Duration
	logger *slog.Logger
}

// NewPipelineTimer creates a new pipeline timer.
func NewPipelineTimer(ctx context.Context, logger *slog.Logger) *PipelineTimer {
	return &PipelineTimer{
		stages: make(map[string]time.Duration),
		logger: Logger(ctx, logger),
	}
}

// Time executes a function and records its duration.
func (pt *PipelineTimer) Time(stage string, fn func() error) error {
	start := time.Now()
	err := fn()
	duration := time.Since(start)
	pt.stages[stage] = duration

	if err != nil {
		pt.logger.Warn("pipeline stage failed",
			"stage", stage,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
	} else {
		pt.logger.Debug("pipeline stage completed",
			"stage", stage,
			"duration_ms", duration.Milliseconds(),
		)
	}
	return err
}

// Summary logs a summary of all stage timings.
func (pt *PipelineTimer) Summary() {
	total := time.Duration(0)
	attrs := []any{}
	for stage, duration := range pt.stages {
		total += duration
		attrs = append(attrs, stage+"_ms", duration.Milliseconds())
	}
	attrs = append(attrs, "total_ms", total.Milliseconds())

	pt.logger.Info("pipeline timing summary", attrs...)
}
