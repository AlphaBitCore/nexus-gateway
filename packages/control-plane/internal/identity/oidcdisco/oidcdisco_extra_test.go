package oidcdisco

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// TestWithInsecureSkipHostCheck verifies that the option sets checkHost to nil
// and replaces the HTTP client with a plain one (no private-dial block), making
// the Resolver reachable to a loopback httptest server.
func TestWithInsecureSkipHostCheck(t *testing.T) {
	srv := discoveryServer(t, nil)

	// The production NewResolver blocks loopback; WithInsecureSkipHostCheck
	// must remove both the pre-check (checkHost=nil) and the dial-time guard
	// so the resolver can reach a 127.0.0.1 test server.
	r := NewResolver(WithInsecureSkipHostCheck())

	// Verify the field is nil — the option must clear checkHost.
	if r.checkHost != nil {
		t.Error("checkHost must be nil after WithInsecureSkipHostCheck")
	}

	// Verify the resolver can actually reach a loopback server (functional
	// proof that both guards are gone — a misfire would return an SSRF error
	// or a connection-refused from the dial hook).
	got, err := r.Resolve(ctx(), srv.URL, Endpoints{})
	if err != nil {
		t.Fatalf("Resolve with loopback server must succeed after WithInsecureSkipHostCheck: %v", err)
	}
	if got.AuthorizeURL != srv.URL+"/auth" {
		t.Errorf("AuthorizeURL = %q; want %q", got.AuthorizeURL, srv.URL+"/auth")
	}
}

// TestBlockPrivateDialControl_PrivateAddresses proves the SSRF dial-time hook
// rejects every non-public IP range: loopback, RFC-1918 private, link-local
// (incl. cloud metadata 169.254.169.254), and unspecified.
func TestBlockPrivateDialControl_PrivateAddresses(t *testing.T) {
	cases := []struct {
		name    string
		address string // host:port passed to blockPrivateDialControl
		wantErr bool
	}{
		{"loopback 127.0.0.1", "127.0.0.1:443", true},
		{"loopback 127.5.5.5", "127.5.5.5:443", true},
		{"RFC-1918 10.x", "10.0.0.1:443", true},
		{"RFC-1918 192.168.x", "192.168.1.1:443", true},
		{"link-local cloud-meta", "169.254.169.254:80", true},
		{"public 1.1.1.1", "1.1.1.1:443", false},
		{"public 8.8.8.8", "8.8.8.8:53", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nexushttp.BlockPrivateDialControl("tcp", tc.address, nil)
			if tc.wantErr && err == nil {
				t.Errorf("nexushttp.BlockPrivateDialControl(%q) = nil; want error (SSRF guard)", tc.address)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("nexushttp.BlockPrivateDialControl(%q) = %v; want nil (public address)", tc.address, err)
			}
		})
	}
}

// TestBlockPrivateDialControl_BadAddress proves that a non-parseable address
// (no port) returns a parse error — the control hook must never silently pass
// an address it cannot interpret (SplitHostPort failure branch).
func TestBlockPrivateDialControl_BadAddress(t *testing.T) {
	cases := []string{
		"badaddress",  // no port → SplitHostPort error
		"bad:bad:bad", // too many colons → SplitHostPort error
	}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			err := nexushttp.BlockPrivateDialControl("tcp", addr, nil)
			if err == nil {
				t.Errorf("nexushttp.BlockPrivateDialControl(%q) = nil; want parse error", addr)
			}
		})
	}
}

// TestBlockPrivateDialControl_NonIPHost proves that when host:port parses but
// the host is not a numeric IP, the hook returns a "not an IP" error. This
// should never happen in production (Control hooks receive resolved addresses)
// but is a defence-in-depth guard.
func TestBlockPrivateDialControl_NonIPHost(t *testing.T) {
	// net.SplitHostPort succeeds for "notanip:443" but net.ParseIP("notanip")
	// returns nil, reaching the "is not an IP" error arm.
	err := nexushttp.BlockPrivateDialControl("tcp", "notanip:443", (syscall.RawConn)(nil))
	if err == nil {
		t.Error("blockPrivateDialControl with non-IP host must error")
	}
}

// TestValidatePublicHost_IP_Coverage exercises all validatePublicHost branches
// for literal IP inputs (the `if ip := net.ParseIP(host)` early-exit path):
//   - public IP → nil
//   - private/loopback/link-local IP → SSRF error
func TestValidatePublicHost_IP_Coverage(t *testing.T) {
	cases := []struct {
		host    string
		wantErr bool
	}{
		{"1.1.1.1", false},        // public → nil
		{"8.8.8.8", false},        // public → nil
		{"127.0.0.1", true},       // loopback → error
		{"192.168.100.1", true},   // RFC-1918 → error
		{"169.254.169.254", true}, // link-local / cloud-meta → error
		{"10.10.10.10", true},     // RFC-1918 → error
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			err := validatePublicHost(ctx(), tc.host)
			if tc.wantErr && err == nil {
				t.Errorf("validatePublicHost(%q) = nil; want SSRF error", tc.host)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validatePublicHost(%q) = %v; want nil", tc.host, err)
			}
		})
	}
}

// TestValidatePublicHost_DNSResolvesPublic exercises the for-range addrs loop
// in validatePublicHost when DNS succeeds and all resolved addresses are public.
// We use one.one.one.one (Cloudflare's hostname, resolves to 1.1.1.1) as an
// in-range public hostname; if DNS is unavailable the test is skipped.
func TestValidatePublicHost_DNSResolvesPublic(t *testing.T) {
	host := "one.one.one.one"
	addrs, lookupErr := net.DefaultResolver.LookupHost(context.Background(), host)
	if lookupErr != nil || len(addrs) == 0 {
		t.Skipf("DNS unavailable (%v) — skipping public-DNS branch test", lookupErr)
	}
	// Self-check: only proceed if the lookup returned public addresses.
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip != nil && nexushttp.IsDisallowedIP(ip) {
			t.Skipf("DNS returned non-public IP %s for %s — skip (unexpected resolver)", a, host)
		}
	}

	// All resolved addresses are public → validatePublicHost must return nil
	// (the for-range loop body runs and finds no disallowed IP).
	if err := validatePublicHost(ctx(), host); err != nil {
		t.Errorf("validatePublicHost(%q) = %v; want nil (all resolved IPs public)", host, err)
	}
}

// TestNewResolver_OptionsApplied verifies NewResolver applies all provided
// Option funcs (the loop `for _, opt := range opts { opt(r) }` body). We use
// a custom option that mutates a Resolver field to confirm invocation.
func TestNewResolver_OptionsApplied(t *testing.T) {
	var called bool
	sentinel := func(r *Resolver) {
		called = true
		r.ttl = 0 // mutate to prove the option ran
	}
	r := NewResolver(sentinel)
	if !called {
		t.Error("option func was not called by NewResolver")
	}
	if r.ttl != 0 {
		t.Errorf("option mutation not applied; ttl = %v", r.ttl)
	}
}

// TestFetch_CancelledContext covers the client.Do error arm inside fetch: a
// cancelled context causes http.Client.Do to fail immediately, reaching the
// "discovery fetch failed: %w" fmt.Errorf branch.
func TestFetch_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Not called — context is already cancelled before the request fires.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Cancelled context causes client.Do to return immediately with an error.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	// WithInsecureSkipHostCheck so the SSRF host check doesn't block first.
	r := testResolver(srv.Client(), time.Now)
	_, err := r.Resolve(cancelCtx, srv.URL, Endpoints{})
	if err == nil {
		t.Error("cancelled context must produce a fetch error")
	}
}
