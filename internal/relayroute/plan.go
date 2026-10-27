// Package relayroute turns the relays a repository declares in its kind-30617
// announcement into a bounded subscription plan.
//
// Customer state (the announcement) must never be able to open unbounded or
// unwanted egress, so every relay passes through Policy: scheme, host class,
// allow/deny suffixes, a per-repository cap, and a global cap. The plan is
// deterministic for a given input so resubscription decisions are stable.
package relayroute

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"git.sharegap.net/cascadia/drydock/internal/scope"
)

const (
	DefaultMaxPerRepository = 3
	DefaultMaxTotal         = 32
)

// Policy bounds which declared relays may be subscribed.
type Policy struct {
	// MaxPerRepository caps relays taken from one announcement, in declaration
	// order. Zero disables customer relays entirely; negative means default.
	MaxPerRepository int
	// MaxTotal caps distinct customer relays across all repositories. Zero
	// disables customer relays entirely; negative means default.
	MaxTotal int
	// AllowInsecure permits ws:// and loopback/private/link-local hosts. It is
	// meant for development only.
	AllowInsecure bool
	// Allow, when non-empty, admits only hosts equal to or ending in one of
	// these suffixes (case-insensitive, leading dot optional).
	Allow []string
	// Deny rejects hosts equal to or ending in one of these suffixes. Deny wins
	// over Allow.
	Deny []string
}

func (p Policy) normalized() Policy {
	if p.MaxPerRepository < 0 {
		p.MaxPerRepository = DefaultMaxPerRepository
	}
	if p.MaxTotal < 0 {
		p.MaxTotal = DefaultMaxTotal
	}
	p.Allow = normalizeSuffixes(p.Allow)
	p.Deny = normalizeSuffixes(p.Deny)
	return p
}

// Repository is one monitored repository with the relays its announcement
// declares, in declaration order.
type Repository struct {
	Ref    scope.RepositoryRef
	Relays []string
}

// Rejection records a declared relay the policy refused and why.
type Rejection struct {
	Repository string
	Relay      string
	Reason     string
}

// Plan is the bounded routing result.
type Plan struct {
	// Relays maps each admitted customer relay URL to the repositories to
	// subscribe there, sorted by address. Base relays never appear here.
	Relays map[string][]scope.RepositoryRef
	// Rejected lists every declared relay that was refused or truncated.
	Rejected []Rejection
	// Truncated reports that MaxTotal cut admitted relays.
	Truncated bool
}

// RelayURLs returns the admitted relays in deterministic order.
func (p Plan) RelayURLs() []string {
	urls := make([]string, 0, len(p.Relays))
	for u := range p.Relays {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	return urls
}

// Build computes the plan. base relays are already subscribed with general
// filters and are therefore excluded from the customer set.
func Build(repos []Repository, base []string, policy Policy) Plan {
	policy = policy.normalized()
	plan := Plan{Relays: map[string][]scope.RepositoryRef{}}
	if policy.MaxPerRepository == 0 || policy.MaxTotal == 0 {
		for _, repo := range repos {
			for _, relay := range repo.Relays {
				plan.Rejected = append(plan.Rejected, Rejection{repo.Ref.Address, relay, "customer relays disabled"})
			}
		}
		return plan
	}
	baseSet := make(map[string]struct{}, len(base))
	for _, raw := range base {
		if u, err := NormalizeRelayURL(raw); err == nil {
			baseSet[u] = struct{}{}
		}
	}

	type candidate struct {
		repos map[string]scope.RepositoryRef
	}
	candidates := map[string]*candidate{}

	sorted := append([]Repository(nil), repos...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Ref.Address < sorted[j].Ref.Address })
	for _, repo := range sorted {
		taken := 0
		seen := map[string]struct{}{}
		for _, raw := range repo.Relays {
			u, err := NormalizeRelayURL(raw)
			if err != nil {
				plan.Rejected = append(plan.Rejected, Rejection{repo.Ref.Address, raw, err.Error()})
				continue
			}
			if _, dup := seen[u]; dup {
				continue
			}
			seen[u] = struct{}{}
			if _, isBase := baseSet[u]; isBase {
				continue // already covered by the general subscription
			}
			if reason := policy.refuse(u); reason != "" {
				plan.Rejected = append(plan.Rejected, Rejection{repo.Ref.Address, u, reason})
				continue
			}
			if taken >= policy.MaxPerRepository {
				plan.Rejected = append(plan.Rejected, Rejection{repo.Ref.Address, u, "per-repository relay cap"})
				continue
			}
			taken++
			c, ok := candidates[u]
			if !ok {
				c = &candidate{repos: map[string]scope.RepositoryRef{}}
				candidates[u] = c
			}
			c.repos[repo.Ref.Address] = repo.Ref
		}
	}

	// Global cap: keep the relays that serve the most repositories; ties by URL.
	urls := make([]string, 0, len(candidates))
	for u := range candidates {
		urls = append(urls, u)
	}
	sort.Slice(urls, func(i, j int) bool {
		li, lj := len(candidates[urls[i]].repos), len(candidates[urls[j]].repos)
		if li != lj {
			return li > lj
		}
		return urls[i] < urls[j]
	})
	for i, u := range urls {
		if i >= policy.MaxTotal {
			plan.Truncated = true
			for address := range candidates[u].repos {
				plan.Rejected = append(plan.Rejected, Rejection{address, u, "global relay cap"})
			}
			continue
		}
		refs := make([]scope.RepositoryRef, 0, len(candidates[u].repos))
		for _, ref := range candidates[u].repos {
			refs = append(refs, ref)
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Address < refs[j].Address })
		plan.Relays[u] = refs
	}
	sort.Slice(plan.Rejected, func(i, j int) bool {
		if plan.Rejected[i].Repository != plan.Rejected[j].Repository {
			return plan.Rejected[i].Repository < plan.Rejected[j].Repository
		}
		return plan.Rejected[i].Relay < plan.Rejected[j].Relay
	})
	return plan
}

// refuse returns a non-empty reason when the policy rejects the normalized URL.
func (p Policy) refuse(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return "invalid relay url"
	}
	host := strings.ToLower(parsed.Hostname())
	if !p.AllowInsecure {
		if parsed.Scheme != "wss" {
			return "insecure scheme"
		}
		if isLocalOrPrivateHost(host) {
			return "loopback or private host"
		}
	}
	if matchesSuffix(host, p.Deny) {
		return "denied host"
	}
	if len(p.Allow) > 0 && !matchesSuffix(host, p.Allow) {
		return "host not in allowlist"
	}
	return ""
}

// NormalizeRelayURL canonicalizes a relay URL: ws/wss scheme, lowercase
// scheme and host, no trailing slash on an empty path, no fragment or
// credentials.
func NormalizeRelayURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty relay url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid relay url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "ws" && scheme != "wss" {
		return "", fmt.Errorf("relay url must use ws:// or wss://")
	}
	if u.Hostname() == "" {
		return "", errors.New("relay url has no host")
	}
	if u.User != nil {
		return "", errors.New("relay url must not carry credentials")
	}
	u.Scheme = scheme
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawQuery = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func isLocalOrPrivateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	// A zone identifier (fe80::1%en0) names a local interface by definition
	// and also defeats net.ParseIP; treat any zoned literal as local.
	if strings.Contains(host, "%") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func normalizeSuffixes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimPrefix(s, ".")
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func matchesSuffix(host string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
