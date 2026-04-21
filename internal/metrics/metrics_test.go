package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCounterIncAndAdd(t *testing.T) {
	c := &Counter{}
	if c.Value() != 0 {
		t.Fatalf("expected 0, got %d", c.Value())
	}
	c.Inc()
	c.Inc()
	c.Add(5)
	if c.Value() != 7 {
		t.Fatalf("expected 7, got %d", c.Value())
	}
}

func TestGaugeSetIncDec(t *testing.T) {
	g := &Gauge{}
	g.Set(10)
	if g.Value() != 10 {
		t.Fatalf("expected 10, got %d", g.Value())
	}
	g.Inc()
	g.Dec()
	g.Dec()
	if g.Value() != 9 {
		t.Fatalf("expected 9, got %d", g.Value())
	}
}

func TestCounterVecConcurrent(t *testing.T) {
	cv := NewCounterVec()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cv.With("a").Inc()
			cv.With("b").Inc()
		}()
	}
	wg.Wait()
	snap := cv.Snapshot()
	if snap["a"] != 100 {
		t.Fatalf("expected a=100, got %d", snap["a"])
	}
	if snap["b"] != 100 {
		t.Fatalf("expected b=100, got %d", snap["b"])
	}
}

func TestSummaryObserve(t *testing.T) {
	s := &Summary{}
	s.Observe(1.5)
	s.Observe(2.5)
	s.Observe(6.0)
	count, sum := s.snapshot()
	if count != 3 {
		t.Fatalf("expected count=3, got %d", count)
	}
	if sum != 10.0 {
		t.Fatalf("expected sum=10.0, got %f", sum)
	}
}

func TestSummaryVecConcurrent(t *testing.T) {
	sv := NewSummaryVec()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sv.With("model-a").Observe(1.0)
			sv.With("model-b").Observe(2.0)
		}()
	}
	wg.Wait()
	countA, sumA := sv.With("model-a").snapshot()
	countB, sumB := sv.With("model-b").snapshot()
	if countA != 50 || sumA != 50.0 {
		t.Fatalf("model-a: expected count=50 sum=50.0, got count=%d sum=%f", countA, sumA)
	}
	if countB != 50 || sumB != 100.0 {
		t.Fatalf("model-b: expected count=50 sum=100.0, got count=%d sum=%f", countB, sumB)
	}
}

func TestTimerObserves(t *testing.T) {
	s := &Summary{}
	done := Timer(s)
	// Ensure non-zero elapsed time so the observation is measurably positive.
	time.Sleep(time.Millisecond)
	done()
	count, sum := s.snapshot()
	if count != 1 {
		t.Fatalf("expected count=1 after timer, got %d", count)
	}
	if sum <= 0 {
		t.Fatalf("expected positive sum after timer, got %f", sum)
	}
}

func TestHandlerOutputPrometheusFormat(t *testing.T) {
	// Set some values on the package-level metrics.
	EventsRejected.Inc()
	ReviewQueueDepth.Set(5)
	PublishAttempts.Add(3)
	PublishSuccesses.Add(2)

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Fatalf("expected text/plain content type, got %s", ct)
	}

	body := rec.Body.String()
	requireContains(t, body, "# TYPE drydock_uptime_seconds gauge")
	requireContains(t, body, "# TYPE drydock_events_rejected_total counter")
	requireContains(t, body, "# TYPE drydock_review_queue_depth gauge")
	requireContains(t, body, "drydock_review_queue_depth 5")
	requireContains(t, body, "# TYPE drydock_publish_attempts_total counter")
}

func requireContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected output to contain %q, but it did not.\nFull output:\n%s", needle, haystack)
	}
}
