// Package securityverify adversarially verifies candidate security findings.
package securityverify

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"drydock/internal/reviewengine"
)

const (
	DefaultVerifyVotes   = 1
	DeepAuditVerifyVotes = 3
)

var cwePattern = regexp.MustCompile(`^CWE-[1-9][0-9]*$`)

// Config controls adversarial verification and classification calls.
type Config struct {
	VerifyVotes      int
	VerifyEndpoint   reviewengine.ModelEndpoint
	ClassifyEndpoint reviewengine.ModelEndpoint
	Temperature      float64
}

// DefaultConfig returns the Pathway B defaults.
func DefaultConfig() Config {
	return Config{
		VerifyVotes:      DefaultVerifyVotes,
		VerifyEndpoint:   reviewengine.ModelEndpoint{Model: "sec70b"},
		ClassifyEndpoint: reviewengine.ModelEndpoint{Model: "secclassify"},
	}
}

// DeepAuditConfig returns the audit deep defaults.
func DeepAuditConfig() Config {
	cfg := DefaultConfig()
	cfg.VerifyVotes = DeepAuditVerifyVotes
	return cfg
}

// Classifier assigns a CWE and calibrates severity for a verified finding.
type Classifier interface {
	Classify(context.Context, reviewengine.Finding) (reviewengine.Finding, error)
}

// Engine runs adversarial refutation followed by classification.
type Engine struct {
	client     reviewengine.LLMClient
	cfg        Config
	classifier Classifier
}

// New constructs an Engine. Supplying a Classifier replaces the default LLM
// classifier, primarily to permit composition and deterministic testing.
func New(client reviewengine.LLMClient, cfg Config, classifier ...Classifier) *Engine {
	if cfg.VerifyVotes <= 0 {
		cfg.VerifyVotes = DefaultVerifyVotes
	}
	if strings.TrimSpace(cfg.VerifyEndpoint.Model) == "" {
		cfg.VerifyEndpoint.Model = "sec70b"
	}
	if strings.TrimSpace(cfg.ClassifyEndpoint.Model) == "" {
		cfg.ClassifyEndpoint.Model = "secclassify"
	}
	e := &Engine{client: client, cfg: cfg}
	if len(classifier) > 0 && classifier[0] != nil {
		e.classifier = classifier[0]
	} else {
		e.classifier = &llmClassifier{client: client, cfg: cfg}
	}
	return e
}

// Run refutes each candidate by majority vote and classifies every survivor.
// Verifier errors, malformed responses, and uncertain verdicts count as refutations.
func (e *Engine) Run(ctx context.Context, candidates []reviewengine.Finding) ([]reviewengine.Finding, error) {
	if e.client == nil {
		return nil, fmt.Errorf("securityverify: nil LLM client")
	}

	survivors := make([]reviewengine.Finding, 0, len(candidates))
	for _, candidate := range candidates {
		if !e.refuted(ctx, candidate) {
			survivors = append(survivors, candidate)
		}
	}

	classified := make([]reviewengine.Finding, 0, len(survivors))
	for _, survivor := range survivors {
		finding, err := e.classifier.Classify(ctx, survivor)
		if err != nil {
			return nil, fmt.Errorf("classify %s:%d: %w", survivor.File, survivor.Line, err)
		}
		classified = append(classified, finding)
	}
	return classified, nil
}

type verifyResult struct {
	Refuted bool
}

type verifierResponse struct {
	Refuted bool   `json:"refuted"`
	Certain bool   `json:"certain"`
	Reason  string `json:"reason"`
}

func (e *Engine) refuted(ctx context.Context, finding reviewengine.Finding) bool {
	results := make(chan verifyResult, e.cfg.VerifyVotes)
	var wg sync.WaitGroup
	for vote := 0; vote < e.cfg.VerifyVotes; vote++ {
		lens := verifierLens(vote)
		wg.Add(1)
		go func(vote int, lens string) {
			defer wg.Done()
			results <- verifyResult{Refuted: e.runVerifier(ctx, finding, vote, lens)}
		}(vote, lens)
	}
	wg.Wait()
	close(results)

	refuteVotes := 0
	for result := range results {
		if result.Refuted {
			refuteVotes++
		}
	}
	return refuteVotes > e.cfg.VerifyVotes/2
}

func (e *Engine) runVerifier(ctx context.Context, finding reviewengine.Finding, vote int, lens string) bool {
	result, err := e.client.ChatCompletion(ctx, reviewengine.ChatRequest{
		BaseURL:     e.cfg.VerifyEndpoint.BaseURL,
		APIKey:      e.cfg.VerifyEndpoint.APIKey,
		Model:       e.cfg.VerifyEndpoint.Model,
		Temperature: e.cfg.Temperature,
		JSONMode:    true,
		System:      verifierSystemPrompt(vote, lens),
		User:        findingPrompt(finding),
	})
	if err != nil {
		return true
	}

	var response verifierResponse
	if err := json.Unmarshal([]byte(result.Content), &response); err != nil {
		return true
	}
	// Only an explicit, certain failure to refute lets the finding survive.
	return response.Refuted || !response.Certain
}

func verifierLens(vote int) string {
	lenses := []string{
		"reachability and exploitability: prove the vulnerable path cannot be reached or exploited",
		"input controllability: prove an attacker cannot control the relevant source, value, or state",
		"existing mitigation: find validation, authorization, escaping, sandboxing, or another effective control",
	}
	return lenses[vote%len(lenses)]
}

type classificationResponse struct {
	CWE         string  `json:"cwe"`
	Severity    string  `json:"severity"`
	Confidence  float64 `json:"confidence"`
	Remediation string  `json:"remediation"`
}

type llmClassifier struct {
	client reviewengine.LLMClient
	cfg    Config
}

func (c *llmClassifier) Classify(ctx context.Context, finding reviewengine.Finding) (reviewengine.Finding, error) {
	result, err := c.client.ChatCompletion(ctx, reviewengine.ChatRequest{
		BaseURL:     c.cfg.ClassifyEndpoint.BaseURL,
		APIKey:      c.cfg.ClassifyEndpoint.APIKey,
		Model:       c.cfg.ClassifyEndpoint.Model,
		Temperature: c.cfg.Temperature,
		JSONMode:    true,
		System:      classifierSystemPrompt(),
		User:        findingPrompt(finding),
	})
	if err != nil {
		return reviewengine.Finding{}, err
	}

	var response classificationResponse
	if err := json.Unmarshal([]byte(result.Content), &response); err != nil {
		return reviewengine.Finding{}, fmt.Errorf("parse classification: %w", err)
	}
	response.CWE = strings.ToUpper(strings.TrimSpace(response.CWE))
	if !cwePattern.MatchString(response.CWE) {
		return reviewengine.Finding{}, fmt.Errorf("invalid CWE %q", response.CWE)
	}
	if !reviewengine.IsValidSeverity(response.Severity) {
		return reviewengine.Finding{}, fmt.Errorf("invalid severity %q", response.Severity)
	}
	if response.Confidence < 0 || response.Confidence > 1 {
		return reviewengine.Finding{}, fmt.Errorf("confidence must be in [0,1]")
	}

	finding.Category = response.CWE
	finding.Severity = strings.ToLower(strings.TrimSpace(response.Severity))
	finding.Confidence = response.Confidence
	finding.Suggestion = strings.TrimSpace(response.Remediation)
	return finding, nil
}
