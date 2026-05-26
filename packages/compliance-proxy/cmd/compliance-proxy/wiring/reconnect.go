package wiring

import (
	"context"
	"log/slog"
	"time"

	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"

	runtimeserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/server"
)

// ReconnectDeps bundles what's needed to compose the OnReconnect callback.
type ReconnectDeps struct {
	ThingClient     *thingclient.Client // may be nil — no-op when nil
	StaticInfo      registry.StaticInfo
	StaticInfoReady bool
	RuntimeServer   *runtimeserver.Server
	ReconnectBuffer *shareddiag.ReconnectBuffer
	Logger          *slog.Logger
}

// WireOnReconnect installs a single composed OnReconnect callback that:
//  1. Re-pushes static_info to Hub.
//  2. Drains the break-glass pending file.
//  3. Drains the diag reconnect buffer.
//
// No-op when ThingClient is nil.
func WireOnReconnect(d ReconnectDeps) {
	if d.ThingClient == nil {
		return
	}
	tc := d.ThingClient
	staticInfo := d.StaticInfo
	pushStatic := d.StaticInfoReady
	buf := d.ReconnectBuffer
	logger := d.Logger
	srv := d.RuntimeServer

	tc.OnReconnect(func() {
		doReconnectWork(tc, staticInfo, pushStatic, buf, logger, srv)
	})
}

// doReconnectWork is the body of the OnReconnect callback. It is extracted so
// tests can invoke it directly without requiring a live WebSocket reconnect.
func doReconnectWork(
	tc *thingclient.Client,
	staticInfo registry.StaticInfo,
	pushStatic bool,
	buf *shareddiag.ReconnectBuffer,
	logger *slog.Logger,
	srv *runtimeserver.Server,
) {
	if pushStatic {
		ctxPush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := tc.UpdateStaticInfo(ctxPush, staticInfo); err != nil {
			logger.Warn("static_info push failed on reconnect", "error", err)
		}
		cancel()
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if drained, err := srv.ReplayPending(drainCtx); err != nil {
		logger.Warn("break-glass replay on reconnect failed", "error", err)
	} else if drained {
		logger.Info("break-glass pending drained on reconnect")
	}

	if buf != nil {
		diagCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, evt := range buf.Drain() {
			_ = tc.PushDiagEvent(diagCtx, evt)
		}
	}
}
