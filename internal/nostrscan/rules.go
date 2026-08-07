package nostrscan

import (
	"fmt"
	"regexp"

	"drydock/internal/nostrscan/knowledge"
	"drydock/internal/securityscan"
)

var nostrLanguages = []string{".go", ".js", ".jsx", ".ts", ".tsx", ".swift", ".rs", ".py", ".kt", ".java", ".dart"}

// PresenceRules returns deterministic Nostr vulnerability findings.
// The wiring layer should enable these only after Nostr project detection.
func PresenceRules() []securityscan.Rule {
	return []securityscan.Rule{
		{
			ID:             "NOSTR-V3",
			Pattern:        regexp.MustCompile(`(?i)\b(?:nip[_-]?04|nip04)(?:\.|_)?(?:encrypt|decrypt)\s*\(`),
			Severity:       "high",
			Category:       "security",
			Description:    "Deprecated NIP-04 encryption uses malleable AES-CBC without ciphertext authentication.",
			Languages:      nostrLanguages,
			Suggestion:     remediation("NOSTR-V3", "Migrate encrypted messages to NIP-44 v2 and reject NIP-04 downgrade."),
			Classification: securityscan.RuleClassificationFinding,
		},
		{
			ID:             "NOSTR-V3",
			Pattern:        regexp.MustCompile(`(?i)(?:\bcreateCipheriv\s*\([^,]*(?:aes(?:-?256)?-?cbc|cbc)[^,]*,\s*(?:shared|conversation|ecdh)[_-]?(?:secret|key)\b[^)]*\)|\b(?:aes(?:-?256)?-?cbc|cbc)[A-Za-z_.]*(?:encrypt|decrypt)?\s*\(\s*(?:shared|conversation|ecdh)[_-]?(?:secret|key)\b[^)]*\))\s*;?\s*$`),
			Severity:       "high",
			Category:       "security",
			Description:    "AES-CBC is constructed directly over a Nostr shared secret without authenticated encryption.",
			Languages:      nostrLanguages,
			Suggestion:     remediation("NOSTR-V3", "Migrate to NIP-44 v2 authenticated encryption; do not use bare AES-CBC over the Nostr shared secret."),
			Classification: securityscan.RuleClassificationFinding,
		},
		{
			ID:             "NOSTR-V4",
			Pattern:        regexp.MustCompile(`(?i)\b(?:shared|conversation|ecdh)[_-]?(?:secret|key)\b\s*(?::=|=)\s*(?:await\s+)?(?:[\w.]+\.)?(?:ecdh|computeSharedSecret|computeSecret|getSharedSecret)\s*\(`),
			Severity:       "high",
			Category:       "security",
			Description:    "ECDH output is used directly without HKDF context or protocol domain separation.",
			Languages:      nostrLanguages,
			Suggestion:     remediation("NOSTR-V4", "Apply HKDF with an explicit protocol-specific info string and keep NIP-04, NIP-44, and NIP-46 transport keys separate."),
			Classification: securityscan.RuleClassificationFinding,
		},
		{
			ID:             "NOSTR-V4",
			Pattern:        regexp.MustCompile(`(?i)(?:\bnip[_-]?04\b.*\b(?:nip[_-]?46|bunker|nostrconnect)\b|\b(?:nip[_-]?46|bunker|nostrconnect)\b.*\bnip[_-]?04\b).*(?:shared|conversation|ecdh)[_-]?(?:secret|key)|\b(?:shared|conversation|ecdh)[_-]?(?:secret|key)\b.*(?:\bnip[_-]?04\b.*\b(?:nip[_-]?46|bunker|nostrconnect)\b|\b(?:nip[_-]?46|bunker|nostrconnect)\b.*\bnip[_-]?04\b)`),
			Severity:       "high",
			Category:       "security",
			Description:    "The same derived secret reaches NIP-04 and NIP-46/Nostr Connect code paths.",
			Languages:      nostrLanguages,
			Suggestion:     remediation("NOSTR-V4", "Use distinct, domain-separated protocol keys and a disposable NIP-46 transport keypair."),
			Classification: securityscan.RuleClassificationFinding,
		},
		{
			ID:             "NOSTR-V5",
			Pattern:        regexp.MustCompile(`(?i)\b(?:http\.(?:Get|Post|NewRequest)|requests\.(?:get|post)|axios\.(?:get|post)|fetch|URLSession\.shared\.dataTask|client\.(?:get|post))\s*\([^\n]*(?:message|event|decrypted|content|url)(?:\.|\[|\b)`),
			Severity:       "high",
			Category:       "security",
			Description:    "Outbound HTTP is constructed from a message-derived URL, exposing recipient network metadata.",
			Languages:      nostrLanguages,
			Suggestion:     remediation("NOSTR-V5", "Treat message-derived URLs as attacker-controlled and do not fetch them on the recipient; generate previews sender-side before encryption."),
			Classification: securityscan.RuleClassificationFinding,
		},
		{
			ID:             "NOSTR-V6",
			Pattern:        regexp.MustCompile(`(?i)(?:\b(?:render|display)[A-Za-z_]*(?:dm|directMessage)\b.*\b(?:fetchPreview|LPMetadataProvider|og:image|linkPreview|unfurl)\b|\b(?:fetchPreview|LPMetadataProvider|og:image|linkPreview|unfurl)\b.*\b(?:render|display)[A-Za-z_]*(?:dm|directMessage)\b|\bkind\s*[:=]{1,2}\s*(?:4|14)\b.*\b(?:fetchPreview|LPMetadataProvider|og:image|linkPreview|unfurl)\b)`),
			Severity:       "critical",
			Category:       "security",
			Description:    "Recipient-side DM rendering can automatically trigger link-preview or unfurl network requests.",
			Languages:      nostrLanguages,
			Suggestion:     remediation("NOSTR-V6", "Disable previews while rendering received DMs (kinds 4 and 14); create previews only on the sender before E2EE transmission."),
			Classification: securityscan.RuleClassificationFinding,
		},
	}
}

// PresenceRulesForRoles returns only rules relevant to at least one selected role.
func PresenceRulesForRoles(roles []Role) []securityscan.Rule {
	all := PresenceRules()
	out := make([]securityscan.Rule, 0, len(all))
	for _, rule := range all {
		if RuleAppliesToRoles(rule.ID, roles) {
			out = append(out, rule)
		}
	}
	return out
}

// RuleAppliesToRoles reports whether a Nostr rule is meaningful for the roles.
func RuleAppliesToRoles(ruleID string, roles []Role) bool {
	if len(roles) == 0 {
		return true
	}
	for _, role := range roles {
		if role == RoleLibrary {
			return true
		}
		switch ruleID {
		case "NOSTR-V5", "NOSTR-V6":
			if role == RoleClient {
				return true
			}
		case "NOSTR-R1":
			if role == RoleRelay || role == RoleDVM {
				return true
			}
		case "NOSTR-V1", "NOSTR-R2":
			if role == RoleClient || role == RoleDVM {
				return true
			}
		case "NOSTR-V3", "NOSTR-V4":
			if role == RoleClient || role == RoleSigner || role == RoleDVM {
				return true
			}
		default: // Event authenticity and id integrity apply to every event consumer.
			return true
		}
	}
	return false
}

// SurfaceRules returns Nostr protocol boundary locators. They provide context
// for absence analysis and must never be reported as findings.
func SurfaceRules() []securityscan.Rule {
	return []securityscan.Rule{
		surfaceRule("NOSTR-SURFACE-EVENT-INGEST", "nostr-event-ingest", `(?i)(?:\b(?:parse|decode|unmarshal|handle|receive|on)[A-Za-z_]*(?:event|message)\b|\[\s*["']EVENT["']\s*,)`),
		surfaceRule("NOSTR-SURFACE-SIGNATURE-VERIFY", "nostr-signature-verification", `(?i)\b(?:verifySignature|verifyEvent|checkSignature|checkEvent|schnorr\.Verify|\.VerifySignature)\s*\(`),
		surfaceRule("NOSTR-SURFACE-ENCRYPT-DECRYPT", "nostr-encrypt-decrypt", `(?i)\b(?:(?:nip[_-]?(?:04|44)|giftwrap|dm)[A-Za-z_.]*(?:encrypt|decrypt)|(?:encrypt|decrypt)[A-Za-z_]*(?:event|message|dm))\s*\(`),
		surfaceRule("NOSTR-SURFACE-SIGNER-BUNKER", "nostr-signer-bunker-boundary", `(?i)\b(?:nip[_-]?46|nostrconnect|bunker|remoteSigner|signEvent|requestSignature)\b`),
		surfaceRule("NOSTR-SURFACE-RELAY-SUBSCRIPTION", "nostr-relay-subscription-handling", `(?i)\b(?:subscribe|subscription|handleREQ|handleEVENT|relayMessage|onEvent|onEose|CLOSE|EOSE)\b`),
		surfaceRule("NOSTR-SURFACE-EVENT-CACHE", "nostr-event-cache", `(?i)\b(?:eventCache|verificationCache|verifiedEvents|cacheEvent|cachedEvent|eventStore|dedup(?:Event)?)\b`),
		surfaceRule("NOSTR-SURFACE-PREVIEW-RENDER", "nostr-preview-render-path", `(?i)\b(?:fetchPreview|LPMetadataProvider|og:image|linkPreview|unfurl|render(?:Event|Message|DM)|display(?:Event|Message|DM))\b`),
	}
}

func surfaceRule(id, tag, pattern string) securityscan.Rule {
	return securityscan.Rule{
		ID:             id,
		Pattern:        regexp.MustCompile(pattern),
		Languages:      nostrLanguages,
		Classification: securityscan.RuleClassificationSurface,
		SurfaceTag:     tag,
	}
}

func remediation(id, action string) string {
	source := knowledge.VulnerabilitySource(id)
	if source == "" {
		return action
	}
	return fmt.Sprintf("%s Source: %s.", action, source)
}
