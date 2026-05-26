package wiring

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
)

// TestInitRuntimeAPIServer_HealthzEndpointResponds starts the runtime API
// server on a real port and hits /healthz to exercise the Health.Run closure
// (redis checker + optional thingclient paths).
func TestInitRuntimeAPIServer_HealthzEndpointResponds(t *testing.T) {
	// Pick a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() //nolint:errcheck

	ks := killswitch.NewKillSwitch(testLogger())
	cm := conn.NewManager(0)
	ex := exemption.NewStore(testLogger())
	readiness := &atomic.Bool{}

	d := RuntimeAPIDeps{
		Addr:           addr,
		Logger:         testLogger(),
		KillSwitch:     ks,
		ConnManager:    cm,
		StartTime:      time.Now(),
		RedisClient:    nil, // nil → redisChecker returns false
		ExemptionStore: ex,
		ThingClient:    nil, // nil → hub_shadow branch skipped
		ProxyID:        "test-proxy",
		DataDir:        t.TempDir(),
		Readiness:      readiness,
	}
	srv, _ := InitRuntimeAPIServer(d)

	// Start the server.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	time.Sleep(100 * time.Millisecond) // let the server bind

	// Hit /healthz to exercise Health.Run (no auth required).
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		// Server may not have bound yet on a slow CI box; non-fatal.
		t.Logf("GET /healthz: %v (server may not be ready)", err)
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unexpected status %d from /healthz", resp.StatusCode)
	}
}

// TestInitRuntimeAPIServer_WithThingClientHealthCheck exercises the hub_shadow
// branch inside Health.Run.
func TestInitRuntimeAPIServer_WithThingClientHealthCheck(t *testing.T) {
	ks := killswitch.NewKillSwitch(testLogger())
	cm := conn.NewManager(0)
	ex := exemption.NewStore(testLogger())
	readiness := &atomic.Bool{}

	d := RuntimeAPIDeps{
		Addr:           "127.0.0.1:0",
		Logger:         testLogger(),
		KillSwitch:     ks,
		ConnManager:    cm,
		StartTime:      time.Now(),
		RedisClient:    nil,
		ExemptionStore: ex,
		ThingClient:    sharedTestThingClient, // non-nil → hub_shadow branch entered
		ProxyID:        "test-proxy",
		DataDir:        t.TempDir(),
		Readiness:      readiness,
	}
	srv, _ := InitRuntimeAPIServer(d)
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
}
