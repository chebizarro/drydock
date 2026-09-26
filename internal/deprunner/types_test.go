package deprunner

import (
	"strings"
	"testing"
)

func TestUpdateRequestValidate(t *testing.T) {
	base := UpdateRequest{
		Ecosystem: EcosystemGo,
		Package:   "example.com/foo",
		ToVersion: "v1.2.3",
		Manifests: []ManifestFile{{Path: "go.mod", Content: "module x\n"}},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(r *UpdateRequest)
		wantErr string
	}{
		{"unsupported ecosystem", func(r *UpdateRequest) { r.Ecosystem = "maven" }, "unsupported ecosystem"},
		{"empty package", func(r *UpdateRequest) { r.Package = " " }, "package is required"},
		{"empty version", func(r *UpdateRequest) { r.ToVersion = "" }, "to_version is required"},
		{"no manifests", func(r *UpdateRequest) { r.Manifests = nil }, "manifests are required"},
		{"absolute path", func(r *UpdateRequest) { r.Manifests[0].Path = "/etc/go.mod" }, "must be relative"},
		{"traversal path", func(r *UpdateRequest) { r.Manifests[0].Path = "../../go.mod" }, "traversal"},
		{"windows abs path", func(r *UpdateRequest) { r.Manifests[0].Path = `C:\go.mod` }, "must be relative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := base
			r.Manifests = append([]ManifestFile(nil), base.Manifests...)
			tt.mutate(&r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestIsSupportedEcosystem(t *testing.T) {
	for _, eco := range SupportedEcosystems() {
		if !IsSupportedEcosystem(eco) {
			t.Fatalf("%q should be supported", eco)
		}
	}
	if IsSupportedEcosystem("nuget") {
		t.Fatal("nuget should not be supported")
	}
}
