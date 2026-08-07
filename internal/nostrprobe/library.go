package nostrprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"git.sharegap.net/cascadia/nostr-secprobe/pkg/probes/client"
	"git.sharegap.net/cascadia/nostr-secprobe/pkg/probes/connect"
	"git.sharegap.net/cascadia/nostr-secprobe/pkg/probes/relay"
	"git.sharegap.net/cascadia/nostr-secprobe/pkg/report"
)

// LibraryBackend runs the published nostr-secprobe probe packages directly.
type LibraryBackend struct {
	logger *slog.Logger
}

func NewLibraryBackend(logger *slog.Logger) *LibraryBackend {
	if logger == nil {
		logger = slog.Default()
	}
	return &LibraryBackend{logger: logger}
}

func (b *LibraryBackend) Run(ctx context.Context, cfg Config) ([]SecurityEvidence, error) {
	var evidence []SecurityEvidence
	for _, target := range cfg.Targets {
		probeType, err := targetProbeType(target)
		if err != nil {
			b.logger.Warn("authorized nostr probe target has unsupported scheme; skipping", "error", err)
			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		var results *report.Results
		switch probeType {
		case "relay":
			results, err = relay.Run(probeCtx, relay.Options{
				Targets: []string{target}, Rate: 5, MaxEvents: 100,
				Active: cfg.Active, IUnderstand: cfg.IUnderstand, NoStore: true,
			})
		case "client":
			results, err = client.Run(probeCtx, client.Options{
				PreviewHost: target, Active: cfg.Active, IUnderstand: cfg.IUnderstand,
			})
		}
		cancel()
		if err != nil {
			return nil, fmt.Errorf("nostr-secprobe %s library probe: %w", probeType, err)
		}
		evidence = append(evidence, mapLibraryReport(results, target)...)
	}

	probeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	results, err := connect.Run(probeCtx, connect.Options{
		Active: cfg.Active, IUnderstand: cfg.IUnderstand,
	})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("nostr-secprobe connect library probe: %w", err)
	}
	evidence = append(evidence, mapLibraryReport(results, "")...)
	return evidence, nil
}

func mapLibraryReport(results *report.Results, target string) []SecurityEvidence {
	if results == nil {
		return nil
	}
	mapped := binaryResults{TargetType: results.TargetType}
	for _, finding := range results.Findings {
		mapped.Findings = append(mapped.Findings, binaryFinding{
			Name: finding.Name, Category: finding.Category,
			Severity: string(finding.Severity), Status: Status(finding.Status),
			Evidence:    plainEvidence(finding.Evidence),
			Mitigations: append([]string(nil), finding.Mitigations...),
			Active:      finding.Active,
		})
	}
	return mapBinaryReport(mapped, target)
}

// plainEvidence prevents dependency-owned concrete types from crossing the
// internal/nostrprobe boundary through the Details map.
func plainEvidence(value any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"value": fmt.Sprint(value)}
	}
	var details map[string]any
	if err := json.Unmarshal(data, &details); err != nil {
		return map[string]any{"value": string(data)}
	}
	return details
}

var _ Backend = (*LibraryBackend)(nil)
