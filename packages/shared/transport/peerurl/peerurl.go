// Package peerurl resolves a peer Nexus service's base URLs from the Hub.
//
// A service never configures another service's address: each service reports
// its own publicURL (external clients + the Agent) and privateURL (internal
// service-to-service address, auto-derived from the primary outbound IPv4
// when not overridden) to the Thing Registry as staticInfo, and peers resolve
// the reported value from the Hub over
// GET /api/internal/things/service-url/:thing_type (service-token Bearer).
//
// Resolver contract:
//   - Lazy: nothing is fetched at construction; the first use resolves. The
//     Hub starts first in every deployment recipe and URLs are stable after
//     boot, so in practice the first call resolves and the cache serves the
//     rest.
//   - In-memory cache: positive results are cached per thing type and
//     refreshed after RefreshTTL (a moved peer converges without a restart).
//   - Error + retry-next-use: an unreachable Hub or a not-yet-reported URL
//     returns an error — never a silent fallback or hardcoded default. A
//     short negative-cache window keeps a hot caller from hammering the Hub
//     while a peer is still inside its post-registration static_info push
//     (~500ms) or the Hub is briefly down.
package peerurl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// Default cache windows. Positive entries refresh lazily after RefreshTTL —
// the stale value keeps serving until a refresh succeeds (URLs are stable
// post-boot; convergence within minutes is sufficient for a moved peer).
// Negative results retry after NegativeTTL.
const (
	DefaultRefreshTTL  = 5 * time.Minute
	DefaultNegativeTTL = 5 * time.Second
)

// ErrNotReported marks "the Hub answered, but no Thing of this type has
// reported a URL yet" — expected during the peer's boot window; retry next use.
var ErrNotReported = fmt.Errorf("peer service URL not reported yet")

// Resolver resolves peer service base URLs from the Hub with lazy in-memory
// caching. Safe for concurrent use.
type Resolver struct {
	hubURL      string // Hub base URL (bootstrap config — the one URL a service must know)
	token       string // INTERNAL_SERVICE_TOKEN, sent as Bearer
	client      *http.Client
	refreshTTL  time.Duration
	negativeTTL time.Duration

	mu    sync.Mutex
	cache map[string]*entry
}

type entry struct {
	privateURL string
	publicURL  string
	fetchedAt  time.Time
	// retryAt throttles refresh attempts while a stale positive value is
	// being served: after a FAILED refresh the stale entry keeps serving
	// fetch-free until retryAt, so an unreachable (black-holed) Hub cannot
	// add fetch latency to every caller. Zero = no throttle.
	retryAt time.Time
	err     error // non-nil = negative entry
}

// Option customizes a Resolver.
type Option func(*Resolver)

// WithHTTPClient overrides the HTTP client (tests; custom timeouts).
func WithHTTPClient(c *http.Client) Option { return func(r *Resolver) { r.client = c } }

// WithRefreshTTL overrides the positive-entry refresh interval.
func WithRefreshTTL(d time.Duration) Option { return func(r *Resolver) { r.refreshTTL = d } }

// WithNegativeTTL overrides the failure retry window.
func WithNegativeTTL(d time.Duration) Option { return func(r *Resolver) { r.negativeTTL = d } }

// New builds a Resolver against the Hub base URL with the internal service
// token. Both must be non-empty for the resolver to work; they are validated
// at call time (not here) so construction can happen before config is final.
func New(hubURL, token string, opts ...Option) *Resolver {
	r := &Resolver{
		hubURL:      strings.TrimRight(hubURL, "/"),
		token:       token,
		client:      &http.Client{Timeout: 5 * time.Second},
		refreshTTL:  DefaultRefreshTTL,
		negativeTTL: DefaultNegativeTTL,
		cache:       map[string]*entry{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// PrivateURL returns the peer's reported internal service-to-service base URL.
func (r *Resolver) PrivateURL(ctx context.Context, thingType string) (string, error) {
	e, err := r.resolve(ctx, thingType)
	if err != nil {
		return "", err
	}
	if e.privateURL == "" {
		r.expireSoon(thingType)
		return "", fmt.Errorf("%w: %s reported no privateUrl", ErrNotReported, thingType)
	}
	return e.privateURL, nil
}

// PublicURL returns the peer's reported external base URL.
func (r *Resolver) PublicURL(ctx context.Context, thingType string) (string, error) {
	e, err := r.resolve(ctx, thingType)
	if err != nil {
		return "", err
	}
	if e.publicURL == "" {
		r.expireSoon(thingType)
		return "", fmt.Errorf("%w: %s reported no publicUrl", ErrNotReported, thingType)
	}
	return e.publicURL, nil
}

// expireSoon flips the type's positive entry into the stale+throttle state so
// the next refresh happens after negativeTTL instead of refreshTTL. Used when
// a caller needed a URL kind the peer has not reported yet (boot window,
// mixed-version rolling deploy): the missing kind should be re-checked on the
// NEGATIVE cadence, while any good kind keeps serving fetch-free meanwhile.
func (r *Resolver) expireSoon(thingType string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.cache[thingType]
	if e == nil || e.err != nil {
		return
	}
	now := time.Now()
	if now.Sub(e.fetchedAt) >= r.refreshTTL && now.Before(e.retryAt) {
		return // already stale+throttled
	}
	r.cache[thingType] = &entry{
		privateURL: e.privateURL,
		publicURL:  e.publicURL,
		fetchedAt:  now.Add(-r.refreshTTL),
		retryAt:    now.Add(r.negativeTTL),
	}
}

// resolve returns the cached entry for thingType, fetching from the Hub when
// the cache is empty, a positive entry is older than refreshTTL (and past any
// retryAt throttle), or a negative entry is older than negativeTTL. A failed
// refresh of a still-held positive entry serves the stale value (URLs are
// stable; availability wins) and arms retryAt so subsequent calls keep
// serving stale WITHOUT refetching until negativeTTL elapses — an
// unreachable Hub must not add fetch latency to every caller.
func (r *Resolver) resolve(ctx context.Context, thingType string) (*entry, error) {
	now := time.Now()

	r.mu.Lock()
	cached := r.cache[thingType]
	if cached != nil {
		if cached.err == nil && (now.Sub(cached.fetchedAt) < r.refreshTTL || now.Before(cached.retryAt)) {
			r.mu.Unlock()
			return cached, nil
		}
		if cached.err != nil && now.Sub(cached.fetchedAt) < r.negativeTTL {
			r.mu.Unlock()
			return nil, cached.err
		}
	}
	r.mu.Unlock()

	fresh, err := r.fetch(ctx, thingType)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// A concurrent resolver may have installed a fresh positive entry
		// between our snapshot and this failure — never clobber it.
		if cur := r.cache[thingType]; cur != nil && cur != cached && cur.err == nil {
			return cur, nil
		}
		// Keep serving a previously-good value on refresh failure — and
		// throttle the next attempt (write a fresh copy; the map slot may
		// have been raced by a concurrent resolver). Only install a
		// negative entry when we never had a good one.
		if cached != nil && cached.err == nil {
			r.cache[thingType] = &entry{
				privateURL: cached.privateURL,
				publicURL:  cached.publicURL,
				fetchedAt:  cached.fetchedAt,
				retryAt:    time.Now().Add(r.negativeTTL),
			}
			return cached, nil
		}
		// A canceled/expired CALLER context is that caller's failure, not
		// the Hub's — don't poison the shared cache with a negative entry
		// every other caller would then see for negativeTTL.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		r.cache[thingType] = &entry{err: err, fetchedAt: time.Now()}
		return nil, err
	}
	fresh.fetchedAt = time.Now()
	r.cache[thingType] = fresh
	return fresh, nil
}

// URLs returns both reported base URLs for the peer in ONE cache/Hub
// resolution — callers that need public and private together (e.g. the
// webhook trusted-bases provider) should prefer this over two calls.
// Either value may be empty when the peer did not report that kind.
func (r *Resolver) URLs(ctx context.Context, thingType string) (publicURL, privateURL string, err error) {
	e, err := r.resolve(ctx, thingType)
	if err != nil {
		return "", "", err
	}
	return e.publicURL, e.privateURL, nil
}

func (r *Resolver) fetch(ctx context.Context, thingType string) (*entry, error) {
	if r.hubURL == "" || r.token == "" {
		return nil, fmt.Errorf("peerurl: hub URL and internal service token are required")
	}
	url := r.hubURL + "/api/internal/things/service-url/" + thingType
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("peerurl: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("peerurl: hub fetch: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotReported, thingType)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("peerurl: hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		ThingType  string `json:"thingType"`
		PrivateURL string `json:"privateUrl"`
		PublicURL  string `json:"publicUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("peerurl: decode response: %w", err)
	}
	return &entry{
		privateURL: strings.TrimRight(payload.PrivateURL, "/"),
		publicURL:  strings.TrimRight(payload.PublicURL, "/"),
	}, nil
}
