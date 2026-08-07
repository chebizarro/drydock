package nostrscan

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/nostrscan/knowledge"
	"drydock/internal/securityscan"
)

func TestPresenceRulesVulnerableAndFixedFixtures(t *testing.T) {
	tests := []struct {
		name   string
		ruleID string
	}{
		{name: "v3_nip04", ruleID: "NOSTR-V3"},
		{name: "v3_cbc", ruleID: "NOSTR-V3"},
		{name: "v4_ecdh", ruleID: "NOSTR-V4"},
		{name: "v4_cross_protocol", ruleID: "NOSTR-V4"},
		{name: "v5_message_http", ruleID: "NOSTR-V5"},
		{name: "v6_dm_preview", ruleID: "NOSTR-V6"},
	}

	scanner := securityscan.NewWithRules(PresenceRules())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vulnerable := filepath.Join("testdata", "rules", tt.name, "vulnerable.ts")
			fixed := filepath.Join("testdata", "rules", tt.name, "fixed.ts")

			vulnerableResult := scanner.ScanFiles(context.Background(), ".", []string{vulnerable}, "")
			if !hasFinding(vulnerableResult.Findings, tt.ruleID) {
				t.Fatalf("%s did not flag vulnerable fixture; findings: %#v", tt.ruleID, vulnerableResult.Findings)
			}

			fixedResult := scanner.ScanFiles(context.Background(), ".", []string{fixed}, "")
			if hasFinding(fixedResult.Findings, tt.ruleID) {
				t.Fatalf("%s flagged fixed fixture; findings: %#v", tt.ruleID, fixedResult.Findings)
			}
		})
	}
}

func TestPresenceRuleDMKindVariants(t *testing.T) {
	rule := ruleByID(t, "NOSTR-V6")
	for _, line := range []string{
		`renderDM(message, fetchPreview)`,
		`if event.kind == 4 { fetchPreview(event.content) }`,
		`if event.kind = 14 { unfurl(event.content) }`,
	} {
		if !rule.Pattern.MatchString(line) {
			t.Errorf("NOSTR-V6 did not match %q", line)
		}
	}
}

func TestPresenceRulesCarryKnowledgePackCitations(t *testing.T) {
	for _, rule := range PresenceRules() {
		source := knowledge.VulnerabilitySource(rule.ID)
		if source == "" {
			t.Fatalf("rule %s has no knowledge-pack source", rule.ID)
		}
		if !strings.Contains(rule.Suggestion, source) {
			t.Errorf("rule %s remediation does not cite %q: %q", rule.ID, source, rule.Suggestion)
		}
		if rule.Classification != securityscan.RuleClassificationFinding {
			t.Errorf("rule %s classification = %q", rule.ID, rule.Classification)
		}
		if securityscan.SASTRuleCWE[rule.ID] == "" {
			t.Errorf("rule %s has no CWE mapping", rule.ID)
		}
	}
}

func TestNostrSurfaceRulesAreDocumentedLocators(t *testing.T) {
	want := map[string]string{
		"NOSTR-SURFACE-EVENT-INGEST":       "nostr-event-ingest",
		"NOSTR-SURFACE-SIGNATURE-VERIFY":   "nostr-signature-verification",
		"NOSTR-SURFACE-ENCRYPT-DECRYPT":    "nostr-encrypt-decrypt",
		"NOSTR-SURFACE-SIGNER-BUNKER":      "nostr-signer-bunker-boundary",
		"NOSTR-SURFACE-RELAY-SUBSCRIPTION": "nostr-relay-subscription-handling",
		"NOSTR-SURFACE-EVENT-CACHE":        "nostr-event-cache",
		"NOSTR-SURFACE-PREVIEW-RENDER":     "nostr-preview-render-path",
	}

	rules := SurfaceRules()
	if len(rules) != len(want) {
		t.Fatalf("surface rules = %d, want %d", len(rules), len(want))
	}
	for _, rule := range rules {
		if rule.Classification != securityscan.RuleClassificationSurface {
			t.Errorf("rule %s classification = %q", rule.ID, rule.Classification)
		}
		if tag, ok := want[rule.ID]; !ok {
			t.Errorf("unexpected surface rule %s", rule.ID)
		} else if rule.SurfaceTag != tag {
			t.Errorf("rule %s tag = %q, want %q", rule.ID, rule.SurfaceTag, tag)
		}
		if rule.Severity != "" || rule.Description != "" || rule.Suggestion != "" {
			t.Errorf("surface rule %s contains finding metadata", rule.ID)
		}
	}
}

func TestNostrSurfaceRulePatterns(t *testing.T) {
	tests := []struct {
		tag  string
		line string
	}{
		{tag: "nostr-event-ingest", line: `handleEvent(rawEvent)`},
		{tag: "nostr-signature-verification", line: `event.verifySignature()`},
		{tag: "nostr-encrypt-decrypt", line: `nip44.decrypt(payload)`},
		{tag: "nostr-signer-bunker-boundary", line: `remoteSigner.requestSignature(event)`},
		{tag: "nostr-relay-subscription-handling", line: `relay.subscribe(filters)`},
		{tag: "nostr-event-cache", line: `eventCache.get(event.id)`},
		{tag: "nostr-preview-render-path", line: `renderDM(message)`},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			for _, rule := range SurfaceRules() {
				if rule.SurfaceTag == tt.tag && !rule.Pattern.MatchString(tt.line) {
					t.Errorf("%s did not match %q", tt.tag, tt.line)
				}
			}
		})
	}
}

func TestPresenceRulesAreRoleGated(t *testing.T) {
	clientRules := PresenceRulesForRoles([]Role{RoleClient})
	if !containsRule(clientRules, "NOSTR-V6") || containsRule(clientRules, "NOSTR-R1") {
		t.Fatalf("client role rules are not gated: %#v", clientRules)
	}
	relayRules := PresenceRulesForRoles([]Role{RoleRelay})
	if containsRule(relayRules, "NOSTR-V5") || containsRule(relayRules, "NOSTR-V6") {
		t.Fatalf("relay role includes client preview rules: %#v", relayRules)
	}
	if !RuleAppliesToRoles("NOSTR-R1", []Role{RoleRelay}) {
		t.Fatal("relay role should include replay/persistence checks")
	}
}

func containsRule(rules []securityscan.Rule, id string) bool {
	for _, rule := range rules {
		if rule.ID == id {
			return true
		}
	}
	return false
}

func ruleByID(t *testing.T, id string) securityscan.Rule {
	t.Helper()
	for _, rule := range PresenceRules() {
		if rule.ID == id {
			return rule
		}
	}
	t.Fatalf("rule %s not found", id)
	return securityscan.Rule{}
}

func hasFinding(findings []securityscan.SecurityFinding, id string) bool {
	for _, finding := range findings {
		if finding.RuleID == id {
			return true
		}
	}
	return false
}
