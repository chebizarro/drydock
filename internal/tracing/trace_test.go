package tracing

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNewTraceID(t *testing.T) {
	id1 := NewTraceID()
	id2 := NewTraceID()

	if len(id1) != 16 {
		t.Errorf("expected 16 char trace ID, got %d", len(id1))
	}
	if id1 == id2 {
		t.Error("expected unique trace IDs")
	}
}

type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestNewTraceIDFallbackOnEntropyFailure(t *testing.T) {
	original := crand.Reader
	crand.Reader = failingReader{}
	defer func() { crand.Reader = original }()

	id1 := NewTraceID()
	id2 := NewTraceID()
	if len(id1) != 16 {
		t.Fatalf("expected 16 char fallback trace ID, got %d", len(id1))
	}
	if id1 == "0000000000000000" {
		t.Fatal("fallback trace ID should not be all zeros")
	}
	if id1 == id2 {
		t.Fatal("expected fallback trace IDs to remain unique")
	}
}

func TestWithTrace(t *testing.T) {
	ctx := context.Background()
	traceID := "abc123"

	ctx = WithTrace(ctx, traceID)
	td := FromContext(ctx)

	if td == nil {
		t.Fatal("expected trace data")
	}
	if td.TraceID != traceID {
		t.Errorf("expected trace ID %q, got %q", traceID, td.TraceID)
	}
	if td.StartTime.IsZero() {
		t.Error("expected non-zero start time")
	}
}

func TestWithTraceData(t *testing.T) {
	ctx := context.Background()
	data := TraceData{
		TraceID: "trace-1",
		EventID: "event-1",
		RepoID:  "repo-1",
	}

	ctx = WithTraceData(ctx, data)
	td := FromContext(ctx)

	if td.TraceID != "trace-1" {
		t.Errorf("expected TraceID 'trace-1', got %q", td.TraceID)
	}
	if td.EventID != "event-1" {
		t.Errorf("expected EventID 'event-1', got %q", td.EventID)
	}
	if td.RepoID != "repo-1" {
		t.Errorf("expected RepoID 'repo-1', got %q", td.RepoID)
	}
}

func TestFromContext_NoTrace(t *testing.T) {
	ctx := context.Background()
	td := FromContext(ctx)
	if td != nil {
		t.Error("expected nil for context without trace")
	}
}

func TestTraceID(t *testing.T) {
	ctx := context.Background()

	// No trace
	if got := TraceID(ctx); got != "" {
		t.Errorf("expected empty, got %q", got)
	}

	// With trace
	ctx = WithTrace(ctx, "my-trace")
	if got := TraceID(ctx); got != "my-trace" {
		t.Errorf("expected 'my-trace', got %q", got)
	}
}

func TestSetEventID(t *testing.T) {
	ctx := context.Background()

	// Set on empty context
	ctx = SetEventID(ctx, "event-123")
	td := FromContext(ctx)
	if td.EventID != "event-123" {
		t.Errorf("expected EventID 'event-123', got %q", td.EventID)
	}

	// Update existing
	ctx = SetEventID(ctx, "event-456")
	td = FromContext(ctx)
	if td.EventID != "event-456" {
		t.Errorf("expected EventID 'event-456', got %q", td.EventID)
	}
}

func TestSetRepoID(t *testing.T) {
	ctx := context.Background()

	ctx = SetRepoID(ctx, "repo-abc")
	td := FromContext(ctx)
	if td.RepoID != "repo-abc" {
		t.Errorf("expected RepoID 'repo-abc', got %q", td.RepoID)
	}
}

func TestElapsed(t *testing.T) {
	ctx := context.Background()

	// No trace
	if got := Elapsed(ctx); got != 0 {
		t.Errorf("expected 0 for no trace, got %v", got)
	}

	// With trace
	ctx = WithTrace(ctx, "test")
	time.Sleep(10 * time.Millisecond)
	elapsed := Elapsed(ctx)
	if elapsed < 10*time.Millisecond {
		t.Errorf("expected elapsed >= 10ms, got %v", elapsed)
	}
}

func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))

	// Without trace data
	ctx := context.Background()
	logger := Logger(ctx, base)
	if logger != base {
		t.Error("expected base logger when no trace data")
	}

	// With trace data
	ctx = WithTraceData(ctx, TraceData{
		TraceID: "trace-123",
		EventID: "event-456",
		RepoID:  "repo-789",
	})
	logger = Logger(ctx, base)
	logger.Info("test message")

	output := buf.String()
	if !strings.Contains(output, "trace-123") {
		t.Error("expected trace_id in log output")
	}
	if !strings.Contains(output, "event-456") {
		t.Error("expected event_id in log output")
	}
	if !strings.Contains(output, "repo-789") {
		t.Error("expected repo_id in log output")
	}
}

func TestSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := WithTrace(context.Background(), "span-test")

	span := StartSpan(ctx, logger, "test_operation")
	time.Sleep(5 * time.Millisecond)
	duration := span.End()

	if duration < 5*time.Millisecond {
		t.Errorf("expected duration >= 5ms, got %v", duration)
	}

	output := buf.String()
	if !strings.Contains(output, "test_operation") {
		t.Error("expected span name in output")
	}
	if !strings.Contains(output, "duration_ms") {
		t.Error("expected duration_ms in output")
	}
}

func TestSpan_EndWithStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx := context.Background()

	span := StartSpan(ctx, logger, "status_op")
	span.EndWithStatus("success")

	output := buf.String()
	if !strings.Contains(output, "success") {
		t.Error("expected status in output")
	}
}

func TestSpan_EndWithError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx := context.Background()

	span := StartSpan(ctx, logger, "error_op")
	span.EndWithError(context.DeadlineExceeded)

	output := buf.String()
	if !strings.Contains(output, "error_op") {
		t.Error("expected span name in output")
	}
}

func TestPipelineTimer(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := WithTrace(context.Background(), "timer-test")

	pt := NewPipelineTimer(ctx, logger)

	// Time some stages
	pt.Time(StageRepoPrepare, func() error {
		time.Sleep(5 * time.Millisecond)
		return nil
	})

	pt.Time(StageLLMReview, func() error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	durations := pt.Durations()
	if durations[StageRepoPrepare] < 5*time.Millisecond {
		t.Error("expected repo_prepare >= 5ms")
	}
	if durations[StageLLMReview] < 10*time.Millisecond {
		t.Error("expected llm_review >= 10ms")
	}

	// Clear buffer and log summary
	buf.Reset()
	pt.Summary()

	output := buf.String()
	if !strings.Contains(output, "pipeline timing summary") {
		t.Error("expected summary message")
	}
	if !strings.Contains(output, "total_ms") {
		t.Error("expected total_ms in summary")
	}
}

func TestPipelineTimer_WithError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx := context.Background()

	pt := NewPipelineTimer(ctx, logger)

	err := pt.Time("failing_stage", func() error {
		return context.Canceled
	})

	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "failing_stage") {
		t.Error("expected stage name in output")
	}
}
