package wiring

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/cache"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/health"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/tls/issuer"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/domain"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// HealthDeps bundles all dependencies needed to build the health + debug handler.
type HealthDeps struct {
	ProxyID           string
	BuildVersion      string
	Logger            *slog.Logger
	Readiness         *atomic.Bool
	ThingClient       *thingclient.Client // may be nil
	KillSwitch        *killswitch.KillSwitch
	ExemptionStore    *exemption.Store
	PayloadCapture    *payloadcapture.Store
	CacheManager      *cache.Manager
	DomainEngine      *domain.Engine
	HookConfigCache   *pipeline.HookConfigCache // may be nil
	ConnManager       connCounter
	CertIssuer        *issuer.Issuer
	ServiceToken      string
	ConfigKeyRecorder *runtimeintrospect.KeyStateRecorder
}

// connCounter is satisfied by conn.Manager.
type connCounter interface {
	ActiveCount() int64
}

// InitHealthHandler wires the /health + /metrics + /debug/runtime +
// /management/ca-cert endpoints and returns the ServeMux and introspect registry.
func InitHealthHandler(d HealthDeps) (*http.ServeMux, *runtimeintrospect.Registry) {
	var shadowProbe health.ShadowProbe
	if d.ThingClient != nil {
		shadowProbe = &shadowProbeAdapter{client: d.ThingClient}
	}
	// Register shared-hooks regex cache counters on the default registerer
	// so they are exposed alongside compliance-proxy's own metrics.
	core.RegisterRegexCacheMetrics(prometheus.DefaultRegisterer)
	// Register the compliance pipeline metric set (hook decisions /
	// durations / fail-opens, storage-redaction outcomes) under the nexus
	// namespace so the pipeline's package-level metrics export on /metrics
	// instead of recording into their isolated no-op defaults.
	pipeline.RegisterDefaultMetrics("nexus")
	mux := health.NewHandler(d.Readiness, shadowProbe, prometheus.DefaultRegisterer)

	// --- Runtime introspection (e31-s7) ---
	introspectReg := runtimeintrospect.New("compliance-proxy", d.ProxyID, d.BuildVersion)
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "config.killswitch",
		Fn: func(_ context.Context) (any, error) {
			return d.KillSwitch.State(), nil
		},
	})
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "config.exemptions",
		Fn: func(_ context.Context) (any, error) {
			return d.ExemptionStore.Snapshot(), nil
		},
	})
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "config.payload_capture",
		Fn: func(_ context.Context) (any, error) {
			return d.PayloadCapture.Get(), nil
		},
	})
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "cache.allowlists",
		Fn: func(ctx context.Context) (any, error) {
			data, err := d.CacheManager.Get(ctx, cache.CategoryAllowlists)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"data":              data,
				"staleness_seconds": d.CacheManager.StalenessSeconds(cache.CategoryAllowlists),
			}, nil
		},
	})
	// e31-s13: full InterceptionDomain + InterceptionPath snapshot.
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "cache.interception_domains_full",
		Fn: func(_ context.Context) (any, error) {
			return d.DomainEngine.Snapshot(), nil
		},
	})
	if d.HookConfigCache != nil {
		introspectReg.Register(runtimeintrospect.SourceFunc{
			SourceName: "config.hooks",
			Fn: func(_ context.Context) (any, error) {
				return d.HookConfigCache.Snapshot(), nil
			},
		})
	}
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "cache.observability",
		Fn: func(ctx context.Context) (any, error) {
			data, err := d.CacheManager.Get(ctx, cache.CategoryObservability)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"data":              data,
				"staleness_seconds": d.CacheManager.StalenessSeconds(cache.CategoryObservability),
			}, nil
		},
	})
	introspectReg.Register(runtimeintrospect.SourceFunc{
		SourceName: "runtime.active_tunnels",
		Fn: func(_ context.Context) (any, error) {
			return d.ConnManager.ActiveCount(), nil
		},
	})
	// Fill runtime-introspection gaps for keys that have no richer in-memory
	// cache view registered above.
	d.ConfigKeyRecorder.RegisterAll(introspectReg, []string{
		"log_level",
		"compliance_streaming",
		"onboarding",
	})
	mux.Handle("/debug/runtime", introspectReg.Handler(runtimeintrospect.HandlerOptions{
		Token:  d.ServiceToken,
		Logger: d.Logger,
	}))

	// Management CA cert endpoint — allows clients to download the proxy CA cert.
	mux.HandleFunc("/management/ca-cert", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pem := d.CertIssuer.CACertPEM()
		if len(pem) == 0 {
			http.Error(w, "CA not loaded", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="nexus-proxy-ca.crt"`)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(pem)
	})

	return mux, introspectReg
}

// shadowProbeAdapter adapts *thingclient.Client to health.ShadowProbe.
type shadowProbeAdapter struct {
	client *thingclient.Client
}

func (p *shadowProbeAdapter) HasReported() bool {
	return !p.client.LastReportedAtTime().IsZero()
}

func (p *shadowProbeAdapter) LastReportAge() time.Duration {
	last := p.client.LastReportedAtTime()
	if last.IsZero() {
		return 0
	}
	return time.Since(last)
}

func (p *shadowProbeAdapter) StaleAfter() time.Duration {
	return 2 * p.client.HeartbeatInterval()
}
