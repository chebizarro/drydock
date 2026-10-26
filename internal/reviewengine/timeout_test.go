package reviewengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const okCompletionBody = `{"model":"served","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{}"}}]}`

// slowServer answers after delay, or gives up when the client disconnects.
// partialBody sends headers and the first byte immediately, then stalls, so a
// timeout fires while the body is being read rather than awaiting headers.
func slowServer(t *testing.T, delay time.Duration, partialBody bool, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		// The server notices a client disconnect only once the request body
		// is drained, so drain it before waiting on r.Context().
		_, _ = io.Copy(io.Discard, r.Body)
		if partialBody {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(okCompletionBody[:1]))
			w.(http.Flusher).Flush()
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		if partialBody {
			_, _ = w.Write([]byte(okCompletionBody[1:]))
			return
		}
		_, _ = w.Write([]byte(okCompletionBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	shortTimeout = 50 * time.Millisecond
	serverDelay  = 300 * time.Millisecond
	stallDelay   = 10 * time.Second // never reached: the client gives up first
)

var userMessages = []CompletionMessage{{Role: MessageRoleUser, Content: "hi"}}

func TestOpenAICompatClientRequestTimeoutOverridesDefault(t *testing.T) {
	srv := slowServer(t, serverDelay, false, nil)
	// A client default shorter than the server's delay would fail the call;
	// the role's longer per-request timeout must win (an http.Client.Timeout
	// would have capped it).
	client := NewOpenAICompatClient(shortTimeout)

	if _, err := client.ChatCompletion(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("chat with long per-role timeout: %v", err)
	}
	if _, err := client.Complete(context.Background(), CompletionRequest{BaseURL: srv.URL, Model: "m", Messages: userMessages, Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("complete with long per-role timeout: %v", err)
	}

	// With no per-request timeout the client default applies.
	_, err := client.ChatCompletion(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m"})
	if !errors.Is(err, ErrCallTimeout) {
		t.Fatalf("chat without per-role timeout: err = %v, want ErrCallTimeout from the client default", err)
	}
}

func TestOpenAICompatClientShortRequestTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
	}{{"awaiting headers", false}, {"reading body", true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := slowServer(t, stallDelay, tc.partial, nil)
			client := NewOpenAICompatClient(time.Minute)

			start := time.Now()
			_, chatErr := client.ChatCompletion(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", Timeout: shortTimeout})
			_, completeErr := client.Complete(context.Background(), CompletionRequest{BaseURL: srv.URL, Model: "m", Messages: userMessages, Timeout: shortTimeout})
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("calls took %s; the per-request timeout was not applied", elapsed)
			}
			for name, err := range map[string]error{"chat": chatErr, "complete": completeErr} {
				if !errors.Is(err, ErrCallTimeout) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%s: err = %v, want ErrCallTimeout wrapping context.DeadlineExceeded", name, err)
				}
				if IsTransient(err) {
					t.Fatalf("%s: IsTransient(%v) = true, want false", name, err)
				}
			}
		})
	}
}

func TestOpenAICompatClientCallerDeadlineWins(t *testing.T) {
	srv := slowServer(t, stallDelay, false, nil)
	client := NewOpenAICompatClient(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	_, err := client.ChatCompletion(ctx, ChatRequest{BaseURL: srv.URL, Model: "m", Timeout: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrCallTimeout) {
		t.Fatalf("err = %v: the caller's deadline fired, not the per-call timeout", err)
	}
}

// Documents the Go 1.26 behaviour IsTransient relies on: an http.Client
// timeout matches context.DeadlineExceeded whether it fires awaiting headers
// or while reading the body, so it is never classified as transient.
func TestIsTransientHTTPClientTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
	}{{"awaiting headers", false}, {"reading body", true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := slowServer(t, stallDelay, tc.partial, nil)
			httpClient := &http.Client{Timeout: shortTimeout}
			res, err := httpClient.Get(srv.URL)
			if err == nil {
				_, err = io.ReadAll(res.Body)
				_ = res.Body.Close()
			}
			if err == nil {
				t.Fatal("expected an http.Client timeout")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("http.Client timeout %v does not match context.DeadlineExceeded", err)
			}
			if IsTransient(err) {
				t.Fatalf("IsTransient(%v) = true, want false", err)
			}
		})
	}
}

func TestIsTransientCallTimeoutWithoutDeadlineInChain(t *testing.T) {
	// ErrCallTimeout is non-transient on its own, so a wrapper that drops
	// context.DeadlineExceeded from the chain cannot turn it into a retry.
	if IsTransient(fmt.Errorf("planner: %w", ErrCallTimeout)) {
		t.Fatal("IsTransient(ErrCallTimeout) = true, want false")
	}
}

func TestRetryingClientDoesNotRetryTimedOutCall(t *testing.T) {
	var hits atomic.Int32
	srv := slowServer(t, stallDelay, false, &hits)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := NewRetryingClient(NewOpenAICompatClient(time.Minute), RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond}, logger)

	_, chatErr := client.ChatCompletion(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", Timeout: shortTimeout})
	_, completeErr := client.Complete(context.Background(), CompletionRequest{BaseURL: srv.URL, Model: "m", Messages: userMessages, Timeout: shortTimeout})
	for name, err := range map[string]error{"chat": chatErr, "complete": completeErr} {
		if !errors.Is(err, ErrCallTimeout) {
			t.Fatalf("%s: err = %v, want ErrCallTimeout", name, err)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits = %d, want 2 (one per call, no retries)", got)
	}
}

func TestRetryingClientGivesEachAttemptItsOwnTimeout(t *testing.T) {
	// Each attempt fails with a fast 503; the per-call timeout is per attempt,
	// so retries are not starved by a deadline spent on earlier attempts.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	client := NewRetryingClient(NewOpenAICompatClient(time.Minute), RetryConfig{MaxAttempts: 3, BaseDelay: shortTimeout, MaxDelay: shortTimeout}, nil)

	_, err := client.ChatCompletion(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", Timeout: shortTimeout})
	var httpErr *LLMHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want the final 503", err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server hits = %d, want 3 attempts", got)
	}
}

func TestRetryingClientStopsAtCallerDeadline(t *testing.T) {
	// Retries x timeout cannot outlive the surrounding review deadline: the
	// retry loop stops when the caller's context ends, even mid-backoff.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	client := NewRetryingClient(NewOpenAICompatClient(time.Minute), RetryConfig{MaxAttempts: 10, BaseDelay: time.Minute, MaxDelay: time.Minute}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel()

	start := time.Now()
	_, err := client.ChatCompletion(ctx, ChatRequest{BaseURL: srv.URL, Model: "m", Timeout: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("retrying took %s, past the caller's %s deadline", elapsed, shortTimeout)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 (deadline ended the first backoff)", got)
	}
}

func TestEnginePropagatesEndpointTimeout(t *testing.T) {
	fake := &fakeLLM{responses: []string{
		`{"change_type":"feature","risk_areas":[],"needed_context":[],"review_focus":"x","model_route":"llm70b"}`,
		`{"summary":"ok","findings":[],"needs_more_context":[]}`,
		`{"walkthrough":"w","file_summaries":[]}`,
	}}
	engine := New(Config{
		Planner:  ModelEndpoint{BaseURL: "http://planner", Model: "p", Timeout: 90 * time.Second},
		Coder32B: ModelEndpoint{BaseURL: "http://32b", Model: "c", Timeout: 4 * time.Minute},
		LLM70B:   ModelEndpoint{BaseURL: "http://70b", Model: "l", Timeout: 6 * time.Minute},
		Coder14B: ModelEndpoint{BaseURL: "http://14b", Model: "s", Timeout: 2 * time.Minute},
	}, fake, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := engine.Run(context.Background(), RunInput{ContextBundle: "ctx", ChangedFiles: []string{"a.go"}}); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{90 * time.Second, 6 * time.Minute, 90 * time.Second} // planner, llm70b reviewer, walkthrough
	if len(fake.requests) != len(want) {
		t.Fatalf("requests = %d, want %d", len(fake.requests), len(want))
	}
	for i, req := range fake.requests {
		if req.Timeout != want[i] {
			t.Fatalf("request %d (%s) Timeout = %s, want %s", i, req.BaseURL, req.Timeout, want[i])
		}
	}
}
