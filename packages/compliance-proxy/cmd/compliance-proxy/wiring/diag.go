package wiring

import (
	"context"
	"log/slog"
	"time"

	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// DiagResult holds the updated logger and the reconnect buffer (for
// OnReconnect drain wiring in main.go).
type DiagResult struct {
	Logger          *slog.Logger
	ReconnectBuffer *shareddiag.ReconnectBuffer
}

// InitDiagSink installs the shared diag slog sink so ERROR+ log records are
// routed to Hub's thing_diag_event pipeline. Returns the updated logger and
// the reconnect buffer the caller must drain on Hub reconnect.
//
// When tc is nil (Hub not configured) the original logger is returned
// unchanged.
func InitDiagSink(
	logger *slog.Logger,
	tc *thingclient.Client,
	proxyID string,
	opsReg *registry.Registry,
) DiagResult {
	buf := shareddiag.NewReconnectBuffer(shareddiag.ReconnectBufferConfig{})
	sink := shareddiag.NewSlogSink(shareddiag.SlogSinkConfig{
		ThingClient:     tc,
		ReconnectBuffer: buf,
		IsWSConnected: func() bool {
			return tc != nil && tc.Mode() == thingclient.ModeWSConnected
		},
		ThingID: proxyID,
		Source:  "compliance-proxy",
		OpsReg:  opsReg,
	})
	updated := slog.New(shareddiag.NewMultiHandler(logger.Handler(), sink))
	slog.SetDefault(updated)
	return DiagResult{Logger: updated, ReconnectBuffer: buf}
}

// PushStartupDiagEvent fires a lifecycle "compliance-proxy started" event to
// Hub in a background goroutine. No-op when tc is nil.
func PushStartupDiagEvent(tc *thingclient.Client, proxyID, buildVersion string) {
	if tc == nil {
		return
	}
	go func() {
		time.Sleep(600 * time.Millisecond)
		pushStartupDiagEventSync(tc, proxyID, buildVersion)
	}()
}

// pushStartupDiagEventSync pushes the startup lifecycle event synchronously.
// Extracted from PushStartupDiagEvent for deterministic testing.
func pushStartupDiagEventSync(tc *thingclient.Client, proxyID, buildVersion string) {
	diagCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tc.PushDiagEvent(diagCtx, registry.DiagEvent{
		ThingID:    proxyID,
		OccurredAt: time.Now().UTC(),
		EventType:  registry.EventTypeLifecycle,
		Level:      registry.LevelInfo,
		Source:     "compliance-proxy",
		Message:    "compliance-proxy started",
		Attrs:      map[string]any{"version": buildVersion},
	})
}
