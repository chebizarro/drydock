package reviewengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ModelIdentity tracks the model identifier actually served by each LLM
// endpoint, keyed by (base URL, credential, configured model) so distinct
// tenants sharing a gateway URL never collide. Configured model names are
// deployment metadata and can go stale; the identity registry is fed from
// ground truth instead:
//
//  1. VerifyEndpoints probes each endpoint's /models listing at startup,
//     warns when the configured name is not served, and seeds the registry
//     when the endpoint serves exactly one model.
//  2. OpenAICompatClient observes the `model` field of every chat-completion
//     response, so the registry converges on whatever actually handled the
//     requests — including gateway-level rerouting.
//
// All methods are safe on a nil receiver, making wiring optional.
type ModelIdentity struct {
	mu     sync.RWMutex
	served map[string]string
}

func NewModelIdentity() *ModelIdentity {
	return &ModelIdentity{served: make(map[string]string)}
}

func identityKey(baseURL, apiKey, configured string) string {
	// The API key is folded in as a short non-reversible hash: two clients
	// hitting the same gateway URL with different credentials may be routed
	// to different deployments and must not share observations.
	sum := sha256.Sum256([]byte(apiKey))
	return baseURL + "|" + hex.EncodeToString(sum[:8]) + "|" + configured
}

// Observe records the model identifier an endpoint reported serving for
// requests configured with the given model name.
func (mi *ModelIdentity) Observe(baseURL, apiKey, configured, served string) {
	if mi == nil {
		return
	}
	served = strings.TrimSpace(served)
	if served == "" {
		return
	}
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.served[identityKey(baseURL, apiKey, configured)] = served
}

// Resolve returns the last observed served model for the endpoint, falling
// back to the configured model name when nothing has been observed yet.
func (mi *ModelIdentity) Resolve(baseURL, apiKey, configured string) string {
	if mi == nil {
		return configured
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	if served, ok := mi.served[identityKey(baseURL, apiKey, configured)]; ok {
		return served
	}
	return configured
}

// VerifyEndpoints probes each distinct endpoint's OpenAI-compatible /models
// listing, logs a warning when a configured model name is not among the
// served models, and seeds the registry with the served identifier when the
// endpoint serves exactly one model. Failures are logged and never fatal.
func (mi *ModelIdentity) VerifyEndpoints(ctx context.Context, httpClient *http.Client, logger *slog.Logger, endpoints ...ModelEndpoint) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	// Group configured endpoints by (base URL, credential) so each distinct
	// endpoint identity is probed exactly once.
	byBase := make(map[string][]ModelEndpoint)
	for _, ep := range endpoints {
		base := strings.TrimSpace(ep.BaseURL)
		if base == "" {
			continue
		}
		group := identityKey(base, ep.APIKey, "")
		byBase[group] = append(byBase[group], ep)
	}

	for _, eps := range byBase {
		base := strings.TrimSpace(eps[0].BaseURL)
		servedIDs, err := listServedModels(ctx, httpClient, base, eps[0].APIKey)
		if err != nil {
			if logger != nil {
				logger.Warn("could not verify served models for LLM endpoint", "base_url", base, "error", err)
			}
			continue
		}
		servedSet := make(map[string]struct{}, len(servedIDs))
		for _, id := range servedIDs {
			servedSet[id] = struct{}{}
		}
		for _, ep := range eps {
			if _, ok := servedSet[ep.Model]; !ok && logger != nil {
				logger.Warn("configured model name is not served by its endpoint",
					"base_url", base,
					"configured_model", ep.Model,
					"served_models", servedIDs)
			}
			if len(servedIDs) == 1 {
				mi.Observe(base, ep.APIKey, ep.Model, servedIDs[0])
			}
		}
	}
}

func listServedModels(ctx context.Context, httpClient *http.Client, baseURL, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("list models: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode models listing: %w", err)
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("models listing is empty")
	}
	return ids, nil
}
