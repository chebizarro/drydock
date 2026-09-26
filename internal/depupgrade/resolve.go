// Package depupgrade resolves scanner-reported fixes against ecosystem registries.
package depupgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"
	pep440 "github.com/aquasecurity/go-pep440-version"
	gomodsemver "golang.org/x/mod/semver"

	"git.sharegap.net/cascadia/drydock/internal/config"
	"git.sharegap.net/cascadia/drydock/internal/reviewengine"
)

const (
	PolicyNextPatch = "next_patch"
	PolicyLatest    = "latest"
	StatusResolved  = "resolved"
	StatusNoFix     = "no_fix_available"
)

var (
	ErrRegistryUnavailable = errors.New("registry unavailable")
	ErrNoPublishedTarget   = errors.New("no eligible published version")
)

// Resolution exposes degraded operation explicitly; a scanner fallback is not
// registry-verified and must not be presented to callers as one. Status is a
// resolver outcome; the later service maps it to its persisted lifecycle state.
type Resolution struct {
	TargetVersion string
	Status        string
	Source        string // registry, scanner_fallback, or none
	Degraded      bool
	Warning       string
}

type Resolver struct {
	client *http.Client
	bases  map[string]string
}

// NewResolver uses operator-owned endpoints. Production TLS checks are performed
// by config.Validate; the client never disables certificate verification.
func NewResolver(cfg config.Config, client *http.Client) (*Resolver, error) {
	bases := map[string]string{
		"go": cfg.GoRegistryURL, "npm": cfg.NPMRegistryURL,
		"cargo": cfg.CargoRegistryURL, "pip": cfg.PyPIRegistryURL,
	}
	for ecosystem, raw := range bases {
		u, err := url.Parse(raw)
		if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("validate %s registry URL: invalid HTTP endpoint", ecosystem)
		}
		if cfg.IsProduction() && u.Scheme != "https" {
			return nil, fmt.Errorf("validate %s registry URL: production requires HTTPS", ecosystem)
		}
		bases[ecosystem] = strings.TrimRight(u.String(), "/")
	}
	if client == nil {
		client = &http.Client{}
	}
	if cfg.IsProduction() && client.Transport != nil {
		transport, ok := client.Transport.(*http.Transport)
		if !ok || (transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify) {
			return nil, errors.New("validate registry client: production requires verified TLS transport")
		}
	}
	copyClient := *client
	// Bound the entire request even when callers supply an unbounded client.
	if copyClient.Timeout == 0 || copyClient.Timeout > 5*time.Second {
		copyClient.Timeout = 5 * time.Second
	}
	// A registry must not redirect a package lookup to an untrusted host.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Resolver{client: &copyClient, bases: bases}, nil
}

// Resolve returns no target for an absent fixed version, even for latest. A
// transport failure degrades to the scanner's fixed version, never silently to
// an empty target. Registry 4xx or malformed metadata are hard failures.
func (r *Resolver) Resolve(ctx context.Context, pkg reviewengine.PackageIdentity, policy string) (Resolution, error) {
	if policy != PolicyNextPatch && policy != PolicyLatest {
		return Resolution{}, fmt.Errorf("resolve dependency: unknown policy %q", policy)
	}
	fixed := strings.TrimSpace(pkg.FixedVersion)
	// SCA tools report Go module versions both with and without the canonical
	// leading v. The Go proxy and updater require the canonical spelling.
	if pkg.Ecosystem == "go" && fixed != "" && !strings.HasPrefix(fixed, "v") && gomodsemver.IsValid("v"+fixed) {
		fixed = "v" + fixed
	}
	if fixed == "" {
		return Resolution{Status: StatusNoFix, Source: "none"}, nil
	}
	if strings.TrimSpace(pkg.Name) == "" {
		return Resolution{}, errors.New("resolve dependency: package name is empty")
	}
	if _, ok := r.bases[pkg.Ecosystem]; !ok {
		return Resolution{}, fmt.Errorf("resolve dependency: unsupported ecosystem %q", pkg.Ecosystem)
	}
	if !validVersion(pkg.Ecosystem, fixed) {
		return Resolution{}, fmt.Errorf("resolve dependency: invalid scanner fixed version %q for %s", fixed, pkg.Ecosystem)
	}

	var versions []string
	var latest string
	var err error
	switch pkg.Ecosystem {
	case "go":
		versions, latest, err = r.goVersions(ctx, pkg.Name, policy)
	case "npm":
		versions, latest, err = r.npmVersions(ctx, pkg.Name)
	case "cargo":
		versions, latest, err = r.cargoVersions(ctx, pkg.Name)
	case "pip":
		versions, latest, err = r.pypiVersions(ctx, pkg.Name)
	}
	if err != nil {
		if errors.Is(err, ErrRegistryUnavailable) && ctx.Err() == nil {
			return Resolution{TargetVersion: fixed, Status: StatusResolved, Source: "scanner_fallback", Degraded: true, Warning: err.Error()}, nil
		}
		return Resolution{}, fmt.Errorf("resolve %s package %q: %w", pkg.Ecosystem, pkg.Name, err)
	}
	if policy == PolicyLatest {
		if latest == "" || !validVersion(pkg.Ecosystem, latest) {
			return Resolution{}, fmt.Errorf("resolve %s package %q: %w (invalid latest)", pkg.Ecosystem, pkg.Name, ErrNoPublishedTarget)
		}
		comparison, _ := compareVersion(pkg.Ecosystem, latest, fixed)
		if comparison < 0 {
			return Resolution{}, fmt.Errorf("resolve %s package %q: %w (latest predates fixed version)", pkg.Ecosystem, pkg.Name, ErrNoPublishedTarget)
		}
		return Resolution{TargetVersion: latest, Status: StatusResolved, Source: "registry"}, nil
	}
	best := ""
	for _, candidate := range versions {
		if !validVersion(pkg.Ecosystem, candidate) || isPrerelease(pkg.Ecosystem, candidate) {
			continue
		}
		cmpFixed, _ := compareVersion(pkg.Ecosystem, candidate, fixed)
		if cmpFixed < 0 {
			continue
		}
		if best == "" {
			best = candidate
			continue
		}
		cmpBest, _ := compareVersion(pkg.Ecosystem, candidate, best)
		if cmpBest < 0 || (cmpBest == 0 && candidate < best) {
			best = candidate
		}
	}
	if best == "" {
		return Resolution{}, fmt.Errorf("resolve %s package %q: %w (at or above %s)", pkg.Ecosystem, pkg.Name, ErrNoPublishedTarget, fixed)
	}
	return Resolution{TargetVersion: best, Status: StatusResolved, Source: "registry"}, nil
}

func validVersion(ecosystem, raw string) bool {
	switch ecosystem {
	case "go":
		return gomodsemver.IsValid(raw)
	case "npm", "cargo":
		_, err := semver.StrictNewVersion(raw)
		return err == nil
	case "pip":
		_, err := pep440.Parse(raw)
		return err == nil
	}
	return false
}

func isPrerelease(ecosystem, raw string) bool {
	switch ecosystem {
	case "go":
		return gomodsemver.Prerelease(raw) != ""
	case "npm", "cargo":
		v, _ := semver.StrictNewVersion(raw)
		return v.Prerelease() != ""
	case "pip":
		v, _ := pep440.Parse(raw)
		return v.IsPreRelease()
	}
	return false
}

func compareVersion(ecosystem, a, b string) (int, error) {
	switch ecosystem {
	case "go":
		return gomodsemver.Compare(a, b), nil
	case "npm", "cargo":
		av, err := semver.StrictNewVersion(a)
		if err != nil {
			return 0, err
		}
		bv, err := semver.StrictNewVersion(b)
		if err != nil {
			return 0, err
		}
		return av.Compare(bv), nil
	case "pip":
		av, err := pep440.Parse(a)
		if err != nil {
			return 0, err
		}
		bv, err := pep440.Parse(b)
		if err != nil {
			return 0, err
		}
		return av.Compare(bv), nil
	}
	return 0, fmt.Errorf("unsupported ecosystem %q", ecosystem)
}
