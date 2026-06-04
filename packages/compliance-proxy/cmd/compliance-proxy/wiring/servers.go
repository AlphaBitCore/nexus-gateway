// servers.go collapses the multi-field proxy-server + runtime + health-server
// initialisations into single calls so main.go stays under its 150-LOC budget.
package wiring

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/access"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	proxyserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/server"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	runtimeserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/server"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

// ProxyServerDeps is a compact view of the deps needed by InitProxyServerFull.
type ProxyServerDeps struct {
	Cfg                  *config.Config
	Logger               *slog.Logger
	AccessChecker        *access.Checker
	ConnManager          *conn.Manager
	ShutdownCoord        *conn.ShutdownCoordinator
	UpstreamTransport    *tlsbump.UpstreamTransport
	CertResult           CertResult
	CompRes              ComplianceResult
	DomainEngine         *domain.Engine
	AdapterRegistry      *traffic.AdapterRegistry
	NormalizeRegistry    *normalizecore.Registry
	KillSwitch           *killswitch.KillSwitch
	ExemptionStore       *exemption.Store
	PayloadCaptureStore  *payloadcapture.Store
	StreamingPolicyStore *streampolicy.Store
}

// InitProxyServerFull assembles and returns the ProxyServer from a compact
// deps struct. This is the preferred entry point from main.go.
func InitProxyServerFull(d ProxyServerDeps) *proxyserver.ProxyServer {
	return InitProxyServer(ListenerDeps{
		Cfg:                  d.Cfg,
		Logger:               d.Logger,
		AccessChecker:        d.AccessChecker,
		ConnManager:          d.ConnManager,
		IdleTimeout:          ParseDurationOrDefault(d.Cfg.Connections.IdleTimeout, 300*time.Second),
		ShutdownCord:         d.ShutdownCoord,
		PinningTracker:       InitPinningTracker(d.Cfg),
		ExemptionStore:       d.ExemptionStore,
		KillSwitch:           d.KillSwitch,
		PayloadCaptureStore:  d.PayloadCaptureStore,
		DomainEngine:         d.DomainEngine,
		AdapterRegistry:      d.AdapterRegistry,
		NormalizeRegistry:    d.NormalizeRegistry,
		UpstreamTransport:    d.UpstreamTransport,
		CertCache:            d.CertResult.CertCache,
		ComplianceResolver:   d.CompRes.Resolver,
		AuditEmitter:         d.CompRes.Emitter,
		StreamingLiveConfig:  d.CompRes.LiveConfig,
		PerHookTimeout:       d.CompRes.PerHookTmout,
		TotalTimeout:         d.CompRes.TotalTmout,
		ParallelHooks:        d.CompRes.Parallel,
		StreamingPolicyStore: d.StreamingPolicyStore,
		AttestationVerifier:  InitAttestationVerifier(d.Cfg, d.Logger),
	})
}

// ServersDeps is a compact view of what's needed to stand up the runtime API
// server + health/metrics HTTP server.
type ServersDeps struct {
	Cfg               *config.Config
	Logger            *slog.Logger
	Readiness         *atomic.Bool
	KillSwitch        *killswitch.KillSwitch
	ConnManager       *conn.Manager
	StartTime         time.Time
	RedisClient       redis.UniversalClient
	ExemptionStore    *exemption.Store
	ThingClient       *thingclient.Client
	ProxyID           string
	BuildVersion      string
	CertResult        CertResult
	CacheManager      *cache.Manager
	DomainEngine      *domain.Engine
	CompRes           ComplianceResult
	PayloadCapture    *payloadcapture.Store
	ConfigKeyRecorder *runtimeintrospect.KeyStateRecorder
	ServiceToken      string
}

// ServersResult bundles the two runtime services returned from InitServers.
type ServersResult struct {
	RuntimeSrv   *runtimeserver.Server
	HealthServer *http.Server
	HealthMux    *http.ServeMux
}

// InitServers stands up the runtime API server and the health/metrics HTTP
// server. Neither is started here — the caller launches both in goroutines.
func InitServers(d ServersDeps) ServersResult {
	runtimeAddr := d.Cfg.RuntimeAPI.ListenAddress
	if runtimeAddr == "" {
		runtimeAddr = "127.0.0.1:3002"
	}
	runtimeSrv, _ := InitRuntimeAPIServer(RuntimeAPIDeps{
		Addr:           runtimeAddr,
		Logger:         d.Logger,
		KillSwitch:     d.KillSwitch,
		ConnManager:    d.ConnManager,
		StartTime:      d.StartTime,
		RedisClient:    d.RedisClient,
		ExemptionStore: d.ExemptionStore,
		ThingClient:    d.ThingClient,
		ProxyID:        d.ProxyID,
		DataDir:        d.Cfg.DataDir,
		Readiness:      d.Readiness,
	})
	healthMux, _ := InitHealthHandler(HealthDeps{
		ProxyID:           d.ProxyID,
		BuildVersion:      d.BuildVersion,
		Logger:            d.Logger,
		Readiness:         d.Readiness,
		ThingClient:       d.ThingClient,
		KillSwitch:        d.KillSwitch,
		ExemptionStore:    d.ExemptionStore,
		PayloadCapture:    d.PayloadCapture,
		CacheManager:      d.CacheManager,
		DomainEngine:      d.DomainEngine,
		HookConfigCache:   d.CompRes.HookConfigCache,
		ConnManager:       d.ConnManager,
		CertIssuer:        d.CertResult.Issuer,
		ServiceToken:      d.ServiceToken,
		ConfigKeyRecorder: d.ConfigKeyRecorder,
	})
	healthServer := &http.Server{Addr: d.Cfg.Metrics.Address, Handler: healthMux}
	return ServersResult{
		RuntimeSrv:   runtimeSrv,
		HealthServer: healthServer,
		HealthMux:    healthMux,
	}
}
