package contextbuilder

// PatchFacade owns structured patch analysis: parsing and filtering a unified
// diff while preserving added-line ranges. It is the one public entry point for
// callers that need the structured PatchAnalysisResult rather than a rendered
// context layer.
type PatchFacade struct{}

func NewPatchFacade() PatchFacade { return PatchFacade{} }

func (PatchFacade) Analyze(req PatchAnalysisRequest) (PatchAnalysisResult, error) {
	return AnalyzePatchStructure(req)
}
