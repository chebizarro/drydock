package depupgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxRegistryResponseBytes = 16 << 20

// registryGet reads only from an operator-selected base URL. Redirects are
// disabled by NewResolver, and response bodies are bounded before decoding.
func (r *Resolver) registryGet(ctx context.Context, ecosystem, suffix, accept string) ([]byte, error) {
	raw := r.bases[ecosystem] + "/" + strings.TrimLeft(suffix, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("create registry request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "drydock-depupgrade/1")
	resp, err := r.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: request %s: %v", ErrRegistryUnavailable, ecosystem, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: %s registry returned HTTP %d", ErrRegistryUnavailable, ecosystem, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s registry returned HTTP %d", ecosystem, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: read %s registry response: %v", ErrRegistryUnavailable, ecosystem, err)
	}
	if len(body) > maxRegistryResponseBytes {
		return nil, fmt.Errorf("%s registry response exceeds %d bytes", ecosystem, maxRegistryResponseBytes)
	}
	return body, nil
}

func escapedPackageName(name string) (string, error) {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n") || name == "." || name == ".." {
		return "", errors.New("invalid package name")
	}
	return url.PathEscape(name), nil
}

func highestStable(ecosystem string, versions []string) string {
	best := ""
	for _, candidate := range versions {
		if !validVersion(ecosystem, candidate) || isPrerelease(ecosystem, candidate) {
			continue
		}
		if best == "" {
			best = candidate
			continue
		}
		cmp, _ := compareVersion(ecosystem, candidate, best)
		if cmp > 0 || (cmp == 0 && candidate < best) {
			best = candidate
		}
	}
	return best
}
