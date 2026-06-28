package oidcdisco

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

func ctx() context.Context { return context.Background() }

// testResolver builds a Resolver wired to a test server's client with an
// injectable clock so cache-TTL behaviour is deterministic.
func testResolver(client *http.Client, now func() time.Time) *Resolver {
	return &Resolver{
		client: client,
		ttl:    DefaultTTL,
		now:    now,
		cache:  make(map[string]cacheEntry),
	}
}

func discoveryServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		_, _ = fmt.Fprintf(w, `{"authorization_endpoint":"%s/auth","token_endpoint":"%s/tok","jwks_uri":"%s/jwks"}`,
			srv.URL, srv.URL, srv.URL)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEndpoints_Complete(t *testing.T) {
	if (Endpoints{AuthorizeURL: "a", TokenURL: "t", JwksURI: "j"}).Complete() != true {
		t.Fatal("all three present must be Complete")
	}
	if (Endpoints{AuthorizeURL: "a", TokenURL: "t"}).Complete() {
		t.Fatal("missing jwks must not be Complete")
	}
}

func TestResolve_SkipsDiscoveryWhenComplete(t *testing.T) {
	// A client whose transport always errors: if Resolve touches the network
	// the test fails. Complete input must short-circuit before any fetch.
	r := testResolver(&http.Client{Transport: errTransport{}}, time.Now)
	have := Endpoints{AuthorizeURL: "https://a", TokenURL: "https://t", JwksURI: "https://j"}
	got, err := r.Resolve(ctx(), "https://issuer", have)
	if err != nil {
		t.Fatalf("complete input must not error: %v", err)
	}
	if got != have {
		t.Fatalf("complete input must be returned verbatim: %+v", got)
	}
}

func TestResolve_EmptyIssuerIncomplete(t *testing.T) {
	r := NewResolver()
	if _, err := r.Resolve(ctx(), "  ", Endpoints{AuthorizeURL: "https://a"}); err == nil {
		t.Fatal("empty issuer with missing endpoints must error")
	}
}

func TestResolve_FillsAllFromDiscovery(t *testing.T) {
	srv := discoveryServer(t, nil)
	r := testResolver(srv.Client(), time.Now)
	got, err := r.Resolve(ctx(), srv.URL, Endpoints{})
	if err != nil {
		t.Fatalf("discovery resolve: %v", err)
	}
	if got.AuthorizeURL != srv.URL+"/auth" || got.TokenURL != srv.URL+"/tok" || got.JwksURI != srv.URL+"/jwks" {
		t.Fatalf("endpoints not filled from discovery: %+v", got)
	}
}

func TestResolve_ExplicitValuesWin(t *testing.T) {
	srv := discoveryServer(t, nil)
	r := testResolver(srv.Client(), time.Now)
	// authorizeUrl pinned; token + jwks come from discovery.
	got, err := r.Resolve(ctx(), srv.URL, Endpoints{AuthorizeURL: "https://pinned/authorize"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.AuthorizeURL != "https://pinned/authorize" {
		t.Fatalf("pinned authorizeUrl must win, got %q", got.AuthorizeURL)
	}
	if got.TokenURL != srv.URL+"/tok" || got.JwksURI != srv.URL+"/jwks" {
		t.Fatalf("gaps must be filled from discovery: %+v", got)
	}
}

func TestResolve_CacheHitAvoidsRefetch(t *testing.T) {
	var hits int32
	srv := discoveryServer(t, &hits)
	r := testResolver(srv.Client(), time.Now)
	for i := range 3 {
		if _, err := r.Resolve(ctx(), srv.URL, Endpoints{}); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 discovery fetch with caching, got %d", got)
	}
}

func TestResolve_CacheExpiryRefetches(t *testing.T) {
	var hits int32
	srv := discoveryServer(t, &hits)
	base := time.Unix(1_700_000_000, 0)
	clock := base
	r := testResolver(srv.Client(), func() time.Time { return clock })

	if _, err := r.Resolve(ctx(), srv.URL, Endpoints{}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Within TTL → cache hit.
	clock = base.Add(DefaultTTL - time.Second)
	if _, err := r.Resolve(ctx(), srv.URL, Endpoints{}); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	// Past TTL → refetch.
	clock = base.Add(DefaultTTL + time.Second)
	if _, err := r.Resolve(ctx(), srv.URL, Endpoints{}); err != nil {
		t.Fatalf("third resolve: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected 2 fetches across TTL expiry, got %d", got)
	}
}

func TestResolve_PartialDiscoveryDocLeavesGap(t *testing.T) {
	// Discovery doc missing token_endpoint: tokenUrl stays empty, no error —
	// the caller's own guard decides what to do with the unresolved endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"authorization_endpoint":"https://a","jwks_uri":"https://j"}`))
	}))
	defer srv.Close()
	got, err := testResolver(srv.Client(), time.Now).Resolve(ctx(), srv.URL, Endpoints{})
	if err != nil {
		t.Fatalf("partial doc must not error: %v", err)
	}
	if got.TokenURL != "" || got.AuthorizeURL != "https://a" {
		t.Fatalf("expected token gap left empty, authorize filled: %+v", got)
	}
}

func TestFetch_InvalidIssuerURL(t *testing.T) {
	if _, err := NewResolver().Resolve(ctx(), "not_a_url", Endpoints{}); err == nil {
		t.Fatal("non-URL issuer must error")
	}
}

func TestFetch_FetchFailure(t *testing.T) {
	// Dead port → transport error.
	if _, err := NewResolver().Resolve(ctx(), "http://127.0.0.1:1", Endpoints{}); err == nil {
		t.Fatal("unreachable issuer must error")
	}
}

func TestFetch_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := testResolver(srv.Client(), time.Now).Resolve(ctx(), srv.URL, Endpoints{}); err == nil {
		t.Fatal("discovery HTTP 500 must error")
	}
}

func TestFetch_ParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()
	if _, err := testResolver(srv.Client(), time.Now).Resolve(ctx(), srv.URL, Endpoints{}); err == nil {
		t.Fatal("malformed discovery body must error")
	}
}

func TestFetch_ReadError(t *testing.T) {
	// Advertise a long body then close early → io.ReadAll returns an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, bufrw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\nshort")
		_ = bufrw.Flush()
		_ = conn.Close()
	}))
	defer srv.Close()
	if _, err := testResolver(srv.Client(), time.Now).Resolve(ctx(), srv.URL, Endpoints{}); err == nil {
		t.Fatal("short discovery body must error")
	}
}

func TestNewResolver_Defaults(t *testing.T) {
	r := NewResolver()
	if r.ttl != DefaultTTL || r.client == nil || r.now == nil || r.cache == nil {
		t.Fatalf("NewResolver defaults not set: %+v", r)
	}
}

// errTransport fails every round trip — used to prove Complete inputs never
// reach the network.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("network must not be touched")
}

// TestIsDisallowedIP_RangeMatrix is the SSRF-guard unit matrix: every non-public
// range the SSRF guard must block returns true; representative public IPs
// return false.
func TestIsDisallowedIP_RangeMatrix(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},             // loopback
		{"127.5.5.5", true},             // loopback /8
		{"::1", true},                   // IPv6 loopback
		{"10.0.0.1", true},              // RFC-1918
		{"10.255.255.255", true},        // RFC-1918
		{"172.16.0.1", true},            // RFC-1918
		{"172.31.255.1", true},          // RFC-1918
		{"192.168.1.1", true},           // RFC-1918
		{"169.254.1.1", true},           // link-local
		{"169.254.169.254", true},       // cloud metadata
		{"fe80::1", true},               // IPv6 link-local
		{"fc00::1", true},               // IPv6 ULA (private)
		{"0.0.0.0", true},               // unspecified
		{"::", true},                    // IPv6 unspecified
		{"224.0.0.1", true},             // multicast
		{"8.8.8.8", false},              // public
		{"1.1.1.1", false},              // public
		{"172.32.0.1", false},           // just outside 172.16/12
		{"203.0.113.10", false},         // public (TEST-NET-3, but not in a blocked range)
		{"2606:4700:4700::1111", false}, // public IPv6 (Cloudflare)
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("test bug: %q is not a valid IP", c.ip)
		}
		if got := nexushttp.IsDisallowedIP(ip); got != c.want {
			t.Errorf("IsDisallowedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// TestValidatePublicHost_LiteralPrivateRejected proves a numeric private/loopback
// issuer host is rejected by the pre-check without any DNS lookup.
func TestValidatePublicHost_LiteralPrivateRejected(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "192.168.0.5", "169.254.169.254", "::1", "[fe80::1]"} {
		h := strings.Trim(host, "[]")
		if err := validatePublicHost(ctx(), h); err == nil {
			t.Errorf("validatePublicHost(%q) = nil; want SSRF rejection", h)
		}
	}
}

// TestValidatePublicHost_PublicLiteralAllowed proves a public numeric host passes.
func TestValidatePublicHost_PublicLiteralAllowed(t *testing.T) {
	if err := validatePublicHost(ctx(), "8.8.8.8"); err != nil {
		t.Errorf("validatePublicHost(8.8.8.8) = %v; want nil (public)", err)
	}
}

// TestValidatePublicHost_DNSFailureNotBlocked proves the SSRF-guard contract that a
// host with zero resolvable addresses is NOT treated as an SSRF block (it returns
// nil so the caller surfaces the real network error). We use a guaranteed-
// nonexistent TLD so the lookup fails.
func TestValidatePublicHost_DNSFailureNotBlocked(t *testing.T) {
	if err := validatePublicHost(ctx(), "nonexistent-host.invalid"); err != nil {
		t.Errorf("DNS failure must not be an SSRF block, got %v", err)
	}
}

// TestResolve_PrivateIssuerRejected is the end-to-end SSRF-guard assertion through
// the production NewResolver(): an issuer pointing at a loopback/private address
// is rejected before any document is fetched (so the error is the SSRF guard's,
// not a connection-refused).
func TestResolve_PrivateIssuerRejected(t *testing.T) {
	r := NewResolver()
	for _, issuer := range []string{
		"http://127.0.0.1/idp",
		"http://10.1.2.3",
		"https://192.168.50.1:8443",
		"http://169.254.169.254",
	} {
		_, err := r.Resolve(ctx(), issuer, Endpoints{})
		if err == nil {
			t.Errorf("Resolve(%q) = nil err; want SSRF rejection", issuer)
			continue
		}
		if !strings.Contains(err.Error(), "SSRF") && !strings.Contains(err.Error(), "non-public") {
			t.Errorf("Resolve(%q) error = %q; want an SSRF-guard message", issuer, err.Error())
		}
	}
}

// TestFetch_NonHTTPSchemeRejected proves a non-http(s) issuer scheme cannot
// reach the fetcher (file:// SSRF / LFI hardening).
func TestFetch_NonHTTPSchemeRejected(t *testing.T) {
	if _, err := NewResolver().Resolve(ctx(), "file:///etc/passwd", Endpoints{}); err == nil {
		t.Fatal("file:// issuer must be rejected")
	}
}
