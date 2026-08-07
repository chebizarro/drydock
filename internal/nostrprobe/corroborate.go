package nostrprobe

import (
	"fmt"
	"strings"

	"drydock/internal/reviewengine"
)

// Corroborate confirms matching static findings with conclusive live evidence.
// Probe-only and inconclusive results never create or gate findings.
func Corroborate(findings []reviewengine.Finding, evidence []SecurityEvidence) []reviewengine.Finding {
	confirmed := make(map[string][]SecurityEvidence)
	for _, item := range evidence {
		if item.Status != StatusFail {
			continue
		}
		for _, staticRule := range corroborates(item.RuleID) {
			confirmed[staticRule] = append(confirmed[staticRule], item)
		}
	}
	out := append([]reviewengine.Finding(nil), findings...)
	for i := range out {
		for staticRule, items := range confirmed {
			if !strings.Contains(out[i].Evidence, "["+staticRule+"]") {
				continue
			}
			out[i].Confidence = 1
			for _, item := range items {
				out[i].Evidence += fmt.Sprintf(
					"\n[dynamic %s] confirmed against an operator-authorized target; raw target details are gift-wrapped",
					item.RuleID,
				)
			}
		}
	}
	return out
}

func corroborates(ruleID string) []string {
	switch ruleID {
	case RuleRelaySignature:
		return []string{"NOSTR-V2"}
	case RuleRelayID:
		return []string{"NOSTR-V7"}
	case RuleRelayDuplicate:
		return []string{"NOSTR-R1"}
	case RuleClientPreview:
		return []string{"NOSTR-V6"}
	case RuleKeySeparation:
		return []string{"NOSTR-V4"}
	default:
		return nil
	}
}
