package wiring

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/keystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/backpressure"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/diag"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/localrollup"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/spilluploader"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
	shareddiag "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag"
	sharedintro "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/diag/runtimeintrospect"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// InitAuditQueue opens the SQLCipher-encrypted audit queue. The keystore is
// injected by the composition root (cmd_run passes the platform store) so
// unit tests inject keystore.NewMemoryStore() — wiring code must never
// construct the platform store itself: on macOS that opens the real
// Keychain, which prompts for authorization under `go test`.
func InitAuditQueue(dbPath string, ks keystore.Store, logger *slog.Logger) (*auditqueue.Queue, error) {
	dbEncKey, err := keystore.GetOrCreateDBKey(ks)
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		dbPath = "audit.db"
	}
	return auditqueue.NewQueue(dbPath, dbEncKey)
}

// InitBackpressure creates the backpressure store and starts its poll goroutine.
// The poll goroutine exits when ctx is cancelled.
func InitBackpressure(ctx context.Context, auditQueue *auditqueue.Queue, logger *slog.Logger) *backpressure.Store {
	store := backpressure.NewStore(backpressure.Config{
		HighWatermark: 500,
		LowWatermark:  200,
		PollInterval:  2 * time.Second,
		Logger:        logger,
	})
	go store.Poll(ctx, func() int { return auditQueue.UnsyncedCount() })
	return store
}

// DiagBundle groups the diag subsystem objects built by InitDiag.
type DiagBundle struct {
	LocalBuffer     *diag.LocalBuffer
	Dedup           *registry.Dedup
	ReconnectBuffer *shareddiag.ReconnectBuffer
	SlogSink        *shareddiag.SlogSink
	RecoveryCfg     shareddiag.RecoveryConfig
}

// InitDiag creates the local diag buffer, dedup ring, reconnect buffer,
// and slog sink. It migrates the pending_diag_event table and composes
// the multi-handler logger (JSON + diag sink).
//
// Returns the bundle and the composed logger (slog.SetDefault has NOT
// been called — the caller does that).
func InitDiag(
	auditQueue *auditqueue.Queue,
	tc *thingclient.Client,
	thingID, version string,
	opsReg *registry.Registry,
	logger *slog.Logger,
) (DiagBundle, *slog.Logger, error) {
	if err := diag.MigratePendingDiagEvent(auditQueue.DB()); err != nil {
		return DiagBundle{}, nil, err
	}
	localDiagBuffer := diag.NewLocalBuffer(auditQueue.DB(), logger)
	diagDedup := registry.NewDedup(time.Now, 60*time.Second, 100)
	// Producer-side visibility: how many duplicate events the dedup ring
	// suppressed (and folded into a single summary on Tick). Counter is
	// per-severity so a flood of "error" duplicates doesn't get masked by
	// a quieter "warn" stream.
	diagCollapsedCounter := opsReg.NewCounter("diag.dedup_collapsed_total", []string{"thing_type", "severity"})
	diagDedup.SetCollapsedCounter(diagCollapsedCounter, "agent")

	diagDroppedCounter := opsReg.NewCounter("diag.dropped_total", []string{"reason"}).With("reconnect_overflow")
	reconnectBuffer := shareddiag.NewReconnectBuffer(shareddiag.ReconnectBufferConfig{
		MaxLen:  100,
		MaxAge:  5 * time.Minute,
		Dropped: diagDroppedCounter,
		Clock:   time.Now,
		Log:     logger,
	})

	wsConnectedFn := func() bool {
		return tc != nil && tc.Mode() == thingclient.ModeWSConnected
	}
	diagSink := shareddiag.NewSlogSink(shareddiag.SlogSinkConfig{
		ThingClient:     tc,
		LocalBuffer:     localDiagBuffer,
		Dedup:           diagDedup,
		ReconnectBuffer: reconnectBuffer,
		IsWSConnected:   wsConnectedFn,
		ThingID:         thingID,
		Source:          "agent",
		IncludeInfo:     false,
		Level:           slog.LevelError,
	})
	composedLogger := slog.New(shareddiag.NewMultiHandler(logger.Handler(), diagSink))

	recoveryCfg := shareddiag.RecoveryConfig{
		ThingID:      thingID,
		Buffer:       localDiagBuffer,
		AgentVersion: version,
		OSInfo: map[string]any{
			"goos":   runtime.GOOS,
			"goarch": runtime.GOARCH,
		},
		Source: "main",
	}

	bundle := DiagBundle{
		LocalBuffer:     localDiagBuffer,
		Dedup:           diagDedup,
		ReconnectBuffer: reconnectBuffer,
		SlogSink:        diagSink,
		RecoveryCfg:     recoveryCfg,
	}
	return bundle, composedLogger, nil
}

// InitIntrospect creates the runtime introspection registry for the agent.
func InitIntrospect(thingID, version string) *sharedintro.Registry {
	return sharedintro.New("agent", thingID, version)
}

// RegisterConfigIntrospection registers the applied-config snapshot sources
// (kill switch state, payload-capture upload config) on the introspection
// registry so the status API's RUNTIME command can report them.
func RegisterConfigIntrospection(reg *sharedintro.Registry, ks *killswitch.Switch, payloadCapture *payloadcapture.Store) {
	reg.Register(sharedintro.SourceFunc{
		SourceName: "config.killswitch",
		Fn:         func(_ context.Context) (any, error) { return ks.SnapshotState(), nil },
	})
	reg.Register(sharedintro.SourceFunc{
		SourceName: "config.payload_capture",
		Fn:         func(_ context.Context) (any, error) { return payloadCapture.Get(), nil },
	})
}

// InitLocalRollup creates the per-agent local rollup aggregator.
func InitLocalRollup(auditQueue *auditqueue.Queue, logger *slog.Logger) *localrollup.Aggregator {
	return localrollup.New(auditQueue.DB(), logger)
}

// InitSpillUploader creates the spill uploader wired to the Hub HTTP client.
func InitSpillUploader(hubClient *hub.Client) *spilluploader.Uploader {
	return spilluploader.New(hubClient)
}
