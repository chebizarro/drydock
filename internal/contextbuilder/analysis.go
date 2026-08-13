package contextbuilder

import "fmt"

// PatchAnalysisRequest parameterizes authoritative patch parsing/filtering.
type PatchAnalysisRequest struct {
	Diff         string
	ExcludePaths []string
}

// PatchAnalysisResult is structured patch metadata shared by deterministic
// builders and future tool-facing callers.
type PatchAnalysisResult struct {
	Files         []PatchFileAnalysis
	FilteredDiff  string
	ChangedFiles  []string
	ExcludedFiles []string
}

// PatchFileAnalysis is structured patch metadata. AddedLines are zero-based
// positions in the new file.
type PatchFileAnalysis struct {
	Path       string
	OldPath    string
	NewPath    string
	AddedLines []uint32
}

// AnalyzePatchStructure parses and filters a unified diff while preserving
// patch order and raw diff bytes.
func AnalyzePatchStructure(req PatchAnalysisRequest) (PatchAnalysisResult, error) {
	files, err := parsePatch(req.Diff)
	if err != nil {
		return PatchAnalysisResult{}, err
	}
	result := PatchAnalysisResult{FilteredDiff: req.Diff, Files: make([]PatchFileAnalysis, 0, len(files))}
	for _, file := range files {
		if file == nil {
			continue
		}
		path := pickPath(file)
		if path != "" {
			path, err = normalizeRepositoryRelativePath(path)
			if err != nil {
				return PatchAnalysisResult{}, fmt.Errorf("analyze patch path %q: %w", pickPath(file), err)
			}
		}
		result.Files = append(result.Files, PatchFileAnalysis{
			Path: path, OldPath: file.OldName, NewPath: file.NewName, AddedLines: extractChangedLineNumbers(file),
		})
		if path == "" {
			continue
		}
		if isExcludedPath(path) || matchesExcludePath(path, req.ExcludePaths) {
			result.ExcludedFiles = append(result.ExcludedFiles, path)
		} else {
			result.ChangedFiles = append(result.ChangedFiles, path)
		}
	}
	if len(result.ExcludedFiles) > 0 {
		filtered := filterPatchInput(BuildInput{PatchEventContent: req.Diff}, result.ExcludedFiles)
		result.FilteredDiff = filtered.PatchEventContent
	}
	return result, nil
}

// AnalyzePatchFiles is a compatibility convenience for structured parsing.
func AnalyzePatchFiles(diff string) ([]PatchFileAnalysis, error) {
	result, err := AnalyzePatchStructure(PatchAnalysisRequest{Diff: diff})
	return result.Files, err
}
