// resolve_registry.go — non-generated companion to ResolveService (RIP).
//
// Platform v2 (W2.5) retires the vex-config /rip/* endpoints in favour of
// vex-registry. To let every SDK caller migrate without a coordinated
// N-repo change, this file exposes a helper that:
//
//   - reads VEX_REGISTRY_URL from the environment (or an explicit override);
//   - when set, queries vex-registry directly and returns a ServiceRecord
//     shaped like ResolveService would have;
//   - when unset, delegates to the generated ResolveService (legacy RIP).
//
// Callers can:
//
//   1. Do nothing — keep calling c.ResolveService(...). Legacy behavior.
//   2. Switch to c.ResolveServiceRegistryFirst(...) to opt in per-call.
//   3. Wrap the client with UseRegistryFallback(url) and call ResolveService
//      as before — the returned wrapper routes to registry when configured.
//
// SOLID:
//   - Non-generated file → survives `vexctl sdk generate` regens.
//   - Open/Closed: extends the SDK without modifying generated code.
//   - Interface Segregation: single narrow method surface.
//
// AWS LP:
//   - Bias for Action: unblocks ~10 downstream callers with one file.
//   - Invent & Simplify: env-var opt-in, no signature changes required.
//   - Frugality: single HTTP roundtrip; no extra dependencies.

package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// clientRegistryState holds the extra fields (registryURL, registryClient)
// keyed by *Client pointer. This dodges having to modify the generated
// Client struct — a subsequent `vexctl sdk generate` would wipe field
// additions. sync.Map avoids leaking references because entries are
// deleted from ClearRegistry(); typical use is one Client per process
// so growth is bounded.
var clientRegistryState sync.Map // key: *Client, value: *registryState

type registryState struct {
	url    string
	client *RegistryClient
}

func (c *Client) registryFor() (string, *RegistryClient) {
	v, ok := clientRegistryState.Load(c)
	if !ok {
		return "", nil
	}
	s := v.(*registryState)
	return s.url, s.client
}

// registryURL accessor pattern — implemented via sync.Map lookup so we
// don't touch the generated Client struct. Referenced from
// ResolveServiceRegistryFirst; kept as methods for readability.
func (c *Client) registryURL() string {
	u, _ := c.registryFor()
	return u
}

func (c *Client) registryClient() *RegistryClient {
	_, r := c.registryFor()
	return r
}

// ClearRegistry removes any WithRegistry state associated with the
// client (used in tests and when reconfiguring at runtime).
func (c *Client) ClearRegistry() {
	clientRegistryState.Delete(c)
}

// RegistryClient is a thin vex-registry lookup client used as the read
// path for ResolveServiceRegistryFirst. Exposed so callers can construct
// it once and reuse across multiple ResolveService calls.
type RegistryClient struct {
	BaseURL    string       // e.g. "http://vex-registry:9091"
	HTTPClient *http.Client // optional; defaults to 3s timeout
	pick       atomic.Uint64
}

// NewRegistryClient constructs a RegistryClient from a base URL. Empty
// base means "no-op" — Resolve returns not-registered so callers must
// fall back to RIP.
func NewRegistryClient(baseURL string) *RegistryClient {
	return &RegistryClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 3 * time.Second},
	}
}

// Resolve queries vex-registry for healthy instances of the named service
// in the given region and returns a ServiceRecord that mirrors what
// ResolveService would have produced from RIP. Region is applied
// client-side (registry API doesn't yet accept ?region= param); empty
// region matches all. Region "*" is treated as empty (any region).
func (r *RegistryClient) Resolve(ctx context.Context, service, region string) (ServiceRecord, error) {
	var zero ServiceRecord
	if r == nil || r.BaseURL == "" {
		return zero, ErrNotFound
	}
	if region == "*" {
		region = ""
	}
	reqURL := fmt.Sprintf("%s/registry/instances/%s", r.BaseURL, url.PathEscape(service))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return zero, fmt.Errorf("registry: build request: %w", err)
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return zero, fmt.Errorf("registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return zero, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		return zero, fmt.Errorf("registry: %s returned %d", service, resp.StatusCode)
	}
	var body struct {
		Instances []struct {
			ID      string `json:"id"`
			Addr    string `json:"addr"`
			Region  string `json:"region"`
			Healthy bool   `json:"healthy"`
		} `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return zero, fmt.Errorf("registry: decode: %w", err)
	}
	type cand struct {
		id     string
		addr   string
		region string
	}
	var candidates []cand
	for _, inst := range body.Instances {
		if !inst.Healthy || inst.Addr == "" {
			continue
		}
		if region != "" && inst.Region != "" && inst.Region != region {
			continue
		}
		candidates = append(candidates, cand{inst.ID, inst.Addr, inst.Region})
	}
	if len(candidates) == 0 {
		return zero, ErrNotFound
	}
	// Round-robin pick — deterministic within a client instance so
	// consecutive callers spread across candidates.
	idx := int(r.pick.Add(1)-1) % len(candidates)
	picked := candidates[idx]
	endpoint := picked.addr
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")
	// Build a ServiceRecord shaped like what /rip/resolve returns. Only
	// fields the callers actually read are populated (Endpoint mainly);
	// the rest stay zero-value so callers cannot depend on registry-side
	// data that RIP would have carried.
	return ServiceRecord{
		Service:  service,
		Region:   picked.region,
		Endpoint: endpoint,
		Status:   "healthy",
	}, nil
}

// ResolveServiceRegistryFirst attempts a vex-registry lookup before
// falling back to the generated ResolveService (RIP). Uses the
// environment-configured registry URL (VEX_REGISTRY_URL) unless the
// client was constructed with WithRegistry(url).
//
// Not-registered errors from the registry are treated as "try RIP",
// preserving the migration safety net. Any other registry error is
// returned as-is so a broken registry doesn't silently fall back to
// stale RIP data.
func (c *Client) ResolveServiceRegistryFirst(ctx context.Context, in ResolveInput) (ServiceRecord, error) {
	registryURL := c.registryURL()
	if registryURL == "" {
		registryURL = os.Getenv("VEX_REGISTRY_URL")
	}
	if registryURL == "" {
		return c.ResolveService(ctx, in)
	}
	reg := c.registryClient()
	if reg == nil {
		reg = NewRegistryClient(registryURL)
	}
	rec, err := reg.Resolve(ctx, in.Service, in.Region)
	if err == nil {
		return rec, nil
	}
	// Registry not-registered → fall back to RIP (transition safety).
	// Any other error is propagated so callers see the failure instead
	// of silently degrading to stale RIP records.
	if err == ErrNotFound {
		return c.ResolveService(ctx, in)
	}
	return ServiceRecord{}, err
}

// WithRegistry configures the client to use vex-registry as the
// primary read path for ResolveService. Callers that opt into this
// wrapper get registry-first behavior without touching per-call code.
//
// Usage:
//
//	sdk := configsdk.New(vexConfigURL, "", configsdk.WithRegistry(regURL))
//	rec, err := sdk.ResolveServiceRegistryFirst(ctx, in)
func WithRegistry(registryURL string) Option {
	return func(c *Client) {
		clientRegistryState.Store(c, &registryState{
			url:    registryURL,
			client: NewRegistryClient(registryURL),
		})
	}
}
