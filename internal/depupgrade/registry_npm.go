package depupgrade

import (
	"context"
	"encoding/json"
	"fmt"
)

// npm packument shape is documented by npm/registry's package-metadata API.
// The abbreviated representation omits readmes and still carries dist-tags,
// versions, and each version's deprecation marker.
func (r *Resolver) npmVersions(ctx context.Context, name string) ([]string, string, error) {
	escaped, err := escapedPackageName(name)
	if err != nil {
		return nil, "", fmt.Errorf("escape npm package: %w", err)
	}
	body, err := r.registryGet(ctx, "npm", escaped, "application/vnd.npm.install-v1+json")
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		Name     string            `json:"name"`
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Deprecated string `json:"deprecated"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Versions == nil || doc.DistTags == nil {
		return nil, "", fmt.Errorf("decode npm packument: missing or malformed versions/dist-tags")
	}
	if doc.Name != "" && doc.Name != name {
		return nil, "", fmt.Errorf("decode npm packument: package name mismatch")
	}
	versions := make([]string, 0, len(doc.Versions))
	for v, metadata := range doc.Versions {
		if metadata.Deprecated == "" {
			versions = append(versions, v)
		}
	}
	latest := doc.DistTags["latest"]
	if metadata, ok := doc.Versions[latest]; !ok || metadata.Deprecated != "" {
		latest = ""
	}
	return versions, latest, nil
}
