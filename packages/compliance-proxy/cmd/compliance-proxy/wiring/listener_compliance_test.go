package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// TestInitProxyServer_ComplianceEnabledWithResolver exercises the branch
// `if cfg.Compliance.Enabled && complianceResolver != nil`.
func TestInitProxyServer_ComplianceEnabledWithResolver(t *testing.T) {
	d := buildMinimalListenerDeps(t)
	d.Cfg.Compliance.Enabled = true
	d.ComplianceResolver = pipeline.NewPolicyResolver(nil, builtins.Registry, testLogger())
	d.AuditEmitter = nil
	d.StreamingPolicyStore = streampolicy.NewStore(streampolicy.DefaultPolicy())

	srv := InitProxyServer(d)
	if srv == nil {
		t.Fatal("expected non-nil ProxyServer with compliance resolver")
	}
}

// TestInitProxyServer_ThreadsRejectResponseConfig: the yaml rejectResponse
// block must reach the proxy server. The zero value silently downgrades every
// refusal body to the stealth "Forbidden", so a dropped wire here makes the
// configured verbosity level a dead knob (and the runbook's body claims false).
func TestInitProxyServer_ThreadsRejectResponseConfig(t *testing.T) {
	d := buildMinimalListenerDeps(t)
	d.Cfg.Compliance.RejectResponse.DefaultLevel = 2
	d.Cfg.Compliance.RejectResponse.ContactInfo = "ops@example.com"

	srv := InitProxyServer(d)

	rc := srv.RejectConfig()
	if rc.DefaultLevel != tlsbump.RejectLevelDetailed {
		t.Fatalf("DefaultLevel = %d, want %d (RejectLevelDetailed) threaded from yaml", rc.DefaultLevel, tlsbump.RejectLevelDetailed)
	}
	if rc.ContactInfo != "ops@example.com" {
		t.Fatalf("ContactInfo = %q, want the yaml value threaded through", rc.ContactInfo)
	}
}

// helper shims for TestInitProxyServerFull_ComplianceEnabledPath.

func domainEngineFor(_ *testing.T) *domain.Engine { return domain.NewEngine() }

func newKS(t *testing.T) *killswitch.KillSwitch {
	t.Helper()
	return killswitch.NewKillSwitch(testLogger())
}

func newExemptionStore(t *testing.T) *exemption.Store {
	t.Helper()
	return exemption.NewStore(testLogger())
}

func newPayloadCaptureStore() *payloadcapture.Store {
	return payloadcapture.NewStore(payloadcapture.DefaultConfig())
}

// TestInitProxyServerFull_ComplianceEnabledPath assembles a ProxyServerDeps
// with a non-nil compliance result (resolver populated).
func TestInitProxyServerFull_ComplianceEnabledPath(t *testing.T) {
	infra, err := InitInfra(&config.Config{}, testLogger())
	if err != nil {
		t.Fatalf("InitInfra: %v", err)
	}
	certRes := buildCertResult(t)
	resolver := pipeline.NewPolicyResolver(nil, builtins.Registry, testLogger())
	compRes := ComplianceResult{
		Resolver: resolver,
	}

	cfg := &config.Config{}
	cfg.Compliance.Enabled = true

	d := ProxyServerDeps{
		Cfg:                  cfg,
		Logger:               testLogger(),
		AccessChecker:        infra.AccessChecker,
		ConnManager:          infra.ConnManager,
		ShutdownCoord:        infra.ShutdownCoord,
		UpstreamTransport:    infra.UpstreamTransport,
		CertResult:           certRes,
		CompRes:              compRes,
		DomainEngine:         domainEngineFor(t),
		AdapterRegistry:      infra.AdapterRegistry,
		KillSwitch:           newKS(t),
		ExemptionStore:       newExemptionStore(t),
		PayloadCaptureStore:  newPayloadCaptureStore(),
		StreamingPolicyStore: streampolicy.NewStore(streampolicy.DefaultPolicy()),
	}
	srv := InitProxyServerFull(d)
	if srv == nil {
		t.Fatal("expected non-nil ProxyServer")
	}
}
