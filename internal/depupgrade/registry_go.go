package depupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	gomodsemver "golang.org/x/mod/semver"
)

// Go's proxy list is newline-delimited tagged versions, never pseudo-versions.
// Retractions are declared in the latest module's go.mod, not in the list.
func (r *Resolver) goVersions(ctx context.Context, name, policy string) ([]string, string, error) {
	modulePath, err := module.EscapePath(name) // includes Go's ! case encoding
	if err != nil {
		return nil, "", fmt.Errorf("escape Go module path: %w", err)
	}
	list, err := r.registryGet(ctx, "go", modulePath+"/@v/list", "text/plain")
	if err != nil {
		return nil, "", err
	}
	body, err := r.registryGet(ctx, "go", modulePath+"/@latest", "application/json")
	if err != nil {
		return nil, "", err
	}
	var info struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(body, &info); err != nil || !gomodsemver.IsValid(info.Version) {
		return nil, "", fmt.Errorf("decode Go proxy latest: invalid Info response")
	}
	mod, err := r.registryGet(ctx, "go", modulePath+"/@v/"+info.Version+".mod", "text/plain")
	if err != nil {
		return nil, "", err
	}
	file, err := modfile.ParseLax("go.mod", mod, nil)
	if err != nil {
		return nil, "", fmt.Errorf("parse Go module retractions: %w", err)
	}
	available := make([]string, 0)
	for _, version := range strings.Fields(string(list)) {
		if gomodsemver.IsValid(version) && !goRetracted(version, file.Retract) {
			available = append(available, version)
		}
	}
	if policy == PolicyNextPatch {
		return available, "", nil
	}
	// Go's `latest` query prefers the highest release, then highest prerelease;
	// only when there are no tags does it use the proxy's pseudo-version.
	stable := highestStable("go", available)
	if stable != "" {
		return nil, stable, nil
	}
	pre := ""
	for _, version := range available {
		if pre == "" || gomodsemver.Compare(version, pre) > 0 {
			pre = version
		}
	}
	if pre != "" {
		return nil, pre, nil
	}
	if goRetracted(info.Version, file.Retract) {
		return nil, "", nil
	}
	return nil, info.Version, nil
}

func goRetracted(version string, retractions []*modfile.Retract) bool {
	for _, retract := range retractions {
		if gomodsemver.Compare(version, retract.Low) >= 0 && gomodsemver.Compare(version, retract.High) <= 0 {
			return true
		}
	}
	return false
}
