package wiring

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
)

func buildRuntimeAPIDeps(t *testing.T) RuntimeAPIDeps {
	t.Helper()
	ks := killswitch.NewKillSwitch(testLogger())
	cm := conn.NewManager(0)
	ex := exemption.NewStore(testLogger())
	readiness := &atomic.Bool{}

	return RuntimeAPIDeps{
		Addr:           "127.0.0.1:0",
		Logger:         testLogger(),
		KillSwitch:     ks,
		ConnManager:    cm,
		StartTime:      time.Now(),
		RedisClient:    nil,
		ExemptionStore: ex,
		ThingClient:    nil,
		ProxyID:        "test-proxy",
		DataDir:        "",
		Readiness:      readiness,
	}
}

func TestInitRuntimeAPIServer_ReturnsServerAndTokenAuth(t *testing.T) {
	d := buildRuntimeAPIDeps(t)
	srv, tokenAuth := InitRuntimeAPIServer(d)
	if srv == nil {
		t.Fatal("expected non-nil runtime server")
	}
	if tokenAuth == nil {
		t.Fatal("expected non-nil token auth")
	}
}

func TestInitRuntimeAPIServer_WithNilOptionals_NoNilDeref(t *testing.T) {
	// ThingClient=nil, RedisClient=nil — must not panic.
	d := buildRuntimeAPIDeps(t)
	d.ThingClient = nil
	d.RedisClient = nil
	srv, _ := InitRuntimeAPIServer(d)
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestInitRuntimeAPIServer_WithThingClient_ThingclientWired(t *testing.T) {
	d := buildRuntimeAPIDeps(t)
	// Use the shared thingclient (not started, just for the non-nil branch).
	d.ThingClient = sharedTestThingClient
	srv, _ := InitRuntimeAPIServer(d)
	if srv == nil {
		t.Fatal("expected non-nil server when ThingClient is non-nil")
	}
}
