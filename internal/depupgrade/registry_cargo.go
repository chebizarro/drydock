package depupgrade

import (
	"context"
	"encoding/json"
	"fmt"
)

// crates.io's crate JSON API exposes versions[].num and versions[].yanked.
// Unlike Cargo requirement matching, resolution here compares exact published
// versions, so caret requirements do not widen the chosen target.
func (r *Resolver) cargoVersions(ctx context.Context, name string) ([]string, string, error) {
	escaped, err := escapedPackageName(name)
	if err != nil {
		return nil, "", fmt.Errorf("escape Cargo crate: %w", err)
	}
	body, err := r.registryGet(ctx, "cargo", "api/v1/crates/"+escaped, "application/json")
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		Crate struct {
			Name        string `json:"name"`
			NumVersions *int   `json:"num_versions"`
		} `json:"crate"`
		Versions []struct {
			Num    string `json:"num"`
			Yanked *bool  `json:"yanked"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Versions == nil {
		return nil, "", fmt.Errorf("decode Cargo crate: missing or malformed versions")
	}
	if doc.Crate.NumVersions != nil && *doc.Crate.NumVersions != len(doc.Versions) {
		return nil, "", fmt.Errorf("decode Cargo crate: incomplete versions list")
	}
	if doc.Crate.Name != "" && doc.Crate.Name != name {
		return nil, "", fmt.Errorf("decode Cargo crate: package name mismatch")
	}
	versions := make([]string, 0, len(doc.Versions))
	for _, version := range doc.Versions {
		if version.Yanked == nil {
			return nil, "", fmt.Errorf("decode Cargo crate: missing yank status")
		}
		if !*version.Yanked {
			versions = append(versions, version.Num)
		}
	}
	return versions, highestStable("cargo", versions), nil
}
