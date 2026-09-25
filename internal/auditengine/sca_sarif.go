package auditengine

import (
	"regexp"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// sarifDocument is the SARIF 2.1.0 shape emitted by trivy, grype, and
// osv-scanner. Decoding it typed keeps each result's file path and message
// together, instead of guessing field names across three schemas where they
// live in different objects — the bug that made the old walker drop every
// finding (DRYDOCK-rnmo). The types are named (not anonymous) so the per-tool
// package-identity extractors below can be handed a single rule/result.
type sarifDocument struct {
	Runs []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool struct {
		Driver struct {
			Rules []sarifRule `json:"rules"`
		} `json:"driver"`
	} `json:"tool"`
	Results []sarifResult `json:"results"`
}

// sarifText is the {text, markdown} pair SARIF uses for descriptions, messages,
// and help. Only help carries markdown in practice (osv-scanner packs the fixed
// version into a markdown table), but modelling both is harmless.
type sarifText struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

type sarifRule struct {
	ID               string   `json:"id"`
	ShortDescription sarifText `json:"shortDescription"`
	FullDescription  sarifText `json:"fullDescription"`
	// Help holds the human-readable vulnerability detail. trivy and grype pack
	// "Package:/Installed Version:/Fixed Version:" lines into Help.Text;
	// osv-scanner packs a "Fixed Versions" table into Help.Markdown.
	Help sarifText `json:"help"`
	// DeprecatedIDs carries advisory aliases (osv-scanner lists CVE/GHSA/RUSTSEC
	// aliases here).
	DeprecatedIDs []string            `json:"deprecatedIds"`
	Properties    sarifRuleProperties `json:"properties"`
}

// sarifRuleProperties carries the GitHub-convention severity fields plus the
// package URLs grype emits. security-severity is a CVSS base score; several
// scanners also emit a textual Severity token.
type sarifRuleProperties struct {
	SecuritySeverity string   `json:"security-severity"`
	Severity         string   `json:"severity"`
	PURLs            []string `json:"purls"`
}

type sarifResult struct {
	RuleID     string                `json:"ruleId"`
	Level      string                `json:"level"`
	Message    sarifText             `json:"message"`
	Properties sarifResultProperties `json:"properties"`
	Locations  []sarifLocation       `json:"locations"`
}

type sarifResultProperties struct {
	SecuritySeverity string `json:"security-severity"`
	Severity         string `json:"severity"`
}

type sarifLocation struct {
	PhysicalLocation struct {
		ArtifactLocation struct {
			URI string `json:"uri"`
		} `json:"artifactLocation"`
		Region struct {
			StartLine int `json:"startLine"`
		} `json:"region"`
	} `json:"physicalLocation"`
}

// advisoryPattern extracts clean advisory identifiers (CVE, GHSA, GO, RUSTSEC,
// PYSEC, OSV, and the generic <PREFIX>-<year>-<num> form) out of the varied
// strings each tool exposes them in — a composite grype rule id, an osv-scanner
// "also known as" clause, a trivy "Vulnerability" line, or a deprecatedIds list.
var advisoryPattern = regexp.MustCompile(`(?i)(CVE-\d{4}-\d+|GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4}|GO-\d{4}-\d+|RUSTSEC-\d{4}-\d+|PYSEC-\d{4}-\d+|OSV-\d+|[A-Z][A-Z0-9]+-\d{4}-\d+)`)

// osvPackagePattern pulls "<name>@<version>" out of osv-scanner's result
// message ("Package '<name>@<version>' is vulnerable to ..."). The name is
// greedy so scoped npm packages (@scope/name@1.2.3) split at the last '@'.
var osvPackagePattern = regexp.MustCompile(`Package '(.+)@([^@']+)' is vulnerable`)

// extractPackageIdentity returns the dependency identity for an SCA result, or
// nil when the result is not about a package (trivy/grype also report secrets
// and misconfigurations, which carry no package coordinates). Each tool encodes
// identity differently, verified against real tool output:
//   - trivy:       rule.Help.Text "Package:/Installed Version:/Fixed Version:" lines.
//   - grype:       rule.Help.Text "Package:/Version:/Fix Version:" lines (Fix Version
//     is a comma-separated ascending list); PURLs in rule properties.
//   - osv-scanner: package@version in the result message; fixed version in a
//     rule.Help.Markdown "Fixed Versions" table; aliases in the message/deprecatedIds.
func extractPackageIdentity(tool string, rule sarifRule, res sarifResult) *reviewengine.PackageIdentity {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "trivy":
		return trivyPackageIdentity(rule, res)
	case "grype":
		return grypePackageIdentity(rule, res)
	case "osv-scanner", "osv":
		return osvPackageIdentity(rule, res)
	default:
		return nil
	}
}

func trivyPackageIdentity(rule sarifRule, res sarifResult) *reviewengine.PackageIdentity {
	fields := parseKeyValueLines(rule.Help.Text)
	name := fields["package"]
	purl := firstNonEmpty(rule.Properties.PURLs)
	eco := ecosystemFromPURL(purl)
	if eco == "" {
		eco = ecosystemFromManifest(resultURI(res))
	}
	return newPackageIdentity(
		eco,
		name,
		fields["installed version"],
		fields["fixed version"],
		purl,
		collectAdvisories(rule.ID, rule.Help.Text, strings.Join(rule.DeprecatedIDs, " ")),
	)
}

func grypePackageIdentity(rule sarifRule, res sarifResult) *reviewengine.PackageIdentity {
	fields := parseKeyValueLines(rule.Help.Text)
	name := fields["package"]
	purl := firstNonEmpty(rule.Properties.PURLs)
	eco := normalizeEcosystem(fields["type"])
	if eco == "" {
		eco = ecosystemFromPURL(purl)
	}
	if eco == "" {
		eco = ecosystemFromManifest(resultURI(res))
	}
	return newPackageIdentity(
		eco,
		name,
		fields["version"],
		// grype lists every fixed version across affected branches, ascending;
		// the first is the minimal upgrade. Stage 4's resolver makes the
		// authoritative choice — here we record the lowest known fix.
		firstListItem(fields["fix version"]),
		purl,
		// The composite grype rule id embeds both the advisory and the package
		// (CVE-…-pkg); advisoryPattern recovers the clean advisory id.
		collectAdvisories(rule.ID, rule.Help.Text, strings.Join(rule.DeprecatedIDs, " ")),
	)
}

func osvPackageIdentity(rule sarifRule, res sarifResult) *reviewengine.PackageIdentity {
	name, installed := parseOSVPackage(res.Message.Text)
	if name == "" {
		return nil
	}
	return newPackageIdentity(
		ecosystemFromManifest(resultURI(res)),
		name,
		installed,
		osvFixedVersion(rule.Help.Markdown, name),
		"", // osv-scanner SARIF carries no PURL
		collectAdvisories(rule.ID, res.Message.Text, strings.Join(rule.DeprecatedIDs, " ")),
	)
}

// newPackageIdentity assembles a PackageIdentity, or returns nil when there is
// no package name — the signal that a result is not a dependency finding.
func newPackageIdentity(ecosystem, name, installed, fixed, purl string, advisories []string) *reviewengine.PackageIdentity {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	return &reviewengine.PackageIdentity{
		Ecosystem:        strings.TrimSpace(ecosystem),
		Name:             name,
		InstalledVersion: strings.TrimSpace(installed),
		FixedVersion:     strings.TrimSpace(fixed),
		PURL:             strings.TrimSpace(purl),
		Advisories:       advisories,
	}
}

// parseKeyValueLines turns trivy/grype help text ("Key: Value" per line) into a
// lookup keyed by the lowercased, trimmed key.
func parseKeyValueLines(text string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		fields[key] = strings.TrimSpace(value)
	}
	return fields
}

// parseOSVPackage extracts the package name and installed version from an
// osv-scanner result message.
func parseOSVPackage(message string) (name, version string) {
	m := osvPackagePattern.FindStringSubmatch(message)
	if m == nil {
		return "", ""
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
}

// osvFixedVersion reads the fixed version for pkg out of osv-scanner's
// "Fixed Versions" markdown table:
//
//	| Vulnerability ID | Package Name | Fixed Version |
//	| ---------------- | ------------ | ------------- |
//	| GO-2021-0053     | github.com/… | 1.3.2         |
//
// The first row whose package column matches pkg wins. Returns "" when no fix
// is listed (osv-scanner omits the table when no fix exists).
func osvFixedVersion(markdown, pkg string) string {
	pkg = strings.TrimSpace(pkg)
	// Only the "Fixed Versions" section is authoritative: the "Affected Packages"
	// table above it also carries the package name in column two, but its third
	// column is the *installed* version. Track which section we are in.
	inFixed := false
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			inFixed = strings.Contains(strings.ToLower(line), "fixed version")
			continue
		}
		if !inFixed || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitMarkdownRow(line)
		if len(cells) != 3 || !strings.EqualFold(cells[1], pkg) {
			continue
		}
		if v := strings.TrimSpace(cells[2]); v != "" && v != "---" && !strings.EqualFold(v, "Fixed Version") {
			return v
		}
	}
	return ""
}

// splitMarkdownRow splits a "| a | b | c |" table row into its trimmed cells.
func splitMarkdownRow(line string) []string {
	line = strings.Trim(strings.TrimSpace(line), "|")
	parts := strings.Split(line, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// collectAdvisories harvests clean advisory identifiers from any number of raw
// strings, preserving first-seen order and dropping duplicates.
func collectAdvisories(sources ...string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, src := range sources {
		for _, id := range advisoryPattern.FindAllString(src, -1) {
			// Dedup case-insensitively but preserve the identifier's canonical
			// casing (CVE ids are upper, GHSA ids are lower).
			key := strings.ToUpper(id)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// firstListItem returns the first comma-separated item, trimmed.
func firstListItem(csv string) string {
	first, _, _ := strings.Cut(csv, ",")
	return strings.TrimSpace(first)
}

// firstNonEmpty returns the first non-empty, trimmed entry of a slice.
func firstNonEmpty(values []string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// resultURI returns the first artifact location URI on a result.
func resultURI(res sarifResult) string {
	for _, loc := range res.Locations {
		if uri := strings.TrimSpace(loc.PhysicalLocation.ArtifactLocation.URI); uri != "" {
			return uri
		}
	}
	return ""
}

// ecosystemFromPURL maps a package-URL type (pkg:<type>/…) to a normalized
// ecosystem, or "" when the input is not a PURL.
func ecosystemFromPURL(purl string) string {
	purl = strings.TrimSpace(purl)
	if !strings.HasPrefix(purl, "pkg:") {
		return ""
	}
	rest := strings.TrimPrefix(purl, "pkg:")
	typ, _, _ := strings.Cut(rest, "/")
	return normalizeEcosystem(typ)
}

// ecosystemFromManifest maps a manifest/lockfile filename to a normalized
// ecosystem. The URI may be a bare path or a file:// URL; only the base name
// matters.
func ecosystemFromManifest(uri string) string {
	base := strings.ToLower(manifestBase(uri))
	switch base {
	case "go.mod", "go.sum":
		return "go"
	case "package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml":
		return "npm"
	case "cargo.toml", "cargo.lock":
		return "cargo"
	case "requirements.txt", "pipfile", "pipfile.lock", "poetry.lock", "pyproject.toml", "setup.py":
		return "pip"
	default:
		return ""
	}
}

// manifestBase returns the final path element of a URI or path, tolerating
// both slash directions and a file:// scheme.
func manifestBase(uri string) string {
	uri = strings.TrimSpace(uri)
	if i := strings.LastIndexAny(uri, "/\\"); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

// normalizeEcosystem folds the various tokens the scanners and PURLs use for an
// ecosystem into drydock's vocabulary ("go", "npm", "cargo", "pip"). Unknown
// tokens (OS package types such as deb/rpm/apk) pass through lowercased so the
// upgrade service can filter them out by not recognizing them.
func normalizeEcosystem(token string) string {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "go", "golang", "go-module", "gomod":
		return "go"
	case "npm", "node", "javascript":
		return "npm"
	case "cargo", "rust", "rust-crate", "crates.io":
		return "cargo"
	case "pip", "pypi", "python", "python-pkg", "pip-package":
		return "pip"
	default:
		return strings.ToLower(strings.TrimSpace(token))
	}
}
