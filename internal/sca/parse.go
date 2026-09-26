// Package sca parses software-composition-analysis (SCA) tool output into
// drydock findings and runs the SCA toolchain against a repository.
//
// It was extracted from internal/auditengine so the dependency-upgrade service
// can consume SCA results without importing the whole audit engine (which drags
// in agenticreview, securityverify, reviewsession, and more). The SARIF decode
// and per-tool package-identity extraction depend only on reviewengine.
package sca

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// ParseSARIFFindings decodes a SARIF 2.1.0 document emitted by trivy, grype, or
// osv-scanner into deduplicated drydock findings, resolving real severity and
// per-tool package identity. tool selects the identity extractor.
func ParseSARIFFindings(tool string, data []byte) ([]reviewengine.Finding, error) {
	var doc sarifDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode %s sarif: %w", tool, err)
	}
	var findings []reviewengine.Finding
	for _, run := range doc.Runs {
		descriptions := make(map[string]string, len(run.Tool.Driver.Rules))
		ruleSeverities := make(map[string]string, len(run.Tool.Driver.Rules))
		rulesByID := make(map[string]sarifRule, len(run.Tool.Driver.Rules))
		for _, rule := range run.Tool.Driver.Rules {
			rulesByID[rule.ID] = rule
			text := strings.TrimSpace(rule.ShortDescription.Text)
			if text == "" {
				text = strings.TrimSpace(rule.FullDescription.Text)
			}
			if text != "" {
				descriptions[rule.ID] = text
			}
			if sev := severityFromProperties(rule.Properties.Severity, rule.Properties.SecuritySeverity); sev != "" {
				ruleSeverities[rule.ID] = sev
			}
		}
		for _, res := range run.Results {
			message := strings.TrimSpace(res.Message.Text)
			if message == "" {
				message = descriptions[res.RuleID]
			}
			file, line := "", 0
			for _, loc := range res.Locations {
				if uri := strings.TrimSpace(loc.PhysicalLocation.ArtifactLocation.URI); uri != "" {
					file = uri
					if line = loc.PhysicalLocation.Region.StartLine; line <= 0 {
						line = 1
					}
					break
				}
			}
			if file == "" || message == "" {
				continue
			}
			finding := reviewengine.Finding{
				Severity:    sarifResultSeverity(res.Properties.Severity, res.Properties.SecuritySeverity, ruleSeverities[res.RuleID], res.Level),
				Category:    "security",
				File:        filepath.ToSlash(file),
				Line:        line,
				Evidence:    "[" + tool + "] " + message,
				Explanation: message,
				RuleID:      strings.TrimSpace(res.RuleID),
				Confidence:  .9,
			}
			if pkg := extractPackageIdentity(tool, rulesByID[res.RuleID], res); pkg != nil {
				finding.Package = pkg
			}
			findings = append(findings, finding)
		}
	}
	return reviewengine.DeduplicateFindings(findings)
}

// sarifResultSeverity resolves a SARIF result to the drydock severity
// vocabulary. Real severity lives in properties on modern scanner output, so
// those win over the coarse SARIF level: a trivy/grype/osv-scanner CRITICAL
// must not be flattened to "high" (the level ceiling) and lose P0. Precedence
// is result properties, then the rule's properties, then the level mapping.
func sarifResultSeverity(resultSeverity, resultSecuritySeverity, ruleSeverity, level string) string {
	if sev := severityFromProperties(resultSeverity, resultSecuritySeverity); sev != "" {
		return sev
	}
	if ruleSeverity != "" {
		return ruleSeverity
	}
	return sarifSeverity(level)
}

// severityFromProperties reads real severity out of SARIF properties: a textual
// Severity token (CRITICAL/HIGH/...) if present, otherwise the GitHub
// security-severity CVSS base score. Returns "" when neither is usable so the
// caller can fall back to the coarse level mapping.
func severityFromProperties(token, securitySeverity string) string {
	if sev := normalizeSeverityToken(token); sev != "" {
		return sev
	}
	return cvssSeverity(securitySeverity)
}

// normalizeSeverityToken maps a textual severity token to the drydock
// vocabulary, or "" if it is empty or unrecognized.
func normalizeSeverityToken(token string) string {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "critical":
		return "critical"
	case "high":
		return "high"
	case "medium", "moderate":
		return "medium"
	case "low":
		return "low"
	default:
		return ""
	}
}

// cvssSeverity maps a CVSS v3 base score (the GitHub security-severity
// convention) to the drydock vocabulary using the standard qualitative bands,
// or "" if the value is not a positive number.
func cvssSeverity(securitySeverity string) string {
	score, err := strconv.ParseFloat(strings.TrimSpace(securitySeverity), 64)
	if err != nil {
		return ""
	}
	switch {
	case score >= 9.0:
		return "critical"
	case score >= 7.0:
		return "high"
	case score >= 4.0:
		return "medium"
	case score > 0:
		return "low"
	default:
		return ""
	}
}

// sarifSeverity maps a SARIF result level to the drydock severity vocabulary.
// An absent level defaults to "warning" per the SARIF 2.1.0 spec, not "low".
func sarifSeverity(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error":
		return "high"
	case "warning", "":
		return "medium"
	default:
		return "low"
	}
}
