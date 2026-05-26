package access

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestPrivateIPChecker_AllRanges(t *testing.T) {
	checker, err := NewPrivateIPChecker(nil)
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	privateIPs := []struct {
		name string
		ip   string
	}{
		{"RFC1918 10.x", "10.0.0.1"},
		{"RFC1918 172.16.x", "172.16.0.1"},
		{"RFC1918 192.168.x", "192.168.0.1"},
		{"loopback", "127.0.0.1"},
		{"link-local", "169.254.1.1"},
		{"CGN RFC6598", "100.64.0.1"},
		{"IPv6 loopback", "::1"},
		{"IPv6 unique local", "fd00::1"},
		{"IPv6 link-local", "fe80::1"},
	}

	for _, tt := range privateIPs {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("invalid test IP: %s", tt.ip)
			}
			err := checker.CheckResolved([]net.IP{ip})
			if err == nil {
				t.Errorf("expected error for private IP %s", tt.ip)
			}
		})
	}
}

func TestPrivateIPChecker_PublicIP(t *testing.T) {
	checker, err := NewPrivateIPChecker(nil)
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	publicIPs := []string{"8.8.8.8", "1.1.1.1", "104.18.0.1", "2606:4700::1"}
	for _, ipStr := range publicIPs {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			t.Fatalf("invalid test IP: %s", ipStr)
		}
		if err := checker.CheckResolved([]net.IP{ip}); err != nil {
			t.Errorf("public IP %s should pass: %v", ipStr, err)
		}
	}
}

func TestPrivateIPChecker_Exception(t *testing.T) {
	// Allow 10.100.0.0/16 as an exception (e.g. internal AI service).
	checker, err := NewPrivateIPChecker([]string{"10.100.0.0/16"})
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	// Excepted IP should pass.
	ip := net.ParseIP("10.100.1.5")
	if err := checker.CheckResolved([]net.IP{ip}); err != nil {
		t.Errorf("excepted private IP should pass: %v", err)
	}

	// Non-excepted private IP should still fail.
	ip2 := net.ParseIP("10.0.0.1")
	if err := checker.CheckResolved([]net.IP{ip2}); err == nil {
		t.Error("non-excepted private IP should fail")
	}
}

func TestPrivateIPChecker_IPv6(t *testing.T) {
	checker, err := NewPrivateIPChecker(nil)
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	tests := []struct {
		name    string
		ip      string
		wantErr bool
	}{
		{"IPv6 loopback", "::1", true},
		{"IPv6 unique local", "fc00::1", true},
		{"IPv6 unique local fd", "fd12:3456:789a::1", true},
		{"IPv6 link-local", "fe80::1", true},
		{"IPv6 public", "2001:4860:4860::8888", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("invalid test IP: %s", tt.ip)
			}
			err := checker.CheckResolved([]net.IP{ip})
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckResolved(%s) error = %v, wantErr %v", tt.ip, err, tt.wantErr)
			}
		})
	}
}

// mockResolver is a test double that returns preconfigured addresses.
type mockResolver struct {
	addrs []net.IPAddr
	err   error
}

func (m *mockResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return m.addrs, m.err
}

func TestPrivateIPChecker_ResolveAndCheck(t *testing.T) {
	checker, err := NewPrivateIPChecker(nil)
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	// Inject a mock resolver that returns a private IP.
	checker.resolver = &mockResolver{
		addrs: []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}},
	}

	err = checker.ResolveAndCheck(context.Background(), "evil.example.com")
	if err == nil {
		t.Error("expected error when resolved IP is private")
	}
	if !errors.Is(err, ErrPrivateIP) {
		t.Errorf("expected ErrPrivateIP wrap, got: %v", err)
	}

	// Inject a mock resolver that returns a public IP.
	checker.resolver = &mockResolver{
		addrs: []net.IPAddr{{IP: net.ParseIP("104.18.0.1")}},
	}

	err = checker.ResolveAndCheck(context.Background(), "api.openai.com")
	if err != nil {
		t.Errorf("expected no error for public IP: %v", err)
	}
}

func TestPrivateIPChecker_ResolveAndCheck_ResolverError(t *testing.T) {
	checker, err := NewPrivateIPChecker(nil)
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	sentinel := errors.New("NXDOMAIN")
	checker.resolver = &mockResolver{err: sentinel}

	err = checker.ResolveAndCheck(context.Background(), "does-not-resolve.example.invalid")
	if err == nil {
		t.Fatal("expected error when resolver fails")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped resolver error, got: %v", err)
	}
}

func TestPrivateIPChecker_ResolveAndCheck_EmptyResolution(t *testing.T) {
	checker, err := NewPrivateIPChecker(nil)
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	// Resolver succeeds but returns no addresses — must reject.
	checker.resolver = &mockResolver{addrs: nil}

	err = checker.ResolveAndCheck(context.Background(), "empty.example.com")
	if err == nil {
		t.Fatal("expected error when resolver returns zero addresses")
	}
}

func TestNewPrivateIPChecker_InvalidExceptionCIDR(t *testing.T) {
	_, err := NewPrivateIPChecker([]string{"not-a-cidr"})
	if err == nil {
		t.Fatal("expected error for invalid exception CIDR")
	}
}

// TestWithResolver_InjectsStubAndNilIsNoOp exercises both branches of
// WithResolver: a non-nil resolver replaces net.DefaultResolver; a nil
// resolver is ignored so the default stays in place.
func TestWithResolver_InjectsStubAndNilIsNoOp(t *testing.T) {
	stub := &mockResolver{addrs: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}}

	// Non-nil resolver: injected stub must be used.
	checker, err := NewPrivateIPChecker(nil, WithResolver(stub))
	if err != nil {
		t.Fatalf("NewPrivateIPChecker with stub resolver: %v", err)
	}
	if checker.resolver != stub {
		t.Fatal("WithResolver: expected stub to be set, got a different resolver")
	}
	// The stub returns a public IP so ResolveAndCheck must succeed.
	if err := checker.ResolveAndCheck(context.Background(), "example.com"); err != nil {
		t.Fatalf("ResolveAndCheck with public-IP stub: %v", err)
	}

	// Nil resolver: must not overwrite the default.
	checkerDefault, err := NewPrivateIPChecker(nil, WithResolver(nil))
	if err != nil {
		t.Fatalf("NewPrivateIPChecker with nil resolver: %v", err)
	}
	if checkerDefault.resolver != net.DefaultResolver {
		t.Fatal("WithResolver(nil): expected net.DefaultResolver to remain, got a different resolver")
	}
}

func TestPrivateIPChecker_CIDRBoundary(t *testing.T) {
	// Verify the first and last IPs of an RFC 1918 /8 are still flagged
	// private — boundary-of-range coverage for the contains check.
	checker, err := NewPrivateIPChecker(nil)
	if err != nil {
		t.Fatalf("NewPrivateIPChecker: %v", err)
	}

	for _, ip := range []string{"10.0.0.0", "10.255.255.255"} {
		if err := checker.CheckResolved([]net.IP{net.ParseIP(ip)}); err == nil {
			t.Errorf("expected %s to be flagged private (boundary)", ip)
		}
	}
	// Adjacent public IP must pass.
	if err := checker.CheckResolved([]net.IP{net.ParseIP("11.0.0.1")}); err != nil {
		t.Errorf("expected 11.0.0.1 to pass (adjacent to RFC1918 /8): %v", err)
	}
}
