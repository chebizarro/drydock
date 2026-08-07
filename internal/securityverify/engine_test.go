package securityverify

import (
	"context"
	"strings"
	"sync"
	"testing"

	"drydock/internal/metrics"
	"drydock/internal/reviewengine"
	"drydock/internal/testutil"
)

type lockedFakeLLM struct {
	mu   sync.Mutex
	fake *testutil.FakeLLM
}

func (f *lockedFakeLLM) ChatCompletion(ctx context.Context, req reviewengine.ChatRequest) (reviewengine.ChatResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fake.ChatCompletion(ctx, req)
}

func candidate() reviewengine.Finding {
	return reviewengine.Finding{
		Severity: "high", Category: "security", File: "handler.go", Line: 42,
		Evidence: "request parameter reaches query", Explanation: "possible injection",
		Confidence: 0.8,
	}
}

func TestConfigDefaults(t *testing.T) {
	if got := DefaultConfig().VerifyVotes; got != 1 {
		t.Fatalf("Pathway B votes = %d, want 1", got)
	}
	if got := DeepAuditConfig().VerifyVotes; got != 3 {
		t.Fatalf("deep audit votes = %d, want 3", got)
	}
}

func TestRunMajorityRefutesFindingWithDistinctLenses(t *testing.T) {
	fake := &testutil.FakeLLM{Responses: []string{
		`{"refuted":true,"certain":true,"reason":"unreachable"}`,
		`{"refuted":false,"certain":true,"reason":"controllable"}`,
		`{"refuted":true,"certain":true,"reason":"mitigated"}`,
	}}
	client := &lockedFakeLLM{fake: fake}
	engine := New(client, DeepAuditConfig())
	refutedBefore := metrics.SecurityVerifyOutcomes.With("refuted").Value()
	falsePositiveBefore := metrics.SecurityFalsePositives.Value()

	got, err := engine.Run(context.Background(), []reviewengine.Finding{candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d findings, want majority-refuted finding removed", len(got))
	}
	if got := metrics.SecurityVerifyOutcomes.With("refuted").Value(); got != refutedBefore+1 {
		t.Fatalf("refuted metric = %d, want %d", got, refutedBefore+1)
	}
	if got := metrics.SecurityFalsePositives.Value(); got != falsePositiveBefore+1 {
		t.Fatalf("false-positive metric = %d, want %d", got, falsePositiveBefore+1)
	}
	if len(fake.Requests) != 3 {
		t.Fatalf("got %d calls, want 3 verifier calls", len(fake.Requests))
	}

	var reachability, controllability, mitigation bool
	for _, req := range fake.Requests {
		reachability = reachability || strings.Contains(req.System, "reachability and exploitability")
		controllability = controllability || strings.Contains(req.System, "input controllability")
		mitigation = mitigation || strings.Contains(req.System, "existing mitigation")
	}
	if !reachability || !controllability || !mitigation {
		t.Fatalf("verifier prompts did not use all distinct lenses")
	}
}

func TestRunSurvivorIsClassifiedAsFinding(t *testing.T) {
	fake := &testutil.FakeLLM{Responses: []string{
		`{"refuted":false,"certain":true,"reason":"reachable and exploitable"}`,
		`{"cwe":"CWE-89","severity":"critical","confidence":0.97,"remediation":"parameterize the query"}`,
	}}
	engine := New(fake, DefaultConfig())
	survivedBefore := metrics.SecurityVerifyOutcomes.With("survived").Value()

	got, err := engine.Run(context.Background(), []reviewengine.Finding{candidate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if metric := metrics.SecurityVerifyOutcomes.With("survived").Value(); metric != survivedBefore+1 {
		t.Fatalf("survived metric = %d, want %d", metric, survivedBefore+1)
	}
	if got[0].Category != "CWE-89" || got[0].Severity != "critical" || got[0].Confidence != 0.97 {
		t.Fatalf("unexpected classified finding: %+v", got[0])
	}
	if got[0].Suggestion != "parameterize the query" {
		t.Fatalf("remediation = %q", got[0].Suggestion)
	}
	if got[0].File != "handler.go" || got[0].Line != 42 {
		t.Fatalf("classifier changed finding identity: %+v", got[0])
	}
	if len(fake.Requests) != 2 || fake.Requests[1].Model != "secclassify" {
		t.Fatalf("survivor was not routed to classify call")
	}
}

func TestRunDefaultsUncertainAndMalformedVotesToRefuted(t *testing.T) {
	for _, response := range []string{
		`{"refuted":false,"certain":false,"reason":"insufficient context"}`,
		`not json`,
	} {
		t.Run(response, func(t *testing.T) {
			fake := &testutil.FakeLLM{Responses: []string{response}}
			engine := New(fake, DefaultConfig())
			got, err := engine.Run(context.Background(), []reviewengine.Finding{candidate()})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("got %d findings, want uncertain finding refuted", len(got))
			}
			if len(fake.Requests) != 1 {
				t.Fatalf("classifier should not run after refutation")
			}
		})
	}
}
