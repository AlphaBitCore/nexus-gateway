package peerurl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hubStub is a fake Hub service-url endpoint. Every field can be swapped
// mid-test to simulate the Hub changing behaviour (peer reports, Hub outage).
type hubStub struct {
	t     *testing.T
	hits  atomic.Int64
	serve atomic.Pointer[func(w http.ResponseWriter, r *http.Request)]
	srv   *httptest.Server
}

func newHubStub(t *testing.T) *hubStub {
	t.Helper()
	h := &hubStub{t: t}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		(*h.serve.Load())(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hubStub) set(fn func(w http.ResponseWriter, r *http.Request)) { h.serve.Store(&fn) }

// serveURLs answers 200 with the given private/public URLs and asserts the
// wire contract (path, bearer token) the Hub enforces.
func (h *hubStub) serveURLs(thingType, privateURL, publicURL string) {
	t := h.t
	h.set(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/internal/things/service-url/"+thingType; got != want {
			t.Errorf("request path = %q; want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q; want the internal service token as Bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"thingType":"` + thingType + `","privateUrl":"` + privateURL + `","publicUrl":"` + publicURL + `"}`))
	})
}

func (h *hubStub) serveStatus(code int, body string) {
	h.set(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})
}

// First use resolves from the Hub and caches: the second call within the
// refresh TTL is served from memory — no second HTTP round-trip.
func TestResolver_FirstUseResolvesThenCaches(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050", "https://gw.example.com")
	r := New(hub.srv.URL, "test-token")

	pub, err := r.PublicURL(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatalf("PublicURL: %v", err)
	}
	if pub != "https://gw.example.com" {
		t.Fatalf("PublicURL = %q; want the Hub-reported public URL", pub)
	}
	priv, err := r.PrivateURL(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatalf("PrivateURL: %v", err)
	}
	if priv != "http://10.0.0.5:3050" {
		t.Fatalf("PrivateURL = %q; want the Hub-reported private URL", priv)
	}
	if got := hub.hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d; want 1 (second call must be served from cache)", got)
	}
}

// 404 maps to ErrNotReported and is negative-cached: an immediate retry does
// not hit the Hub again, but once the (tiny) negative TTL expires the next
// use retries and picks up the now-reported URL.
func TestResolver_NotReportedNegativeCachedThenRetries(t *testing.T) {
	hub := newHubStub(t)
	hub.serveStatus(http.StatusNotFound, `{"code":"SERVICE_URL_NOT_REPORTED"}`)
	r := New(hub.srv.URL, "test-token", WithNegativeTTL(20*time.Millisecond))

	_, err := r.PublicURL(context.Background(), "control-plane")
	if !errors.Is(err, ErrNotReported) {
		t.Fatalf("err = %v; want ErrNotReported on Hub 404", err)
	}
	if got := hub.hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d; want 1", got)
	}

	// Immediate retry — inside the negative TTL, no Hub hit, same error.
	_, err = r.PublicURL(context.Background(), "control-plane")
	if !errors.Is(err, ErrNotReported) {
		t.Fatalf("negative-cached err = %v; want ErrNotReported", err)
	}
	if got := hub.hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d after immediate retry; want still 1 (negative cache)", got)
	}

	// The peer reports; after the negative TTL the resolver retries and succeeds.
	hub.serveURLs("control-plane", "", "https://cp.example.com")
	time.Sleep(30 * time.Millisecond)
	pub, err := r.PublicURL(context.Background(), "control-plane")
	if err != nil {
		t.Fatalf("PublicURL after negative TTL: %v", err)
	}
	if pub != "https://cp.example.com" {
		t.Fatalf("PublicURL = %q; want the newly reported URL", pub)
	}
	if got := hub.hits.Load(); got != 2 {
		t.Fatalf("hub hits = %d; want 2 (one initial, one post-TTL retry)", got)
	}
}

// A Hub failure AFTER a good resolve serves the stale value — availability
// wins over freshness for stable post-boot URLs.
func TestResolver_ServesStaleOnRefreshFailure(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050", "https://gw.example.com")
	r := New(hub.srv.URL, "test-token", WithRefreshTTL(10*time.Millisecond))

	if _, err := r.PublicURL(context.Background(), "ai-gateway"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}

	// Hub goes down; the positive entry ages past the refresh TTL.
	hub.serveStatus(http.StatusInternalServerError, "boom")
	time.Sleep(20 * time.Millisecond)

	pub, err := r.PublicURL(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatalf("PublicURL during Hub outage: %v; want the stale value, not an error", err)
	}
	if pub != "https://gw.example.com" {
		t.Fatalf("PublicURL = %q; want the stale cached URL", pub)
	}
	if got := hub.hits.Load(); got < 2 {
		t.Fatalf("hub hits = %d; want a refresh attempt before serving stale", got)
	}
}

// After the refresh TTL a successful refresh replaces the cached value — a
// moved peer converges without a restart.
func TestResolver_RefreshPicksUpMovedPeer(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050", "https://old.example.com")
	r := New(hub.srv.URL, "test-token", WithRefreshTTL(10*time.Millisecond))

	if _, err := r.PublicURL(context.Background(), "ai-gateway"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	hub.serveURLs("ai-gateway", "http://10.0.0.9:3050", "https://new.example.com")
	time.Sleep(20 * time.Millisecond)

	pub, err := r.PublicURL(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatalf("PublicURL after move: %v", err)
	}
	if pub != "https://new.example.com" {
		t.Fatalf("PublicURL = %q; want the refreshed URL", pub)
	}
}

// Missing bootstrap config is an explicit error — never a silent fallback.
func TestResolver_MissingHubURLOrTokenErrors(t *testing.T) {
	for name, r := range map[string]*Resolver{
		"no hub url": New("", "tok"),
		"no token":   New("http://hub.example.com", ""),
		"neither":    New("", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.PublicURL(context.Background(), "ai-gateway")
			if err == nil || !strings.Contains(err.Error(), "required") {
				t.Fatalf("PublicURL err = %v; want a hub-url/token-required error", err)
			}
			_, err = r.PrivateURL(context.Background(), "ai-gateway")
			if err == nil || !strings.Contains(err.Error(), "required") {
				t.Fatalf("PrivateURL err = %v; want a hub-url/token-required error", err)
			}
		})
	}
}

// Trailing slashes are trimmed everywhere: the Hub base URL when building the
// request path, and the reported URLs before they are handed to callers (so
// base+path concatenation never yields "//").
func TestResolver_TrailingSlashesTrimmed(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050/", "https://gw.example.com/")
	r := New(hub.srv.URL+"/", "test-token")

	pub, err := r.PublicURL(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatalf("PublicURL: %v", err)
	}
	if pub != "https://gw.example.com" {
		t.Fatalf("PublicURL = %q; want trailing slash trimmed", pub)
	}
	priv, err := r.PrivateURL(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatalf("PrivateURL: %v", err)
	}
	if priv != "http://10.0.0.5:3050" {
		t.Fatalf("PrivateURL = %q; want trailing slash trimmed", priv)
	}
}

// A 200 whose payload lacks the requested URL kind is "not reported yet" for
// that kind: PrivateURL errors with ErrNotReported while PublicURL succeeds.
func TestResolver_MissingURLKindIsNotReported(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("control-plane", "", "https://cp.example.com")
	r := New(hub.srv.URL, "test-token")

	if _, err := r.PrivateURL(context.Background(), "control-plane"); !errors.Is(err, ErrNotReported) {
		t.Fatalf("PrivateURL err = %v; want ErrNotReported when privateUrl is empty", err)
	}
	if pub, err := r.PublicURL(context.Background(), "control-plane"); err != nil || pub != "https://cp.example.com" {
		t.Fatalf("PublicURL = %q, %v; want the reported public URL", pub, err)
	}

	// And the mirror case: only a private URL reported.
	hub2 := newHubStub(t)
	hub2.serveURLs("ai-gateway", "http://10.0.0.5:3050", "")
	r2 := New(hub2.srv.URL, "test-token")
	if _, err := r2.PublicURL(context.Background(), "ai-gateway"); !errors.Is(err, ErrNotReported) {
		t.Fatalf("PublicURL err = %v; want ErrNotReported when publicUrl is empty", err)
	}
	if priv, err := r2.PrivateURL(context.Background(), "ai-gateway"); err != nil || priv != "http://10.0.0.5:3050" {
		t.Fatalf("PrivateURL = %q, %v; want the reported private URL", priv, err)
	}
}

// Non-404 Hub errors surface the status and body for diagnosis.
func TestResolver_HubErrorStatusSurfaced(t *testing.T) {
	hub := newHubStub(t)
	hub.serveStatus(http.StatusInternalServerError, "database down")
	r := New(hub.srv.URL, "test-token")

	_, err := r.PublicURL(context.Background(), "ai-gateway")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("err = %v; want hub status 500 + body in the error", err)
	}
	if errors.Is(err, ErrNotReported) {
		t.Fatalf("a 500 must not be classified as ErrNotReported (that would mask a Hub outage)")
	}
}

// A malformed 200 payload is a decode error, not a silent zero value.
func TestResolver_MalformedPayloadErrors(t *testing.T) {
	hub := newHubStub(t)
	hub.serveStatus(http.StatusOK, `{"privateUrl": nope}`)
	r := New(hub.srv.URL, "test-token")

	_, err := r.PublicURL(context.Background(), "ai-gateway")
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v; want a decode error on malformed payload", err)
	}
}

// An unreachable Hub is a fetch error (and negative-cached like any failure).
func TestResolver_UnreachableHubErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // now guaranteed-refused address
	r := New(srv.URL, "test-token")

	_, err := r.PublicURL(context.Background(), "ai-gateway")
	if err == nil || !strings.Contains(err.Error(), "hub fetch") {
		t.Fatalf("err = %v; want a hub-fetch (dial) error", err)
	}
}

// A hub URL that cannot form a valid request URL errors at request build.
func TestResolver_InvalidHubURLErrors(t *testing.T) {
	r := New("http://[::1", "test-token") // unclosed IPv6 literal
	_, err := r.PublicURL(context.Background(), "ai-gateway")
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("err = %v; want a build-request error for a malformed hub URL", err)
	}
}

// WithHTTPClient swaps the transport — proven by a client whose transport
// fails distinctively.
func TestResolver_WithHTTPClient(t *testing.T) {
	sentinel := errors.New("sentinel transport")
	c := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, sentinel
	})}
	r := New("http://hub.example.com", "test-token", WithHTTPClient(c))
	_, err := r.PublicURL(context.Background(), "ai-gateway")
	if err == nil || !strings.Contains(err.Error(), "sentinel transport") {
		t.Fatalf("err = %v; want the injected client's transport error", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// URLs returns both reported kinds from ONE cache/Hub resolution — the
// webhook trusted-bases provider depends on this being a single round-trip.
func TestResolver_URLsSingleResolution(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050", "https://gw.example.com")
	r := New(hub.srv.URL, "test-token")

	pub, priv, err := r.URLs(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatalf("URLs: %v", err)
	}
	if pub != "https://gw.example.com" || priv != "http://10.0.0.5:3050" {
		t.Fatalf("URLs = (%q, %q); want both Hub-reported values", pub, priv)
	}
	if _, _, err := r.URLs(context.Background(), "ai-gateway"); err != nil {
		t.Fatalf("URLs (cached): %v", err)
	}
	if got := hub.hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d; want 1 (one resolution serves both kinds, cached)", got)
	}

	// Error path: a not-reported peer surfaces the error from URLs too.
	hub.serveStatus(http.StatusNotFound, `{"code":"SERVICE_URL_NOT_REPORTED"}`)
	if _, _, err := r.URLs(context.Background(), "compliance-proxy"); !errors.Is(err, ErrNotReported) {
		t.Fatalf("URLs(not reported) err = %v; want ErrNotReported", err)
	}
}

// A failed refresh serves the stale value AND throttles further refetches for
// the negative TTL — an unreachable Hub must not add a fetch attempt (and its
// timeout) to every caller in the stale window.
func TestResolver_StaleServeThrottlesRefetch(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050", "https://gw.example.com")
	r := New(hub.srv.URL, "test-token",
		WithRefreshTTL(10*time.Millisecond), WithNegativeTTL(150*time.Millisecond))

	if _, err := r.PrivateURL(context.Background(), "ai-gateway"); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}
	if got := hub.hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d; want 1 after initial resolve", got)
	}

	// Entry goes stale, and the Hub starts failing.
	time.Sleep(20 * time.Millisecond)
	hub.serveStatus(http.StatusInternalServerError, "boom")

	// First stale call attempts ONE refresh (hit 2) and serves the stale value.
	priv, err := r.PrivateURL(context.Background(), "ai-gateway")
	if err != nil || priv != "http://10.0.0.5:3050" {
		t.Fatalf("stale serve = (%q, %v); want the stale value, no error", priv, err)
	}
	if got := hub.hits.Load(); got != 2 {
		t.Fatalf("hub hits = %d; want 2 (exactly one refresh attempt)", got)
	}

	// Calls inside the throttle window keep serving stale WITHOUT refetching.
	for i := range 5 {
		if v, err := r.PrivateURL(context.Background(), "ai-gateway"); err != nil || v != "http://10.0.0.5:3050" {
			t.Fatalf("throttled stale serve #%d = (%q, %v)", i, v, err)
		}
	}
	if got := hub.hits.Load(); got != 2 {
		t.Fatalf("hub hits = %d; want 2 (refetch throttled while stale-serving)", got)
	}

	// After the throttle window the next call retries — and picks up recovery.
	time.Sleep(160 * time.Millisecond)
	hub.serveURLs("ai-gateway", "http://10.0.0.9:3050", "https://gw.example.com")
	priv, err = r.PrivateURL(context.Background(), "ai-gateway")
	if err != nil || priv != "http://10.0.0.9:3050" {
		t.Fatalf("post-throttle refresh = (%q, %v); want the recovered value", priv, err)
	}
	if got := hub.hits.Load(); got != 3 {
		t.Fatalf("hub hits = %d; want 3 (one retry after the throttle window)", got)
	}
}

// A peer that reported only ONE URL kind: the missing kind errors AND flips
// the entry to the negative cadence, so the other kind keeps serving from
// cache while the missing one is re-checked after negativeTTL — not after the
// full refresh TTL (mixed-version rolling deploys report publicUrl first).
func TestResolver_MissingKindRechecksOnNegativeCadence(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "", "https://gw.example.com") // no privateUrl yet
	r := New(hub.srv.URL, "test-token",
		WithRefreshTTL(time.Hour), WithNegativeTTL(80*time.Millisecond))

	if _, err := r.PrivateURL(context.Background(), "ai-gateway"); !errors.Is(err, ErrNotReported) {
		t.Fatalf("PrivateURL err = %v; want ErrNotReported for the missing kind", err)
	}
	// The good kind still serves — from cache, no extra Hub hit.
	if pub, err := r.PublicURL(context.Background(), "ai-gateway"); err != nil || pub != "https://gw.example.com" {
		t.Fatalf("PublicURL = (%q, %v); want the reported public URL from cache", pub, err)
	}
	if got := hub.hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d; want 1 (good kind served from cache)", got)
	}

	// After the NEGATIVE TTL (not the 1h refresh TTL) the next use refetches
	// and picks up the now-reported private URL.
	time.Sleep(100 * time.Millisecond)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050", "https://gw.example.com")
	priv, err := r.PrivateURL(context.Background(), "ai-gateway")
	if err != nil || priv != "http://10.0.0.5:3050" {
		t.Fatalf("PrivateURL after report = (%q, %v); want the newly reported value", priv, err)
	}
	if got := hub.hits.Load(); got != 2 {
		t.Fatalf("hub hits = %d; want 2 (one recheck on the negative cadence)", got)
	}
}

// A caller whose OWN context is canceled must not poison the shared cache:
// the next caller with a live context resolves normally, immediately.
func TestResolver_CanceledCallerDoesNotPoisonCache(t *testing.T) {
	hub := newHubStub(t)
	hub.serveURLs("ai-gateway", "http://10.0.0.5:3050", "https://gw.example.com")
	r := New(hub.srv.URL, "test-token")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.PrivateURL(canceled, "ai-gateway"); err == nil {
		t.Fatal("PrivateURL with canceled ctx: want error")
	}
	// A healthy caller right after must succeed — no negative entry installed.
	priv, err := r.PrivateURL(context.Background(), "ai-gateway")
	if err != nil || priv != "http://10.0.0.5:3050" {
		t.Fatalf("PrivateURL after canceled caller = (%q, %v); want a clean resolve", priv, err)
	}
}

// expireSoon edge branches: a negative entry and an already-throttled entry
// are left untouched; repeated missing-kind getters do not stack rewrites.
func TestResolver_ExpireSoonEdgeBranches(t *testing.T) {
	hub := newHubStub(t)
	r := New(hub.srv.URL, "test-token",
		WithRefreshTTL(time.Hour), WithNegativeTTL(time.Hour))

	// Negative entry: getter on a 404 type — expireSoon must not touch it.
	hub.serveStatus(http.StatusNotFound, `{}`)
	if _, err := r.PrivateURL(context.Background(), "control-plane"); !errors.Is(err, ErrNotReported) {
		t.Fatalf("want ErrNotReported, got %v", err)
	}
	if _, err := r.PrivateURL(context.Background(), "control-plane"); !errors.Is(err, ErrNotReported) {
		t.Fatalf("negative entry must keep serving ErrNotReported, got %v", err)
	}
	if got := hub.hits.Load(); got != 1 {
		t.Fatalf("hub hits = %d; want 1 (negative entry cached)", got)
	}

	// Missing-kind positive entry: the second getter sees the entry already
	// in the stale+throttle state and leaves it as-is (no refetch storm).
	hub.serveURLs("ai-gateway", "", "https://gw.example.com")
	_, _ = r.PrivateURL(context.Background(), "ai-gateway")
	_, _ = r.PrivateURL(context.Background(), "ai-gateway")
	_, _ = r.PrivateURL(context.Background(), "ai-gateway")
	if got := hub.hits.Load(); got != 2 {
		t.Fatalf("hub hits = %d; want 2 (throttled — no refetch per missing-kind getter)", got)
	}
}

// expireSoon on a type with no cache entry is a no-op (nil-entry branch).
func TestResolver_ExpireSoonUnknownTypeNoop(t *testing.T) {
	r := New("http://hub.invalid", "test-token")
	r.expireSoon("ai-gateway") // must not panic or create an entry
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cache) != 0 {
		t.Fatalf("cache entries = %d; want 0 after expireSoon on unknown type", len(r.cache))
	}
}

// A concurrent resolver's fresh SUCCESS must never be clobbered by this
// caller's failure: the failure path re-reads the map and returns the
// winner's positive entry instead of installing a negative one.
func TestResolver_FailureDoesNotClobberConcurrentSuccess(t *testing.T) {
	hub := newHubStub(t)
	r := New("", "", WithHTTPClient(http.DefaultClient)) // hubURL set below
	// Plant the "concurrent winner" from inside the Hub handler — it runs
	// while THIS caller's fetch is in flight, exactly the race window.
	hub.set(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.cache["ai-gateway"] = &entry{privateURL: "http://10.0.0.9:3050", fetchedAt: time.Now()}
		r.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	r.hubURL = hub.srv.URL
	r.token = "test-token"

	priv, err := r.PrivateURL(context.Background(), "ai-gateway")
	if err != nil || priv != "http://10.0.0.9:3050" {
		t.Fatalf("PrivateURL = (%q, %v); want the concurrent winner's value, no error", priv, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.cache["ai-gateway"]; e == nil || e.err != nil {
		t.Fatalf("cache entry = %+v; the winner's positive entry must survive", e)
	}
}
