// Package nostrprobe runs explicitly authorized dynamic Nostr security probes.
package nostrprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

type Status string

const (
	StatusPass         Status = "PASS"
	StatusFail         Status = "FAIL"
	StatusInconclusive Status = "INCONCLUSIVE"
)

const (
	RuleRelaySignature = "NOSTR-RELAY-SIG"
	RuleRelayID        = "NOSTR-RELAY-ID"
	RuleRelayDuplicate = "NOSTR-RELAY-DUP"
	RuleRelayMalformed = "NOSTR-RELAY-MALFORMED"
	RuleRelayRate      = "NOSTR-RELAY-RATE"
	RuleClientPreview  = "NOSTR-V6"
	RuleKeySeparation  = "NOSTR-V4"
)

var ErrUnavailable = errors.New("nostrprobe: backend unavailable")

// SecurityEvidence is private audit evidence. Target and Details must only be
// delivered in the gift-wrapped audit detail, never in a public kind 30619.
type SecurityEvidence struct {
	RuleID   string         `json:"rule_id"`
	CWE      string         `json:"cwe"`
	Status   Status         `json:"status"`
	Severity string         `json:"severity"`
	Name     string         `json:"name"`
	Target   string         `json:"target,omitempty"`
	Active   bool           `json:"active,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

// Config contains both the requested targets and the independent operator
// allow-list. Keeping them separate makes authorization testable and fail-closed.
type Config struct {
	Enabled           bool
	Active            bool
	IUnderstand       bool
	Targets           []string
	AuthorizedTargets []string
	Timeout           time.Duration
}

type Backend interface {
	Run(context.Context, Config) ([]SecurityEvidence, error)
}

// Prober enforces authorization before invoking a library backend or the
// operator-installed binary fallback.
type Prober struct {
	library  Backend
	fallback Backend
	logger   *slog.Logger
}

func New(library, fallback Backend, logger *slog.Logger) *Prober {
	if logger == nil {
		logger = slog.Default()
	}
	return &Prober{library: library, fallback: fallback, logger: logger}
}

func (p *Prober) Run(ctx context.Context, cfg Config) ([]SecurityEvidence, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if !cfg.IUnderstand {
		p.logger.Info("nostr dynamic probing disabled; explicit acknowledgement is missing")
		return nil, nil
	}
	targets, err := authorizedTargets(cfg.Targets, cfg.AuthorizedTargets)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		p.logger.Info("nostr dynamic probing disabled; operator allow-list is empty")
		return nil, nil
	}
	if !cfg.Active {
		// Every useful probe exposed by the reference implementation publishes
		// malformed/replayed events or operates an active client harness.
		p.logger.Info("nostr dynamic probing enabled but intrusive probes are disabled")
		return nil, nil
	}
	cfg.Targets = targets
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	if p.library != nil {
		evidence, err := p.library.Run(ctx, cfg)
		if err == nil {
			return evidence, nil
		}
		p.logger.Warn("nostr probe library failed; trying binary fallback", "error", err)
	}
	if p.fallback == nil {
		p.logger.Info("nostr-secprobe unavailable; skipping optional dynamic probing")
		return nil, nil
	}
	evidence, err := p.fallback.Run(ctx, cfg)
	if errors.Is(err, ErrUnavailable) {
		p.logger.Info("nostr-secprobe unavailable; skipping optional dynamic probing")
		return nil, nil
	}
	return evidence, err
}

func authorizedTargets(targets, allow []string) ([]string, error) {
	allowed := make(map[string]struct{}, len(allow))
	for _, target := range allow {
		if target = strings.TrimSpace(target); target != "" {
			allowed[target] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if _, ok := allowed[target]; !ok {
			return nil, fmt.Errorf("nostrprobe: target is not operator-authorized")
		}
		if _, ok := seen[target]; !ok {
			seen[target] = struct{}{}
			out = append(out, target)
		}
	}
	slices.Sort(out)
	return out, nil
}

func cweForRule(ruleID string) string {
	switch ruleID {
	case RuleRelaySignature:
		return "CWE-347"
	case RuleRelayID:
		return "CWE-345"
	case RuleRelayDuplicate:
		return "CWE-294"
	case RuleRelayMalformed:
		return "CWE-20"
	case RuleRelayRate:
		return "CWE-770"
	case RuleClientPreview:
		return "CWE-200"
	case RuleKeySeparation:
		return "CWE-323"
	default:
		return ""
	}
}
