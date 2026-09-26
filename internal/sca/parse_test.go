package sca

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

// TestParseSARIFFindingsResolvesRealSeverity guards DRYDOCK against flattening
// a CRITICAL SCA finding to "high": SARIF 2.1.0 has no level above "error", so
// real severity must be read from properties (GitHub's security-severity CVSS
// score, or a Severity token) and only fall back to the level mapping. It also
// pins the SARIF-spec default level (absent -> "warning" -> medium, not low).
func TestParseSARIFFindingsResolvesRealSeverity(t *testing.T) {
	loc := `"locations":[{"physicalLocation":{"artifactLocation":{"uri":"go.mod"},"region":{"startLine":1}}}]`
	for _, tc := range []struct {
		name  string
		sarif string
		want  string
	}{
		{
			name:  "rule security-severity CVSS critical beats level ceiling",
			sarif: `{"runs":[{"tool":{"driver":{"rules":[{"id":"CVE-1","properties":{"security-severity":"9.8"}}]}},"results":[{"ruleId":"CVE-1","level":"error","message":{"text":"m"},` + loc + `}]}]}`,
			want:  "critical",
		},
		{
			name:  "result properties severity token",
			sarif: `{"runs":[{"tool":{"driver":{"rules":[]}},"results":[{"ruleId":"CVE-2","level":"warning","message":{"text":"m"},"properties":{"severity":"CRITICAL"},` + loc + `}]}]}`,
			want:  "critical",
		},
		{
			name:  "absent level defaults to warning then medium",
			sarif: `{"runs":[{"tool":{"driver":{"rules":[]}},"results":[{"ruleId":"CVE-3","message":{"text":"m"},` + loc + `}]}]}`,
			want:  "medium",
		},
		{
			name:  "error level without properties stays high",
			sarif: `{"runs":[{"tool":{"driver":{"rules":[]}},"results":[{"ruleId":"CVE-4","level":"error","message":{"text":"m"},` + loc + `}]}]}`,
			want:  "high",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := ParseSARIFFindings("trivy", []byte(tc.sarif))
			if err != nil {
				t.Fatalf("ParseSARIFFindings: %v", err)
			}
			if len(findings) != 1 {
				t.Fatalf("expected 1 finding, got %#v", findings)
			}
			if findings[0].Severity != tc.want {
				t.Fatalf("severity = %q, want %q", findings[0].Severity, tc.want)
			}
		})
	}
}

// TestParseSARIFFindingsExtractsPackageIdentity pins per-tool package-identity
// extraction against checked-in SARIF goldens (trivy fs, grype dir:, osv-scanner
// --format sarif). None of these tools ship in the image or CI, so goldens are
// the only way to test this; the fixtures are trimmed but structurally faithful
// to real tool output. Each reports two distinct packages against one go.mod,
// which also guards the dedup fix: before package identity participated in
// finding identity, both collapsed into a single finding.
func TestParseSARIFFindingsExtractsPackageIdentity(t *testing.T) {
	cases := []struct {
		tool string
		file string
		want map[string]reviewengine.PackageIdentity
	}{
		{
			tool: "trivy",
			file: "trivy_go.sarif",
			want: map[string]reviewengine.PackageIdentity{
				"github.com/foo/bar": {Ecosystem: "go", Name: "github.com/foo/bar", InstalledVersion: "1.2.0", FixedVersion: "1.2.4", Advisories: []string{"CVE-2024-1111"}},
				"github.com/baz/qux": {Ecosystem: "go", Name: "github.com/baz/qux", InstalledVersion: "0.9.0", FixedVersion: "0.9.1", Advisories: []string{"CVE-2024-2222"}},
			},
		},
		{
			tool: "grype",
			file: "grype_dir.sarif",
			want: map[string]reviewengine.PackageIdentity{
				"github.com/foo/bar": {Ecosystem: "go", Name: "github.com/foo/bar", InstalledVersion: "1.2.0", FixedVersion: "1.2.4", PURL: "pkg:golang/github.com/foo/bar@1.2.0", Advisories: []string{"CVE-2024-1111"}},
				"github.com/baz/qux": {Ecosystem: "go", Name: "github.com/baz/qux", InstalledVersion: "0.9.0", FixedVersion: "0.9.1", PURL: "pkg:golang/github.com/baz/qux@0.9.0", Advisories: []string{"CVE-2024-2222"}},
			},
		},
		{
			tool: "osv-scanner",
			file: "osv-scanner.sarif",
			want: map[string]reviewengine.PackageIdentity{
				"github.com/gogo/protobuf": {Ecosystem: "go", Name: "github.com/gogo/protobuf", InstalledVersion: "1.3.1", FixedVersion: "1.3.2", Advisories: []string{"CVE-2021-3121", "GO-2021-0053", "GHSA-c3h9-896r-86jm"}},
				"golang.org/x/net":         {Ecosystem: "go", Name: "golang.org/x/net", InstalledVersion: "0.6.0", FixedVersion: "0.7.0", Advisories: []string{"CVE-2022-41717", "GO-2022-1144", "GHSA-xrjj-mj9h-534m"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			findings, err := ParseSARIFFindings(tc.tool, data)
			if err != nil {
				t.Fatalf("ParseSARIFFindings: %v", err)
			}
			if len(findings) != len(tc.want) {
				t.Fatalf("got %d findings, want %d (distinct packages collapsed in dedup?): %#v", len(findings), len(tc.want), findings)
			}
			for _, f := range findings {
				if f.Package == nil {
					t.Fatalf("finding missing package identity: %#v", f)
				}
				want, ok := tc.want[f.Package.Name]
				if !ok {
					t.Fatalf("unexpected package %q", f.Package.Name)
				}
				if !reflect.DeepEqual(*f.Package, want) {
					t.Fatalf("package identity mismatch for %s:\n got %#v\nwant %#v", f.Package.Name, *f.Package, want)
				}
			}
		})
	}
}
