package access

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// silentLogger is a slog.Logger that discards everything — used so swap tests
// don't pollute go test output but still exercise the logger.Warn / logger.Info
// code paths that the production callbacks rely on.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newCheckerForTest constructs a Checker with a deterministic mock resolver so
// CheckConnect can exercise the private-IP branch without touching real DNS.
func newCheckerForTest(t *testing.T, ipCIDRs, domains, exceptions []string, resolvedIPs []net.IP, resolverErr error) *Checker {
	t.Helper()
	c, err := NewChecker(ipCIDRs, domains, exceptions)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	addrs := make([]net.IPAddr, 0, len(resolvedIPs))
	for _, ip := range resolvedIPs {
		addrs = append(addrs, net.IPAddr{IP: ip})
	}
	c.privateIPCheck.resolver = &mockResolver{addrs: addrs, err: resolverErr}
	return c
}

func TestNewChecker_PropagatesIPAllowlistError(t *testing.T) {
	_, err := NewChecker([]string{"not-a-cidr"}, nil, nil)
	if err == nil {
		t.Fatal("expected error for invalid source IP CIDR")
	}
	if !strings.Contains(err.Error(), "invalid CIDR") {
		t.Errorf("expected invalid-CIDR wrap, got: %v", err)
	}
}

func TestNewChecker_PropagatesDomainAllowlistError(t *testing.T) {
	// Open wildcard is rejected by NewDomainAllowlist.
	_, err := NewChecker(nil, []string{"*:443"}, nil)
	if err == nil {
		t.Fatal("expected error for open-wildcard domain entry")
	}
	if !strings.Contains(err.Error(), "open wildcard") {
		t.Errorf("expected open-wildcard error, got: %v", err)
	}
}

func TestNewChecker_PropagatesPrivateIPCheckerError(t *testing.T) {
	_, err := NewChecker(nil, nil, []string{"bogus-cidr"})
	if err == nil {
		t.Fatal("expected error for invalid exception CIDR")
	}
	if !strings.Contains(err.Error(), "invalid exception CIDR") {
		t.Errorf("expected invalid-exception-CIDR error, got: %v", err)
	}
}

func TestCheckConnect_AllowsValidRequest(t *testing.T) {
	c := newCheckerForTest(t,
		[]string{"10.0.0.0/8"},
		[]string{"api.openai.com:443"},
		nil,
		[]net.IP{net.ParseIP("104.18.0.1")},
		nil,
	)
	if err := c.CheckConnect(context.Background(), net.ParseIP("10.1.2.3"), "api.openai.com", "443"); err != nil {
		t.Errorf("expected allow, got: %v", err)
	}
}

func TestCheckConnect_DeniesUnlistedSourceIP(t *testing.T) {
	c := newCheckerForTest(t,
		[]string{"10.0.0.0/8"},
		[]string{"api.openai.com:443"},
		nil,
		[]net.IP{net.ParseIP("104.18.0.1")},
		nil,
	)
	err := c.CheckConnect(context.Background(), net.ParseIP("8.8.8.8"), "api.openai.com", "443")
	if !errors.Is(err, ErrIPDenied) {
		t.Errorf("expected ErrIPDenied for off-list source, got: %v", err)
	}
}

func TestCheckConnect_DeniesUnlistedDestination(t *testing.T) {
	c := newCheckerForTest(t,
		[]string{"10.0.0.0/8"},
		[]string{"api.openai.com:443"},
		nil,
		[]net.IP{net.ParseIP("104.18.0.1")},
		nil,
	)
	err := c.CheckConnect(context.Background(), net.ParseIP("10.1.2.3"), "evil.example.com", "443")
	if !errors.Is(err, ErrDomainDenied) {
		t.Errorf("expected ErrDomainDenied, got: %v", err)
	}
}

func TestCheckConnect_DeniesPrivateResolvedIP(t *testing.T) {
	c := newCheckerForTest(t,
		[]string{"10.0.0.0/8"},
		[]string{"sneaky.example.com:443"},
		nil,
		// Domain is allowed, but host resolves to a private IP — must reject.
		[]net.IP{net.ParseIP("192.168.1.5")},
		nil,
	)
	err := c.CheckConnect(context.Background(), net.ParseIP("10.1.2.3"), "sneaky.example.com", "443")
	if !errors.Is(err, ErrPrivateIP) {
		t.Errorf("expected ErrPrivateIP wrap, got: %v", err)
	}
}

func TestCheckConnect_OrderIsIPThenDomainThenDNS(t *testing.T) {
	// All three checks would fail; verify IP wins (first gate).
	c := newCheckerForTest(t,
		[]string{"10.0.0.0/8"},
		[]string{"sneaky.example.com:443"},
		nil,
		[]net.IP{net.ParseIP("192.168.1.5")},
		nil,
	)
	err := c.CheckConnect(context.Background(), net.ParseIP("8.8.8.8"), "evil.example.com", "443")
	if !errors.Is(err, ErrIPDenied) {
		t.Errorf("expected IP gate to fire first, got: %v", err)
	}

	// Now make IP pass but domain fail; DNS would also fail — verify domain wins.
	err = c.CheckConnect(context.Background(), net.ParseIP("10.1.2.3"), "evil.example.com", "443")
	if !errors.Is(err, ErrDomainDenied) {
		t.Errorf("expected domain gate to fire after IP passes, got: %v", err)
	}
}

func TestSwapDomainAllowlist_MergesStaticAndDynamic(t *testing.T) {
	// Static YAML entry: api.openai.com:443
	c, err := NewChecker(
		[]string{"10.0.0.0/8"},
		[]string{"api.openai.com:443"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	c.privateIPCheck.resolver = &mockResolver{addrs: []net.IPAddr{{IP: net.ParseIP("104.18.0.1")}}}

	// Push a DB-derived entry; expect static + dynamic both to allow.
	c.SwapDomainAllowlist([]string{"api.anthropic.com:443"}, silentLogger())

	if err := c.CheckConnect(context.Background(), net.ParseIP("10.0.0.1"), "api.openai.com", "443"); err != nil {
		t.Errorf("static entry should survive swap, got: %v", err)
	}
	if err := c.CheckConnect(context.Background(), net.ParseIP("10.0.0.1"), "api.anthropic.com", "443"); err != nil {
		t.Errorf("dynamic entry should be active after swap, got: %v", err)
	}
	if err := c.CheckConnect(context.Background(), net.ParseIP("10.0.0.1"), "evil.example.com", "443"); !errors.Is(err, ErrDomainDenied) {
		t.Errorf("unrelated host must remain denied, got: %v", err)
	}
}

func TestSwapDomainAllowlist_DedupesOverlappingEntries(t *testing.T) {
	// Both static and dynamic supply the same host — must not double-add and
	// must not trip an "open wildcard" parse error.
	c, err := NewChecker(
		[]string{"10.0.0.0/8"},
		[]string{"api.openai.com:443"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	c.privateIPCheck.resolver = &mockResolver{addrs: []net.IPAddr{{IP: net.ParseIP("104.18.0.1")}}}

	c.SwapDomainAllowlist([]string{"api.openai.com:443", "api.openai.com:443"}, silentLogger())

	if err := c.CheckConnect(context.Background(), net.ParseIP("10.0.0.1"), "api.openai.com", "443"); err != nil {
		t.Errorf("post-dedupe allow failed: %v", err)
	}
}

func TestSwapDomainAllowlist_KeepsCurrentOnInvalidEntry(t *testing.T) {
	// Open wildcard is rejected by NewDomainAllowlist; bad Hub payload must
	// not blank out the static entry.
	c, err := NewChecker(
		[]string{"10.0.0.0/8"},
		[]string{"api.openai.com:443"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	c.privateIPCheck.resolver = &mockResolver{addrs: []net.IPAddr{{IP: net.ParseIP("104.18.0.1")}}}

	c.SwapDomainAllowlist([]string{"*:443"}, silentLogger())

	// Static entry must still resolve.
	if err := c.CheckConnect(context.Background(), net.ParseIP("10.0.0.1"), "api.openai.com", "443"); err != nil {
		t.Errorf("expected previous domain allowlist to remain active, got: %v", err)
	}
}
