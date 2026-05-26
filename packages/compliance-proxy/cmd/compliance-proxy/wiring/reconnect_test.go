package wiring

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

func makeStaticInfo() registry.StaticInfo {
	return registry.StaticInfo{}
}

func TestWireOnReconnect_NilThingClientIsNoop(t *testing.T) {
	// When ThingClient is nil the callback must never be installed.
	// No panic expected.
	srv := buildTestRuntimeServer(t)
	d := ReconnectDeps{
		ThingClient:     nil,
		StaticInfo:      makeStaticInfo(),
		StaticInfoReady: true,
		RuntimeServer:   srv,
		ReconnectBuffer: shareddiag.NewReconnectBuffer(shareddiag.ReconnectBufferConfig{}),
		Logger:          testLogger(),
	}
	WireOnReconnect(d) // must not panic
}

func TestWireOnReconnect_NilBufferIsNoop(t *testing.T) {
	// ThingClient nil → early return. Buffer nil should also be safe.
	srv := buildTestRuntimeServer(t)
	d := ReconnectDeps{
		ThingClient:     nil,
		StaticInfo:      makeStaticInfo(),
		StaticInfoReady: false,
		RuntimeServer:   srv,
		ReconnectBuffer: nil,
		Logger:          testLogger(),
	}
	WireOnReconnect(d) // must not panic
}

func TestCaptureThingClientResult_NilClientReturnsEmpty(t *testing.T) {
	cfg := &config.Config{}
	result := CaptureThingClientResult(nil, cfg, time.Now(), testLogger())
	if result.Client != nil {
		t.Error("expected nil Client for nil input")
	}
	if result.StaticInfoReady {
		t.Error("expected StaticInfoReady=false for nil Client")
	}
}

func TestCaptureThingClientResult_NonNilClientPopulatesResult(t *testing.T) {
	cfg := &config.Config{}
	cfg.PublicURL = "http://proxy.test:3040"
	result := CaptureThingClientResult(sharedTestThingClient, cfg, time.Now(), testLogger())
	if result.Client == nil {
		t.Error("expected non-nil Client")
	}
	if !result.StaticInfoReady {
		t.Error("expected StaticInfoReady=true")
	}
}
