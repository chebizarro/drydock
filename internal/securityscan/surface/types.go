package surface

// Location identifies security-relevant code without classifying it as a finding.
type Location struct {
	Tag      string
	RuleID   string
	File     string
	Line     int
	Evidence string
}

// Result holds security-surface locator output and scan coverage.
type Result struct {
	Locations    []Location
	FilesScanned int
	FilesSkipped int
	FilesErrored int
	RulesChecked int
}
