package wiring

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

func buildMinimalListenerDeps(t *testing.T) ListenerDeps {
	t.Helper()
	certRes := buildCertResult(t)

	accessChecker, err := newMinimalAccessChecker(t), error(nil)
	_ = err

	cm := conn.NewManager(0)
	shutdownCoord := conn.NewShutdownCoordinator(30*time.Second, testLogger())
	pinningTracker := InitPinningTracker(&config.Config{})
	exStore := exemption.NewStore(testLogger())
	ks := killswitch.NewKillSwitch(testLogger())
	captureStore := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	domainEngine := domain.NewEngine()

	adapterRegistry := traffic.NewAdapterRegistry("nexus_test")
	adapterRegistry.Freeze()

	upstream, err := tlsbump.NewUpstreamTransport(0,
		ParseDurationOrDefault("90s", 90*time.Second),
		ParseDurationOrDefault("10s", 10*time.Second),
	)
	if err != nil {
		t.Fatalf("NewUpstreamTransport: %v", err)
	}

	return ListenerDeps{
		Cfg:                  &config.Config{},
		Logger:               testLogger(),
		AccessChecker:        accessChecker,
		ConnManager:          cm,
		IdleTimeout:          5 * time.Minute,
		ShutdownCord:         shutdownCoord,
		PinningTracker:       pinningTracker,
		ExemptionStore:       exStore,
		KillSwitch:           ks,
		PayloadCaptureStore:  captureStore,
		DomainEngine:         domainEngine,
		AdapterRegistry:      adapterRegistry,
		UpstreamTransport:    upstream,
		CertCache:            certRes.CertCache,
		ComplianceResolver:   nil,
		AuditEmitter:         nil,
		StreamingPolicyStore: streampolicy.NewStore(streampolicy.DefaultPolicy()),
	}
}

func TestInitProxyServer_ComplianceDisabled_ReturnsServer(t *testing.T) {
	d := buildMinimalListenerDeps(t)
	d.Cfg.Compliance.Enabled = false
	srv := InitProxyServer(d)
	if srv == nil {
		t.Fatal("expected non-nil ProxyServer")
	}
}

func TestInitProxyServer_ComplianceEnabledNilResolver_ReturnsServer(t *testing.T) {
	// Compliance enabled but resolver nil — the compliance branch in InitProxyServer
	// is guarded by `cfg.Compliance.Enabled && resolver != nil`.
	d := buildMinimalListenerDeps(t)
	d.Cfg.Compliance.Enabled = true
	d.ComplianceResolver = nil
	srv := InitProxyServer(d)
	if srv == nil {
		t.Fatal("expected non-nil ProxyServer")
	}
}

func TestInitProxyServerFull_ReturnsServer(t *testing.T) {
	infra, err := InitInfra(&config.Config{}, testLogger())
	if err != nil {
		t.Fatalf("InitInfra: %v", err)
	}
	certRes := buildCertResult(t)
	ks := killswitch.NewKillSwitch(testLogger())
	exStore := exemption.NewStore(testLogger())
	captureStore := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	domainEngine := domain.NewEngine()

	d := ProxyServerDeps{
		Cfg:                  &config.Config{},
		Logger:               testLogger(),
		AccessChecker:        infra.AccessChecker,
		ConnManager:          infra.ConnManager,
		ShutdownCoord:        infra.ShutdownCoord,
		UpstreamTransport:    infra.UpstreamTransport,
		CertResult:           certRes,
		CompRes:              ComplianceResult{},
		DomainEngine:         domainEngine,
		AdapterRegistry:      infra.AdapterRegistry,
		KillSwitch:           ks,
		ExemptionStore:       exStore,
		PayloadCaptureStore:  captureStore,
		StreamingPolicyStore: streampolicy.NewStore(streampolicy.DefaultPolicy()),
	}
	srv := InitProxyServerFull(d)
	if srv == nil {
		t.Fatal("expected non-nil ProxyServer from InitProxyServerFull")
	}
}
