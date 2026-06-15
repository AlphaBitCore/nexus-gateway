// Package oidcdisco resolves the OIDC endpoints (authorization, token, JWKS)
// an external IdP's login flow needs from that IdP's issuer, fetching the
// `<issuer>/.well-known/openid-configuration` discovery document and caching
// it per issuer with a TTL.
//
// The admin Add-IdP form intentionally collects only the issuer URL (its help
// text promises "Nexus uses its /.well-known/openid-configuration for
// discovery"), so the saved config carries no authorize/token/jwks endpoints.
// Both the interactive login path (`authserver/login`) and the connectivity
// probe (`identity/idptest`) resolve those endpoints through this one package
// so discovery behaviour stays identical across them.
//
// Explicit endpoint values always win: if an admin pins `authorizeUrl`,
// `tokenUrl`, or `jwksUri` on the config, the pinned value is preserved and
// discovery only fills the remaining gaps. When every endpoint is already
// present no network call is made.
package oidcdisco

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

const (
	// DefaultTTL is how long a discovered document is reused before re-fetch.
	// OIDC endpoints rotate rarely; 10 minutes keeps the login path off the
	// network on the hot path while still picking up an IdP endpoint change
	// within a reasonable window.
	DefaultTTL = 10 * time.Minute
	// defaultTimeout bounds a single discovery fetch.
	defaultTimeout = 10 * time.Second
	// maxDocBytes caps the discovery document read to defend against a hostile
	// or misbehaving issuer streaming an unbounded body.
	maxDocBytes = 1 << 20
)

// Endpoints holds the three OIDC endpoints the login flow consumes. The
// authorization endpoint is needed by the SSO-start redirect; the token and
// JWKS endpoints are needed by the callback's code exchange and ID-token
// verification.
type Endpoints struct {
	AuthorizeURL string
	TokenURL     string
	JwksURI      string
	// EndSessionURL is the IdP's RP-initiated logout endpoint
	// (`end_session_endpoint`). Optional — empty when the IdP's discovery
	// document omits it; only the logout flow consumes it, so it is not part of
	// Complete().
	EndSessionURL string
}

// Complete reports whether all three endpoints are populated, in which case
// discovery is unnecessary.
func (e Endpoints) Complete() bool {
	return e.AuthorizeURL != "" && e.TokenURL != "" && e.JwksURI != ""
}

// discoveryDoc is the subset of the OIDC Discovery 1.0 document this package
// consumes.
type discoveryDoc struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JwksURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

type cacheEntry struct {
	eps       Endpoints
	fetchedAt time.Time
}

// Resolver fetches and caches OIDC discovery documents per issuer. The zero
// value is not usable; construct one with NewResolver.
type Resolver struct {
	client *http.Client
	ttl    time.Duration
	now    func() time.Time
	// checkHost is the SSRF pre-check applied to an issuer host before fetch.
	// NewResolver wires validatePublicHost (rejects non-public addresses);
	// nil disables the pre-check (tests that point at a loopback httptest
	// server, which already inject a client without the dial-time guard).
	checkHost func(ctx context.Context, host string) error

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// NewResolver builds a Resolver with the default cache TTL and per-fetch
// timeout. A single Resolver is safe for concurrent use and is intended to be
// shared across the SSO-start and OIDC-callback handlers so a document fetched
// on the start leg is reused on the return leg.
//
// SSRF defense: the issuer URL is admin-configured, so a malicious or
// compromised admin could point it at an internal address to make the Control
// Plane fetch `http://169.254.169.254/...` (cloud metadata) or an in-cluster
// service. The HTTP client below installs a dial-time Control hook that rejects
// any connection to a loopback / private / link-local / unspecified IP, which
// also defeats DNS-rebinding (the check runs on the actual dialed address, not a
// pre-resolved one). An OIDC issuer is external by nature — a private/loopback
// issuer is never legitimate — so the guard comes from the admin-egress
// chokepoint as ExternalOnly (FIX-4 consistency follow-up; behaviour-identical
// to the prior BlockPrivateDialControl). fetch() additionally pre-validates via
// a DNS lookup so the common case fails fast with a clear error before any
// socket is opened.
func NewResolver(opts ...Option) *Resolver {
	dialer := &net.Dialer{Control: nexushttp.AdminEgressDialControl(nexushttp.AdminEgressExternalOnly)}
	transport := &http.Transport{DialContext: dialer.DialContext}
	r := &Resolver{
		client:    &http.Client{Timeout: defaultTimeout, Transport: transport},
		ttl:       DefaultTTL,
		now:       time.Now,
		checkHost: validatePublicHost,
		cache:     make(map[string]cacheEntry),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Option customizes a Resolver at construction.
type Option func(*Resolver)

// WithInsecureSkipHostCheck disables the SSRF host guard (both the pre-fetch
// lookup AND the dial-time private-IP block) so the resolver can reach a
// loopback / private discovery endpoint. It exists ONLY for tests that run an
// in-process httptest discovery server on 127.0.0.1; production must never use
// it. Named "Insecure" so a misuse is obvious in review and greppable.
func WithInsecureSkipHostCheck() Option {
	return func(r *Resolver) {
		r.checkHost = nil
		r.client = &http.Client{Timeout: defaultTimeout}
	}
}

// validatePublicHost resolves host and returns an error if it has no public IP
// to connect to: every resolved address must be public. A DNS failure (zero
// addresses) is NOT treated as an SSRF block — it returns nil so fetch proceeds
// and surfaces the real network error (DNS failure ≠
// SSRF). The dial-time Control hook remains the authoritative guard against
// rebinding; this lookup is the fail-fast pre-check.
func validatePublicHost(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if nexushttp.IsDisallowedIP(ip) {
			return fmt.Errorf("oidcdisco: issuer host %s is a non-public address (SSRF guard)", ip)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		// DNS failure: not an SSRF block — let fetch proceed and fail with the
		// real network error (F-0270 contract: DNS failure ≠ SSRF). Returning
		// err here would mis-classify a resolution failure as an SSRF block, so
		// nilerr's "return the error" suggestion is wrong for this call site.
		return nil //nolint:nilerr // F-0270: DNS resolution failure must NOT be reported as an SSRF block
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		if nexushttp.IsDisallowedIP(ip) {
			return fmt.Errorf("oidcdisco: issuer host %s resolves to a non-public address %s (SSRF guard)", host, ip)
		}
	}
	return nil
}

// Resolve returns the endpoints for issuer, preserving every value already
// present in have and filling the rest from the issuer's discovery document.
// It performs no network call when have is already Complete. The returned
// error is non-nil only when discovery was required (some endpoint missing)
// but the document could not be fetched or parsed; the caller decides whether
// that is fatal (login bounce) or surfaced (probe error).
func (r *Resolver) Resolve(ctx context.Context, issuer string, have Endpoints) (Endpoints, error) {
	if have.Complete() {
		return have, nil
	}
	if strings.TrimSpace(issuer) == "" {
		return have, fmt.Errorf("oidcdisco: issuer is empty and endpoints are incomplete")
	}

	disc, err := r.discover(ctx, issuer)
	if err != nil {
		return have, err
	}

	out := have
	if out.AuthorizeURL == "" {
		out.AuthorizeURL = disc.AuthorizeURL
	}
	if out.TokenURL == "" {
		out.TokenURL = disc.TokenURL
	}
	if out.JwksURI == "" {
		out.JwksURI = disc.JwksURI
	}
	// end_session is never config-pinned (no admin field), so it always comes
	// from discovery; may legitimately stay empty if the IdP omits it.
	if out.EndSessionURL == "" {
		out.EndSessionURL = disc.EndSessionURL
	}
	return out, nil
}

// discover returns the issuer's discovered endpoints, serving a fresh cache
// entry when one exists and fetching + caching otherwise. The cache stores the
// raw discovery result (not a caller's merged view), so a caller's explicit
// overrides never pollute another caller's lookup.
func (r *Resolver) discover(ctx context.Context, issuer string) (Endpoints, error) {
	if eps, ok := r.cached(issuer); ok {
		return eps, nil
	}
	eps, err := r.fetch(ctx, issuer)
	if err != nil {
		return Endpoints{}, err
	}
	r.store(issuer, eps)
	return eps, nil
}

func (r *Resolver) cached(issuer string) (Endpoints, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[issuer]
	if !ok || r.now().Sub(e.fetchedAt) > r.ttl {
		return Endpoints{}, false
	}
	return e.eps, true
}

func (r *Resolver) store(issuer string, eps Endpoints) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[issuer] = cacheEntry{eps: eps, fetchedAt: r.now()}
}

func (r *Resolver) fetch(ctx context.Context, issuer string) (Endpoints, error) {
	u, err := url.ParseRequestURI(issuer)
	if err != nil {
		return Endpoints{}, fmt.Errorf("oidcdisco: issuer is not a valid URL: %w", err)
	}
	// Only http(s) issuers are valid OIDC issuers; reject file://, gopher://,
	// etc. so a non-HTTP scheme can never reach the fetcher (SSRF hardening).
	if u.Scheme != "http" && u.Scheme != "https" {
		return Endpoints{}, fmt.Errorf("oidcdisco: issuer scheme %q is not http(s)", u.Scheme)
	}
	// SSRF pre-check: refuse an issuer whose host is / resolves to a
	// non-public address before opening any socket. The dial-time Control hook
	// is the authoritative guard (catches rebinding); this is the fail-fast path.
	if r.checkHost != nil {
		if err := r.checkHost(ctx, u.Hostname()); err != nil {
			return Endpoints{}, err
		}
	}
	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return Endpoints{}, fmt.Errorf("oidcdisco: build discovery request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Endpoints{}, fmt.Errorf("oidcdisco: discovery fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Endpoints{}, fmt.Errorf("oidcdisco: discovery returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes))
	if err != nil {
		return Endpoints{}, fmt.Errorf("oidcdisco: discovery read: %w", err)
	}
	var doc discoveryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return Endpoints{}, fmt.Errorf("oidcdisco: discovery parse: %w", err)
	}
	return Endpoints{
		AuthorizeURL:  doc.AuthorizationEndpoint,
		TokenURL:      doc.TokenEndpoint,
		JwksURI:       doc.JwksURI,
		EndSessionURL: doc.EndSessionEndpoint,
	}, nil
}
