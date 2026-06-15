package wiring

import (
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/protectionpause"
	lifecycle "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/state"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// InitKillSwitch creates the fleet kill switch.
func InitKillSwitch(logger *slog.Logger) *killswitch.Switch {
	return killswitch.New(logger)
}

// InitProtectionPause creates the user-initiated protection pauser.
// Constructed early so the status collector can read paused state.
func InitProtectionPause(ks *killswitch.Switch) *protectionpause.Pauser {
	return protectionpause.New(ks)
}

// LifecycleEmitterConfig groups the config for the lifecycle emitter.
type LifecycleEmitterConfig struct {
	ThingID      string
	AgentVersion string
	Logger       *slog.Logger
}

// InitLifecycleEmitter creates the lifecycle emitter. Ships user/system
// lifecycle events (startup, shutdown, pause, resume, sso_login) to Hub via
// the diag-event pipeline. Nil-safe: when tc is nil (HubURL absent / Start
// failed) the emitter skips the Hub push and only records locally.
func InitLifecycleEmitter(
	tc *thingclient.Client,
	auditQueue *auditqueue.Queue,
	cfg LifecycleEmitterConfig,
) *lifecycle.Emitter {
	lcfg := lifecycle.Config{
		Recorder:     auditQueue,
		ThingID:      cfg.ThingID,
		AgentVersion: cfg.AgentVersion,
		Logger:       cfg.Logger,
	}
	// Assign the Pusher only for a non-nil client: a nil *thingclient.Client
	// stored in the interface field would read as non-nil inside the emitter
	// and crash the first emit (typed-nil interface).
	if tc != nil {
		lcfg.Pusher = tc
	}
	return lifecycle.New(lcfg)
}

// EmitShutdownGracefully emits an agent.shutdown lifecycle event and
// then sleeps briefly so the WS outbox has time to flush the message
// to Hub before the caller cancels the main context. Without the
// flush window, the WS write pump's select sees ctx.Done at the same
// instant outCh has a pending diag_event envelope, and exits before
// the write — losing the shutdown row from Hub's view.
//
// Call this at EVERY shutdown-triggering cancel site (signal handler,
// user-quit flag watcher, status-IPC SHUTDOWN) BEFORE the cancel().
// 200 ms is empirically enough on a healthy local-Hub round-trip.
func EmitShutdownGracefully(e *lifecycle.Emitter, reason string) {
	if e == nil {
		return
	}
	e.Shutdown(reason)
	time.Sleep(200 * time.Millisecond)
}
