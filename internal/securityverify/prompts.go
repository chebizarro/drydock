package securityverify

import (
	"encoding/json"
	"fmt"
	"strings"

	"drydock/internal/nostrscan/knowledge"
	"drydock/internal/reviewengine"
)

func verifierSystemPrompt(vote int, lens string) string {
	return fmt.Sprintf(`You are adversarial security verifier %d. Your task is to REFUTE the claimed vulnerability, not confirm it.
Use this distinct lens: %s.
Return only JSON: {"refuted":true|false,"certain":true|false,"reason":"brief evidence"}.
Set refuted=true when the claim is false or not exploitable. If evidence is incomplete, ambiguous, or you are uncertain, default to refuted=true.`, vote+1, lens)
}

func nostrRefuteLens() string {
	const instruction = `Nostr absence-check refutation: Here is a claimed missing check with its ingest→use path. Point to the exact file:line where this check DOES occur on this path, or to the framework/library guarantee that performs it. Default to refuted if uncertain.`
	grounding, err := knowledge.Context()
	if err != nil || strings.TrimSpace(grounding) == "" {
		return instruction
	}
	return instruction + "\n\nNostr protocol security knowledge pack:\n" + grounding
}

func classifierSystemPrompt() string {
	return `You classify only security findings that survived adversarial verification.
Assign the most specific CWE identifier, calibrate severity based on demonstrated reachability and impact, and give a concise remediation.
Return only JSON: {"cwe":"CWE-N","severity":"critical|high|medium|low|info","confidence":0.0,"remediation":"actionable fix"}.`
}

func findingPrompt(finding reviewengine.Finding) string {
	raw, _ := json.Marshal(finding)
	return "Candidate security finding:\n" + string(raw)
}
