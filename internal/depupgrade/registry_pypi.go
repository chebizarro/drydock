package depupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var pypiSeparator = regexp.MustCompile(`[-_.]+`)
var pypiName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// PyPI project JSON exposes releases as version -> files. A release with no
// files, or with every file yanked, is not an eligible target.
func (r *Resolver) pypiVersions(ctx context.Context, name string) ([]string, string, error) {
	if !pypiName.MatchString(name) {
		return nil, "", fmt.Errorf("escape PyPI project: invalid package name")
	}
	normalized := strings.ToLower(pypiSeparator.ReplaceAllString(name, "-"))
	escaped, _ := escapedPackageName(normalized)
	body, err := r.registryGet(ctx, "pip", "pypi/"+escaped+"/json", "application/json")
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		Info struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string][]struct {
			Yanked *bool `json:"yanked"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Releases == nil {
		return nil, "", fmt.Errorf("decode PyPI project: missing or malformed releases")
	}
	if doc.Info.Name != "" && strings.ToLower(pypiSeparator.ReplaceAllString(doc.Info.Name, "-")) != normalized {
		return nil, "", fmt.Errorf("decode PyPI project: package name mismatch")
	}
	versions := make([]string, 0, len(doc.Releases))
	for version, files := range doc.Releases {
		for _, file := range files {
			if file.Yanked == nil {
				return nil, "", fmt.Errorf("decode PyPI project: missing yank status")
			}
			if !*file.Yanked {
				versions = append(versions, version)
				break
			}
		}
	}
	// Without a target Python runtime, choose the highest stable non-yanked
	// PEP 440 release, not info.version (which can name a prerelease). The
	// later updater must still check Requires-Python and installability.
	return versions, highestStable("pip", versions), nil
}
