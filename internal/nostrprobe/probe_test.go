package nostrprobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"drydock/internal/reviewengine"

	"git.sharegap.net/cascadia/nostr-secprobe/pkg/report"
)

type fakeBackend struct {
	calls int
	cfg   Config
	out   []SecurityEvidence
	err   error
}

func (f *fakeBackend) Run(_ context.Context, cfg Config) ([]SecurityEvidence, error) {
	f.calls++
	f.cfg = cfg
	return f.out, f.err
}

func TestProberAuthorizationGates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "disabled", cfg: Config{Active: true, IUnderstand: true, Targets: []string{"ws://relay.test"}, AuthorizedTargets: []string{"ws://relay.test"}}},
		{name: "missing acknowledgement", cfg: Config{Enabled: true, Active: true, Targets: []string{"ws://relay.test"}, AuthorizedTargets: []string{"ws://relay.test"}}},
		{name: "active disabled", cfg: Config{Enabled: true, IUnderstand: true, Targets: []string{"ws://relay.test"}, AuthorizedTargets: []string{"ws://relay.test"}}},
		{name: "empty operator allow-list", cfg: Config{Enabled: true, Active: true, IUnderstand: true, Targets: []string{"ws://relay.test"}}},
		{name: "unauthorized target", cfg: Config{Enabled: true, Active: true, IUnderstand: true, Targets: []string{"ws://other.test"}, AuthorizedTargets: []string{"ws://relay.test"}}, wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &fakeBackend{}
			prober := New(backend, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			_, err := prober.Run(context.Background(), test.cfg)
			if (err != nil) != test.wantErr {
				t.Fatalf("Run() error = %v, wantErr %v", err, test.wantErr)
			}
			if backend.calls != 0 {
				t.Fatalf("backend calls = %d, want 0", backend.calls)
			}
		})
	}
}

func TestProberUsesAuthorizedLibraryBackend(t *testing.T) {
	t.Parallel()
	backend := &fakeBackend{out: []SecurityEvidence{{RuleID: RuleRelaySignature, Status: StatusFail}}}
	prober := New(backend, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := prober.Run(context.Background(), Config{
		Enabled: true, Active: true, IUnderstand: true,
		Targets:           []string{"ws://relay.test", "ws://relay.test"},
		AuthorizedTargets: []string{"ws://relay.test"},
		Timeout:           time.Second,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if backend.calls != 1 || len(backend.cfg.Targets) != 1 || backend.cfg.Targets[0] != "ws://relay.test" {
		t.Fatalf("backend config = %#v", backend.cfg)
	}
	if len(got) != 1 || got[0].RuleID != RuleRelaySignature {
		t.Fatalf("evidence = %#v", got)
	}
}

func TestProberFallsBackAndSkipsMissingBinary(t *testing.T) {
	t.Parallel()
	library := &fakeBackend{err: errors.New("library unavailable")}
	prober := New(library, NewBinaryBackend(missingTool{}, nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := prober.Run(context.Background(), Config{
		Enabled: true, Active: true, IUnderstand: true,
		Targets: []string{"ws://relay.test"}, AuthorizedTargets: []string{"ws://relay.test"},
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("Run() = %#v, %v", got, err)
	}
}

type reportTool struct {
	calls [][]string
}

func (f *reportTool) LookPath(string) (string, error) { return "/opt/bin/nostr-secprobe", nil }
func (f *reportTool) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	var out string
	for i := range args {
		if args[i] == "--out" && i+1 < len(args) {
			out = args[i+1]
		}
	}
	report := binaryResults{TargetType: "connect", Findings: []binaryFinding{}}
	if len(args) >= 2 && args[1] == "relay" {
		report = binaryResults{TargetType: "relay", Findings: []binaryFinding{{
			Name: "Reject invalid signature", Severity: "HIGH", Status: StatusFail,
			Evidence: map[string]any{"status": map[string]any{"Success": true}}, Active: true,
		}}}
	}
	data, _ := json.Marshal(report)
	return nil, os.WriteFile(out, data, 0o600)
}

func TestBinaryBackendUsesActiveAcknowledgedCLIAndParsesReport(t *testing.T) {
	t.Parallel()
	tool := &reportTool{}
	backend := NewBinaryBackend(tool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := backend.Run(context.Background(), Config{
		Active: true, IUnderstand: true, Targets: []string{"ws://127.0.0.1:12345"}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if len(got) != 1 || got[0].RuleID != RuleRelaySignature || got[0].Status != StatusFail {
		t.Fatalf("evidence = %#v", got)
	}
	if len(tool.calls) != 2 {
		t.Fatalf("binary calls = %d, want relay + connect", len(tool.calls))
	}
	for _, args := range tool.calls {
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--active") || !strings.Contains(joined, "--i-understand") {
			t.Fatalf("unsafe binary args: %q", joined)
		}
	}
}

type missingTool struct{}

func (missingTool) LookPath(string) (string, error) { return "", errors.New("missing") }
func (missingTool) Run(context.Context, string, ...string) ([]byte, error) {
	panic("Run called")
}

func TestMapLibraryReportMapsUpstreamResultsAtBoundary(t *testing.T) {
	t.Parallel()
	results := &report.Results{Findings: []report.Finding{
		{
			Name: "Reject invalid signature", Severity: report.High, Status: report.Fail,
			Evidence: map[string]any{
				"status": struct {
					Success bool `json:"Success"`
				}{Success: true},
			},
			Active: true,
		},
		{
			Name: "Receiver-side link preview leakage", Severity: report.High, Status: report.Pass,
			Evidence: map[string]any{"auto_detect_seen": true},
			Active:   true,
		},
		{
			Name: "Cross-protocol key reuse (NIP-04 vs NIP-46)", Severity: report.High, Status: report.Fail,
			Evidence: map[string]any{"domain_separated": false},
			Active:   true,
		},
	}}
	got := mapLibraryReport(results, "wss://private.example")
	want := []string{RuleRelaySignature, RuleClientPreview, RuleKeySeparation}
	if len(got) != len(want) {
		t.Fatalf("mapped evidence = %#v", got)
	}
	for i, ruleID := range want {
		if got[i].RuleID != ruleID || got[i].Status != StatusFail {
			t.Fatalf("evidence[%d] = %#v, want rule %s FAIL", i, got[i], ruleID)
		}
	}
	status, ok := got[0].Details["status"].(map[string]any)
	if !ok {
		t.Fatalf("dependency-owned evidence type crossed boundary: %T", got[0].Details["status"])
	}
	if success, ok := status["Success"].(bool); !ok || !success {
		t.Fatalf("plain status evidence = %#v", status)
	}
}

func TestMapBinaryReport(t *testing.T) {
	t.Parallel()
	report := binaryResults{Findings: []binaryFinding{
		{Name: "Reject invalid signature", Severity: "HIGH", Status: StatusFail, Evidence: map[string]any{"status": map[string]any{"Success": true}}},
		{Name: "Reject mutated body with stale id/sig", Severity: "MEDIUM", Status: StatusFail, Evidence: map[string]any{"status": map[string]any{"Success": true}}},
		{Name: "Reject duplicate event (id replay)", Severity: "LOW", Status: StatusFail, Evidence: map[string]any{"status": map[string]any{"Success": true}}},
		{Name: "Malformed tag handling", Severity: "LOW", Status: StatusInconclusive, Evidence: map[string]any{"status": map[string]any{"success": true}}},
		{Name: "Rate limiting / burst behavior (informational)", Severity: "LOW", Status: StatusInconclusive, Evidence: map[string]any{"attempted": float64(20), "failures": float64(0)}},
		{Name: "Receiver-side link preview leakage", Severity: "HIGH", Status: StatusPass, Evidence: map[string]any{"auto_detect_seen": true}},
		{Name: "Cross-protocol key reuse (NIP-04 vs NIP-46)", Severity: "HIGH", Status: StatusFail, Evidence: map[string]any{}},
	}}
	got := mapBinaryReport(report, "ws://private.example")
	want := []string{RuleRelaySignature, RuleRelayID, RuleRelayDuplicate, RuleRelayMalformed, RuleRelayRate, RuleClientPreview, RuleKeySeparation}
	if len(got) != len(want) {
		t.Fatalf("mapped evidence = %#v", got)
	}
	for i := range want {
		if got[i].RuleID != want[i] || got[i].Status != StatusFail {
			t.Fatalf("evidence[%d] = %#v, want rule %s FAIL", i, got[i], want[i])
		}
		if got[i].Target != "ws://private.example" {
			t.Fatalf("target = %q", got[i].Target)
		}
	}
}

func TestCorroborateOnlyConclusiveMatchingStaticFindings(t *testing.T) {
	t.Parallel()
	findings := []reviewengine.Finding{
		{Evidence: "[CWE-347] [NOSTR-V2] missing verification", Confidence: .55},
		{Evidence: "[CWE-323] [NOSTR-V4] key reuse", Confidence: .7},
	}
	got := Corroborate(findings, []SecurityEvidence{
		{RuleID: RuleRelaySignature, Status: StatusFail, Target: "wss://secret.example"},
		{RuleID: RuleKeySeparation, Status: StatusInconclusive},
		{RuleID: RuleRelayRate, Status: StatusFail},
	})
	if got[0].Confidence != 1 || !strings.Contains(got[0].Evidence, "dynamic "+RuleRelaySignature) {
		t.Fatalf("corroborated finding = %#v", got[0])
	}
	if strings.Contains(got[0].Evidence, "secret.example") {
		t.Fatalf("public finding evidence leaked target: %q", got[0].Evidence)
	}
	if got[1].Confidence != .7 {
		t.Fatalf("inconclusive evidence changed confidence: %#v", got[1])
	}
}

func TestSecurityEvidenceKeepsTargetInPrivateJSON(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(SecurityEvidence{RuleID: RuleRelaySignature, Target: "wss://private.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "private.example") {
		t.Fatalf("private evidence omitted target: %s", data)
	}
}
