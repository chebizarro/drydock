package publisher

import (
	"encoding/json"
	"strings"
)

const sarifSchema = "https://json.schemastore.org/sarif-2.1.0.json"

type sarifLog struct {
	Version string     `json:"version"`
	Schema  string     `json:"$schema"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name    string      `json:"name"`
	Version string      `json:"version,omitempty"`
	Rules   []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string            `json:"id"`
	ShortDescription sarifMessage      `json:"shortDescription"`
	Properties       sarifRuleProperty `json:"properties"`
}

type sarifRuleProperty struct {
	Tags []string `json:"tags,omitempty"`
}

type sarifResult struct {
	RuleID     string              `json:"ruleId"`
	Level      string              `json:"level"`
	Message    sarifMessage        `json:"message"`
	Locations  []sarifLocation     `json:"locations"`
	Properties sarifResultProperty `json:"properties"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           sarifRegion           `json:"region"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine,omitempty"`
}

type sarifResultProperty struct {
	Severity    string  `json:"severity"`
	CWE         string  `json:"cwe,omitempty"`
	Evidence    string  `json:"evidence,omitempty"`
	Taint       string  `json:"taint,omitempty"`
	Remediation string  `json:"remediation,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
}

// GenerateSARIF produces a deterministic SARIF 2.1.0 artifact for a security
// audit. The returned bytes end with a newline for stable file output.
func GenerateSARIF(findings []SecurityAuditFinding, tools []AuditTool) ([]byte, error) {
	toolName, toolVersion := sarifToolIdentity(tools)
	rules := make([]sarifRule, 0)
	seenRules := make(map[string]struct{})
	results := make([]sarifResult, 0, len(findings))

	for _, finding := range findings {
		ruleID := securityRuleID(finding)
		if _, exists := seenRules[ruleID]; !exists {
			seenRules[ruleID] = struct{}{}
			rules = append(rules, sarifRule{
				ID:               ruleID,
				ShortDescription: sarifMessage{Text: securityRuleDescription(finding, ruleID)},
				Properties:       sarifRuleProperty{Tags: securityRuleTags(finding)},
			})
		}
		line := finding.Line
		if line < 1 {
			line = 1
		}
		endLine := finding.EndLine
		if endLine < line {
			endLine = 0
		}
		results = append(results, sarifResult{
			RuleID: ruleID,
			Level:  sarifLevel(finding.Severity),
			Message: sarifMessage{
				Text: strings.TrimSpace(finding.Message),
			},
			Locations: []sarifLocation{{
				PhysicalLocation: sarifPhysicalLocation{
					ArtifactLocation: sarifArtifactLocation{URI: finding.File},
					Region:           sarifRegion{StartLine: line, EndLine: endLine},
				},
			}},
			Properties: sarifResultProperty{
				Severity:    strings.ToLower(strings.TrimSpace(finding.Severity)),
				CWE:         strings.ToUpper(strings.TrimSpace(finding.CWE)),
				Evidence:    finding.Evidence,
				Taint:       finding.Taint,
				Remediation: finding.Remediation,
				Confidence:  finding.Confidence,
			},
		})
	}

	log := sarifLog{
		Version: "2.1.0",
		Schema:  sarifSchema,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:    toolName,
				Version: toolVersion,
				Rules:   rules,
			}},
			Results: results,
		}},
	}
	artifact, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(artifact, '\n'), nil
}

func sarifToolIdentity(tools []AuditTool) (string, string) {
	for _, tool := range tools {
		if strings.EqualFold(strings.TrimSpace(tool.Name), "drydock") {
			return "drydock", strings.TrimSpace(tool.Version)
		}
	}
	if len(tools) > 0 && strings.TrimSpace(tools[0].Name) != "" {
		return strings.TrimSpace(tools[0].Name), strings.TrimSpace(tools[0].Version)
	}
	return "drydock", ""
}

func securityRuleID(finding SecurityAuditFinding) string {
	if id := strings.TrimSpace(finding.RuleID); id != "" {
		return id
	}
	if cwe := strings.ToUpper(strings.TrimSpace(finding.CWE)); cwe != "" {
		return cwe
	}
	return "DRYDOCK-SECURITY"
}

func securityRuleDescription(finding SecurityAuditFinding, fallback string) string {
	if cwe := strings.ToUpper(strings.TrimSpace(finding.CWE)); cwe != "" {
		return cwe + " security finding"
	}
	return fallback + " security finding"
}

func securityRuleTags(finding SecurityAuditFinding) []string {
	if cwe := strings.ToUpper(strings.TrimSpace(finding.CWE)); cwe != "" {
		return []string{"security", cwe}
	}
	return []string{"security"}
}

func sarifLevel(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "high":
		return "error"
	case "medium":
		return "warning"
	default:
		return "note"
	}
}
