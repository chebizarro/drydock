package nostrprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ToolRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) ([]byte, error)
}

type BinaryBackend struct {
	tools  ToolRunner
	logger *slog.Logger
}

func NewBinaryBackend(tools ToolRunner, logger *slog.Logger) *BinaryBackend {
	if logger == nil {
		logger = slog.Default()
	}
	return &BinaryBackend{tools: tools, logger: logger}
}

type binaryResults struct {
	TargetType string          `json:"target_type"`
	Targets    []string        `json:"targets"`
	Findings   []binaryFinding `json:"findings"`
}

type binaryFinding struct {
	Name        string         `json:"name"`
	Category    string         `json:"category"`
	Severity    string         `json:"severity"`
	Status      Status         `json:"status"`
	Evidence    map[string]any `json:"evidence"`
	Mitigations []string       `json:"mitigations"`
	Active      bool           `json:"active"`
}

func (b *BinaryBackend) Run(ctx context.Context, cfg Config) ([]SecurityEvidence, error) {
	if b == nil || b.tools == nil {
		return nil, ErrUnavailable
	}
	binary, err := b.tools.LookPath("nostr-secprobe")
	if err != nil {
		return nil, ErrUnavailable
	}
	dir, err := os.MkdirTemp("", "drydock-nostrprobe-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	var evidence []SecurityEvidence
	for i, target := range cfg.Targets {
		probeType, err := targetProbeType(target)
		if err != nil {
			b.logger.Warn("authorized nostr probe target has unsupported scheme; skipping", "error", err)
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%d.json", probeType, i))
		args := []string{"probe", probeType, "--out", path, "--timeout", cfg.Timeout.String(), "--active", "--i-understand"}
		switch probeType {
		case "relay":
			args = append(args, "--targets", target, "--no-store")
		case "client":
			args = append(args, "--preview-host", target)
		}
		output, runErr := b.tools.Run(ctx, binary, args...)
		report, readErr := readBinaryReport(path)
		if readErr != nil {
			if runErr != nil {
				return nil, fmt.Errorf("nostr-secprobe %s failed: %w: %s", probeType, runErr, strings.TrimSpace(string(output)))
			}
			return nil, readErr
		}
		evidence = append(evidence, mapBinaryReport(report, target)...)
	}
	// The reference connect probe is target-independent, but authorization is
	// still required and has already been established above.
	path := filepath.Join(dir, "connect.json")
	output, runErr := b.tools.Run(ctx, binary, "probe", "connect", "--out", path, "--timeout", cfg.Timeout.String(), "--active", "--i-understand")
	report, readErr := readBinaryReport(path)
	if readErr == nil {
		evidence = append(evidence, mapBinaryReport(report, "")...)
	} else if runErr != nil {
		b.logger.Warn("nostr-secprobe connect check failed", "error", runErr, "output", strings.TrimSpace(string(output)))
	}
	return evidence, nil
}

func targetProbeType(target string) (string, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "ws", "wss":
		return "relay", nil
	case "http", "https":
		return "client", nil
	default:
		return "", fmt.Errorf("unsupported target scheme")
	}
}

func readBinaryReport(path string) (binaryResults, error) {
	var report binaryResults
	data, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, fmt.Errorf("decode nostr-secprobe report: %w", err)
	}
	return report, nil
}

func mapBinaryReport(report binaryResults, target string) []SecurityEvidence {
	out := make([]SecurityEvidence, 0, len(report.Findings))
	for _, finding := range report.Findings {
		ruleID := ruleForFinding(finding.Name)
		if ruleID == "" {
			continue
		}
		status := normalizedStatus(ruleID, finding)
		severity := strings.ToLower(strings.TrimSpace(finding.Severity))
		if severity == "" {
			severity = "medium"
		}
		out = append(out, SecurityEvidence{
			RuleID: ruleID, CWE: cweForRule(ruleID), Status: status,
			Severity: severity, Name: finding.Name, Target: target,
			Active: finding.Active, Details: finding.Evidence,
		})
	}
	return out
}

func ruleForFinding(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "reject invalid signature":
		return RuleRelaySignature
	case "reject mutated body with stale id/sig":
		return RuleRelayID
	case "reject duplicate event (id replay)":
		return RuleRelayDuplicate
	case "reject invalid pubkey in event", "reject non-hex pubkey", "reject short hex pubkey",
		"reject invalid kind (negative)", "future timestamp policy", "past timestamp policy",
		"empty tag handling", "too-long tag handling", "malformed tag handling",
		"oversized content policy":
		return RuleRelayMalformed
	case "rate limiting / burst behavior (informational)":
		return RuleRelayRate
	case "receiver-side link preview leakage":
		return RuleClientPreview
	case "cross-protocol key reuse (nip-04 vs nip-46)":
		return RuleKeySeparation
	default:
		return ""
	}
}

func normalizedStatus(ruleID string, finding binaryFinding) Status {
	switch ruleID {
	case RuleRelaySignature, RuleRelayID, RuleRelayDuplicate, RuleRelayMalformed:
		if success, observed := publishSuccess(finding.Evidence); observed {
			if success {
				return StatusFail
			}
			return StatusPass
		}
		return StatusInconclusive
	case RuleClientPreview:
		if boolDetail(finding.Evidence, "auto_detect_seen") {
			return StatusFail
		}
	case RuleRelayRate:
		attempted, attemptedOK := intDetail(finding.Evidence, "attempted")
		failures, failuresOK := intDetail(finding.Evidence, "failures")
		if attemptedOK && failuresOK && attempted > 0 && failures == 0 {
			return StatusFail
		}
		return StatusInconclusive
	}
	switch finding.Status {
	case StatusPass, StatusFail, StatusInconclusive:
		return finding.Status
	default:
		return StatusInconclusive
	}
}

func publishSuccess(details map[string]any) (bool, bool) {
	status, ok := details["status"].(map[string]any)
	if !ok {
		return false, false
	}
	if value, ok := status["Success"].(bool); ok {
		return value, true
	}
	value, ok := status["success"].(bool)
	return value, ok
}

func boolDetail(details map[string]any, key string) bool {
	value, _ := details[key].(bool)
	return value
}

func intDetail(details map[string]any, key string) (int, bool) {
	switch value := details[key].(type) {
	case float64:
		return int(value), true
	case json.Number:
		number, err := strconv.Atoi(value.String())
		return number, err == nil
	default:
		return 0, false
	}
}

var _ Backend = (*BinaryBackend)(nil)
