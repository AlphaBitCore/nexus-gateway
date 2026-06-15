package wiring

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	proxyserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/server"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// ListenerDeps bundles all dependencies for building the proxy server.
type ListenerDeps struct {
	Cfg                  *config.Config
	Logger               *slog.Logger
	AccessChecker        *access.Checker
	ConnManager          *conn.Manager
	IdleTimeout          time.Duration
	ShutdownCord         *conn.ShutdownCoordinator
	PinningTracker       *tlsbump.PinningTracker
	ExemptionStore       *exemption.Store
	KillSwitch           *killswitch.KillSwitch
	PayloadCaptureStore  *payloadcapture.Store
	DomainEngine         *domain.Engine
	AdapterRegistry      *traffic.AdapterRegistry
	NormalizeRegistry    *normalizecore.Registry
	UpstreamTransport    *tlsbump.UpstreamTransport
	CertCache            *cache.CertCache
	ComplianceResolver   *compliance.PolicyResolver
	AuditEmitter         *compliance.AuditEmitter
	StreamingLiveConfig  streaming.LiveConfig
	PerHookTimeout       time.Duration
	TotalTimeout         time.Duration
	ParallelHooks        bool
	StreamingPolicyStore *streampolicy.Store
	// AttestationVerifier is wired by wiring/attestation.go when attestation
	// is enabled in ComplianceConfig.AttestationEnabled. Nil disables the feature.
	AttestationVerifier *proxyserver.AttestationVerifier
}

// InitProxyServer assembles and returns the ProxyServer. Start is not called
// here; the caller launches it in a goroutine.
func InitProxyServer(d ListenerDeps) *proxyserver.ProxyServer {
	proxyCfg := proxyserver.ProxyConfig{
		Checker:                  d.AccessChecker,
		ConnManager:              d.ConnManager,
		IdleTimeout:              d.IdleTimeout,
		ShutdownCord:             d.ShutdownCord,
		PinningTracker:           d.PinningTracker,
		ExemptionStore:           d.ExemptionStore,
		KillSwitchChecker:        d.KillSwitch.IsEngaged,
		PayloadCaptureStore:      d.PayloadCaptureStore,
		AllowUnlistedPassthrough: d.Cfg.AccessControl.AllowUnlistedPassthrough,
		DomainEngine:             d.DomainEngine,
		AdapterRegistry:          d.AdapterRegistry,
		NormalizeRegistry:        d.NormalizeRegistry,
		OnboardingEnabled:        d.Cfg.Onboarding.Enabled,
		OnboardingCPUIBaseURL:    d.Cfg.Onboarding.CPUIBaseURL,
		AttestationVerifier:      d.AttestationVerifier,
		// Reject-body verbosity comes from the yaml rejectResponse block.
		// Without this wiring the zero value (stealth) silently overrides
		// the configured level and every refusal body is a bare Forbidden.
		RejectConfig: tlsbump.RejectConfig{
			DefaultLevel: tlsbump.RejectLevel(d.Cfg.Compliance.RejectResponse.DefaultLevel),
			ContactInfo:  d.Cfg.Compliance.RejectResponse.ContactInfo,
		},
	}
	if d.Cfg.Compliance.Enabled && d.ComplianceResolver != nil {
		proxyCfg.CompliancePipeline = d.ComplianceResolver
		proxyCfg.AuditEmitter = d.AuditEmitter
		proxyCfg.StreamingConfig = d.StreamingLiveConfig
		proxyCfg.PerHookTimeout = d.PerHookTimeout
		proxyCfg.TotalTimeout = d.TotalTimeout
		proxyCfg.ParallelHooks = d.ParallelHooks
		proxyCfg.StreamingPolicyStore = d.StreamingPolicyStore
	}
	return proxyserver.NewProxyServer(
		proxyCfg,
		d.UpstreamTransport,
		d.CertCache.GetCert,
		d.Logger,
	)
}
