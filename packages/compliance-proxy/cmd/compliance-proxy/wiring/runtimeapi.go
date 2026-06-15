package wiring

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	runtimeserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/server"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// RuntimeAPIDeps bundles all dependencies for the runtime API server.
type RuntimeAPIDeps struct {
	Addr           string
	APIToken       string // COMPLIANCE_PROXY_API_TOKEN; boot-required, fail-closed
	Logger         *slog.Logger
	KillSwitch     *killswitch.KillSwitch
	ConnManager    *conn.Manager
	StartTime      time.Time
	RedisClient    redis.UniversalClient
	ExemptionStore *exemption.Store
	ThingClient    *thingclient.Client // may be nil
	ProxyID        string
	DataDir        string
	Readiness      *atomic.Bool // controls 200/503 on the health endpoint
}

// InitRuntimeAPIServer constructs and returns the runtime API server + token
// auth. The server is not started here; the caller launches it in a goroutine.
func InitRuntimeAPIServer(d RuntimeAPIDeps) (*runtimeserver.Server, *auth.TokenAuth) {
	tokenAuth := auth.NewTokenAuth(d.APIToken)

	redisChecker := func() bool {
		if d.RedisClient == nil {
			return false
		}
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer pingCancel()
		return d.RedisClient.Ping(pingCtx).Err() == nil
	}

	deps := handler.RuntimeDeps{
		KillSwitch:     d.KillSwitch,
		ConnManager:    d.ConnManager,
		StartTime:      d.StartTime,
		RedisChecker:   redisChecker,
		Logger:         d.Logger,
		Readiness:      d.Readiness,
		ExemptionStore: d.ExemptionStore,

		// /runtime/* read-surface wiring.
		ThingID:        d.ProxyID,
		ThingType:      "compliance-proxy",
		KillswitchSnap: d.KillSwitch,
		ExemptionSnap:  d.ExemptionStore,
		Health: config.HealthChecks{
			Run: func(ctx context.Context) map[string]string {
				out := map[string]string{}
				if redisChecker() {
					out["redis"] = "ok"
				} else {
					out["redis"] = "unavailable"
				}
				if d.ThingClient != nil {
					if d.ThingClient.ReportedVer() >= d.ThingClient.DesiredVer() {
						out["hub_shadow"] = "ok"
					} else {
						out["hub_shadow"] = "out_of_sync"
					}
				}
				return out
			},
		},
	}
	if d.ThingClient != nil {
		deps.Thingclient = d.ThingClient
		deps.BreakGlassReporter = d.ThingClient
	}
	deps.ExemptionRebuilder = d.ExemptionStore
	deps.DataDir = d.DataDir

	srv := runtimeserver.NewServer(d.Addr, deps, tokenAuth)
	return srv, tokenAuth
}
