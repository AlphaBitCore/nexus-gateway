package wiring

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	configcache "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/testutil"
	tlscache "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

func buildCertResult(t *testing.T) CertResult {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath, err := testutil.WriteTestCA(dir)
	if err != nil {
		t.Fatalf("WriteTestCA: %v", err)
	}
	iss, err := issuer.NewIssuer(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	lru := tlscache.NewLRUCache(16)
	cc := tlscache.NewCertCache(iss, lru, nil, 23*time.Hour, testLogger())
	return CertResult{Issuer: iss, CertCache: cc}
}

func buildServersDeps(t *testing.T) ServersDeps {
	t.Helper()
	ks := killswitch.NewKillSwitch(testLogger())
	cm := conn.NewManager(0)
	exStore := exemption.NewStore(testLogger())
	captureStore := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	domainEngine := domain.NewEngine()
	cacheManager := configcache.NewManager(5*time.Minute, testLogger())
	keyRecorder := runtimeintrospect.NewKeyStateRecorder()

	return ServersDeps{
		Cfg:               &config.Config{},
		Logger:            testLogger(),
		Readiness:         &atomic.Bool{},
		KillSwitch:        ks,
		ConnManager:       cm,
		StartTime:         time.Now(),
		RedisClient:       nil,
		ExemptionStore:    exStore,
		ThingClient:       nil,
		ProxyID:           "test-proxy",
		BuildVersion:      "v0.0.0-test",
		CertResult:        buildCertResult(t),
		CacheManager:      cacheManager,
		DomainEngine:      domainEngine,
		CompRes:           ComplianceResult{},
		PayloadCapture:    captureStore,
		ConfigKeyRecorder: keyRecorder,
		ServiceToken:      "test-token",
	}
}

func TestInitServers_ReturnsAllComponents(t *testing.T) {
	d := buildServersDeps(t)
	result := InitServers(d)

	if result.RuntimeSrv == nil {
		t.Error("expected non-nil RuntimeSrv")
	}
	if result.HealthServer == nil {
		t.Error("expected non-nil HealthServer")
	}
	if result.HealthMux == nil {
		t.Error("expected non-nil HealthMux")
	}
}

func TestInitServers_CustomRuntimeAddr(t *testing.T) {
	d := buildServersDeps(t)
	d.Cfg.RuntimeAPI.ListenAddress = "127.0.0.1:3099"
	result := InitServers(d)
	if result.RuntimeSrv == nil {
		t.Fatal("expected non-nil RuntimeSrv")
	}
}

func TestInitServers_DefaultRuntimeAddr(t *testing.T) {
	// Empty ListenAddress → falls back to "127.0.0.1:3002".
	d := buildServersDeps(t)
	d.Cfg.RuntimeAPI.ListenAddress = ""
	result := InitServers(d)
	if result.RuntimeSrv == nil {
		t.Fatal("expected non-nil RuntimeSrv")
	}
}

func TestInitServers_WithMetricsAddress(t *testing.T) {
	d := buildServersDeps(t)
	d.Cfg.Metrics.Address = "127.0.0.1:9091"
	result := InitServers(d)
	if result.HealthServer.Addr != "127.0.0.1:9091" {
		t.Errorf("health server addr = %q; want 127.0.0.1:9091", result.HealthServer.Addr)
	}
}
