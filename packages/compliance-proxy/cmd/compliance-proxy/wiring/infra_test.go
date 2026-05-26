package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
)

func TestInitInfra_DefaultConfig(t *testing.T) {
	cfg := &config.Config{}
	res, err := InitInfra(cfg, testLogger())
	if err != nil {
		t.Fatalf("InitInfra: %v", err)
	}
	if res.AccessChecker == nil {
		t.Error("expected non-nil AccessChecker")
	}
	if res.ConnManager == nil {
		t.Error("expected non-nil ConnManager")
	}
	if res.ShutdownCoord == nil {
		t.Error("expected non-nil ShutdownCoordinator")
	}
	if res.UpstreamTransport == nil {
		t.Error("expected non-nil UpstreamTransport")
	}
	if res.AdapterRegistry == nil {
		t.Error("expected non-nil AdapterRegistry")
	}
}

func TestInitInfra_WithDomainAllowlist(t *testing.T) {
	cfg := &config.Config{}
	cfg.AccessControl.DomainAllowlist = []string{"api.example.com:443", "*.internal.example.com:443"}
	res, err := InitInfra(cfg, testLogger())
	if err != nil {
		t.Fatalf("InitInfra: %v", err)
	}
	if res.AccessChecker == nil {
		t.Error("expected non-nil AccessChecker with domain allowlist")
	}
}

func TestInitInfra_WithAllowUnlistedPassthrough(t *testing.T) {
	cfg := &config.Config{}
	cfg.AccessControl.AllowUnlistedPassthrough = true
	res, err := InitInfra(cfg, testLogger())
	if err != nil {
		t.Fatalf("InitInfra with AllowUnlistedPassthrough: %v", err)
	}
	if res.AccessChecker == nil {
		t.Error("expected non-nil AccessChecker")
	}
}

func TestInitInfra_WithConnLimits(t *testing.T) {
	cfg := &config.Config{}
	cfg.Connections.MaxConcurrentTunnels = 100
	cfg.Connections.ShutdownGracePeriod = "10s"
	cfg.Upstream.MaxConnsPerHost = 50
	cfg.Upstream.IdleConnTimeout = "90s"
	cfg.Upstream.DialTimeout = "10s"
	res, err := InitInfra(cfg, testLogger())
	if err != nil {
		t.Fatalf("InitInfra with conn limits: %v", err)
	}
	if res.ConnManager == nil {
		t.Error("expected non-nil ConnManager")
	}
}

func TestInitInfra_InvalidSourceIPReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.AccessControl.SourceIPAllowlist = []string{"not-an-ip-cidr!!!"}
	_, err := InitInfra(cfg, testLogger())
	if err == nil {
		t.Error("expected error for invalid source IP CIDR")
	}
}
