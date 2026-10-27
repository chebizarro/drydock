package agenticreview

import (
	"errors"

	"git.sharegap.net/cascadia/drydock/internal/agenttools"
	"git.sharegap.net/cascadia/drydock/internal/contextbuilder"
)

// CI claims are optional context on successful discovery too, not just its
// deterministic fallback. Never displace the authoritative patch or turn an
// oversized third-party claim section into a review-blocking budget failure.
func withCIClaims(bundle contextbuilder.ContextBundle, claims string, counter contextbuilder.TokenCounter, budget int, headroom float64) (contextbuilder.ContextBundle, error) {
	if claims == "" {
		return bundle, nil
	}
	candidate := bundle
	candidate.Content += "\n\n## " + contextbuilder.LayerCIAttestations + "\n" + claims
	candidate.LayersUsed = append(append([]string(nil), bundle.LayersUsed...), contextbuilder.LayerCIAttestations)
	candidate.LayerStatuses = append(append([]contextbuilder.LayerStatus(nil), bundle.LayerStatuses...), contextbuilder.LayerStatus{Layer: contextbuilder.LayerCIAttestations, Status: "used", Tokens: counter.Count(claims)})
	gated, err := agenttools.GateBundle(candidate, counter, budget, headroom)
	if errors.Is(err, agenttools.ErrBudgetExceeded) {
		bundle.LayersDropped = append(append([]string(nil), bundle.LayersDropped...), contextbuilder.LayerCIAttestations)
		bundle.LayerStatuses = append(append([]contextbuilder.LayerStatus(nil), bundle.LayerStatuses...), contextbuilder.LayerStatus{Layer: contextbuilder.LayerCIAttestations, Status: "dropped", Message: "optional CI claims exceed context budget"})
		return bundle, nil
	}
	if err != nil {
		return bundle, err
	}
	return gated, nil
}
