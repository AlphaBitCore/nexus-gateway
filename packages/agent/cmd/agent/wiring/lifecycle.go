package wiring

import (
	"log/slog"

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
// failed) the emitter no-ops gracefully.
func InitLifecycleEmitter(
	tc *thingclient.Client,
	auditQueue *auditqueue.Queue,
	cfg LifecycleEmitterConfig,
) *lifecycle.Emitter {
	return lifecycle.New(lifecycle.Config{
		Pusher:       tc,
		Recorder:     auditQueue,
		ThingID:      cfg.ThingID,
		AgentVersion: cfg.AgentVersion,
		Logger:       cfg.Logger,
	})
}
