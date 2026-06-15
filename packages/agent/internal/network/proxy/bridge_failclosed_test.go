package proxy

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// failClosedBridgeDeps builds the minimal BridgeDeps needed to reach the
// bumpConnectionFn fallback branch: a real (self-CA) TLS engine, an in-memory
// audit queue, a real upstream transport, and the supplied domain engine.
func failClosedBridgeDeps(t *testing.T, eng *domain.Engine) BridgeDeps {
	t.Helper()
	tlsEng, err := agentTLS.NewEngine(nil, nil, 10, time.Hour)
	if err != nil {
		t.Fatalf("tls engine: %v", err)
	}
	q, err := auditqueue.NewQueue(":memory:", nil)
	if err != nil {
		t.Fatalf("audit queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	up, err := tlsbump.NewUpstreamTransport(8, 30*time.Second, 2*time.Second)
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	return BridgeDeps{
		TLSEngine:    tlsEng,
		Upstream:     up,
		AuditQueue:   q,
		DomainEngine: eng,
	}
}

func mkExactDomain(host string, behavior domain.AdapterErrorBehavior) domain.InterceptionDomain {
	return domain.InterceptionDomain{
		ID:                "id-" + host,
		Name:              host,
		HostPattern:       host,
		HostMatchType:     domain.HostMatchExact,
		NetworkZone:       domain.ZonePublic,
		DefaultPathAction: domain.PathActionProcess,
		OnAdapterError:    behavior,
		Enabled:           true,
	}
}

// withSeamedBump replaces bumpConnectionFn with one that returns the given
// error (simulating a bump failure) and restores it after the test. It also
// installs an opaqueDialContext spy that records whether the fail-open relay
// fallback dialed the upstream.
func withSeamedBump(t *testing.T, bumpErr error) (relayDialed *atomic.Bool) {
	t.Helper()
	origBump := bumpConnectionFn
	origDial := opaqueDialContext
	t.Cleanup(func() {
		bumpConnectionFn = origBump
		opaqueDialContext = origDial
	})
	bumpConnectionFn = func(_ context.Context, _ net.Conn, _ string, _ func(*tls.ClientHelloInfo) (*tls.Certificate, error), _ *tlsbump.UpstreamTransport, _ *slog.Logger, _ ...tlsbump.BumpOption) error {
		return bumpErr
	}
	var dialed atomic.Bool
	opaqueDialContext = func(ctx context.Context, addr string) (net.Conn, error) {
		dialed.Store(true)
		// Return a closed pipe end so opaqueRelay returns promptly.
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	return &dialed
}

// TestBumpFlow_FailClosed_RefusesNoOpaqueRelay is the safety-critical agent
// assertion for FIX 1: when the matched domain is FAIL_CLOSED and the bump
// fails, BumpFlow must refuse (return the error) WITHOUT opaque-relaying the
// flow uninspected.
func TestBumpFlow_FailClosed_RefusesNoOpaqueRelay(t *testing.T) {
	relayDialed := withSeamedBump(t, tls.AlertError(48))

	eng := domain.NewEngine()
	if err := eng.Swap([]domain.InterceptionDomain{mkExactDomain("api.example.com", domain.AdapterErrorFailClosed)}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	deps := failClosedBridgeDeps(t, eng)

	client, _ := net.Pipe()
	defer client.Close()

	err := BumpFlow(context.Background(), client, []byte{0x16, 0x03, 0x01}, "api.example.com", 443, "flow-1", FlowProcess{}, deps)
	if err == nil {
		t.Fatal("FAIL_CLOSED bump failure MUST return an error (flow refused)")
	}
	if relayDialed.Load() {
		t.Fatal("FAIL_CLOSED domain MUST NOT opaque-relay — the flow was relayed uninspected")
	}
}

// TestBumpFlow_FailOpen_StillOpaqueRelays confirms the unchanged default: a
// matched FAIL_OPEN domain still falls open to opaque relay on bump failure.
func TestBumpFlow_FailOpen_StillOpaqueRelays(t *testing.T) {
	relayDialed := withSeamedBump(t, tls.AlertError(48))

	eng := domain.NewEngine()
	if err := eng.Swap([]domain.InterceptionDomain{mkExactDomain("api.example.com", domain.AdapterErrorFailOpen)}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	deps := failClosedBridgeDeps(t, eng)

	client, _ := net.Pipe()
	defer client.Close()

	_ = BumpFlow(context.Background(), client, []byte{0x16, 0x03, 0x01}, "api.example.com", 443, "flow-2", FlowProcess{}, deps)
	if !relayDialed.Load() {
		t.Fatal("FAIL_OPEN domain MUST still opaque-relay on bump failure (fail-open preserved)")
	}
}

// TestBumpFlow_UnmatchedHost_StillOpaqueRelays confirms an unmatched host
// (e.g. system/DNS-adjacent traffic, or any host with no domain rule) still
// fails open even when a FAIL_CLOSED domain exists for a different host.
func TestBumpFlow_UnmatchedHost_StillOpaqueRelays(t *testing.T) {
	relayDialed := withSeamedBump(t, tls.AlertError(48))

	eng := domain.NewEngine()
	if err := eng.Swap([]domain.InterceptionDomain{mkExactDomain("api.example.com", domain.AdapterErrorFailClosed)}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	deps := failClosedBridgeDeps(t, eng)

	client, _ := net.Pipe()
	defer client.Close()

	_ = BumpFlow(context.Background(), client, []byte{0x16, 0x03, 0x01}, "other.example.com", 443, "flow-3", FlowProcess{}, deps)
	if !relayDialed.Load() {
		t.Fatal("unmatched host MUST still opaque-relay (fail-open preserved for non-matched hosts)")
	}
}

// TestBumpFlow_NilDomainEngine_StillOpaqueRelays confirms the nil-engine path
// stays fail-open (ShouldFailClosed is nil-safe).
func TestBumpFlow_NilDomainEngine_StillOpaqueRelays(t *testing.T) {
	relayDialed := withSeamedBump(t, tls.AlertError(48))

	deps := failClosedBridgeDeps(t, nil)

	client, _ := net.Pipe()
	defer client.Close()

	_ = BumpFlow(context.Background(), client, []byte{0x16, 0x03, 0x01}, "api.example.com", 443, "flow-4", FlowProcess{}, deps)
	if !relayDialed.Load() {
		t.Fatal("nil DomainEngine MUST fail open (opaque relay)")
	}
}
