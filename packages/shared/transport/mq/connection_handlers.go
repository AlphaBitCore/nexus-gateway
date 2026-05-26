package mq

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// disconnectWatchdogThreshold is the time after a NATS disconnect before
// the watchdog escalates the WARN-level disconnect log to an ERROR.
//
// Picked so a routine NATS restart (single-instance deploys typically
// hand back the listener within 5-15 s) doesn't pollute
// `/infrastructure/errors`, but a genuinely stuck reconnect loop —
// auth misconfig, broker hard-down, network partition — gets surfaced
// before operators have to notice missing audit data.
const disconnectWatchdogThreshold = 60 * time.Second

// newConnectionHandlers returns the four lifecycle callback functions
// used by every natsmq connection. Factored into a single helper so
// Producer and Consumer share identical semantics, AND so tests can
// invoke the handlers directly without standing up a real NATS server.
//
// role labels the log lines ("producer" / "consumer") so a stuck side
// is identifiable. threshold is the disconnect-duration watchdog
// horizon — prod callers pass disconnectWatchdogThreshold; tests pass
// a short value to verify timing.
//
// Semantics summary:
//
//   - Disconnect → WARN, plus start a one-shot timer
//   - Reconnect  → INFO, plus cancel the running watchdog
//   - Closed     → ERROR (terminal under MaxReconnects(-1)); cancel watchdog
//   - Async err  → ERROR (slow-consumer, permission, max-payload, etc.)
//   - Watchdog fires when threshold elapses without a Reconnect — ERROR
//     so the diag pipeline picks it up and `/infrastructure/errors`
//     shows "NATS still disconnected past 60s".
//
// All four returned closures share a single *atomic.Pointer[time.Timer]
// so the watchdog can be cancelled cleanly on reconnect/close without
// torn reads.
func newConnectionHandlers(
	role string,
	threshold time.Duration,
	logger *slog.Logger,
) (
	onDisconnect func(*nats.Conn, error),
	onReconnect func(*nats.Conn),
	onClosed func(*nats.Conn),
	onAsyncErr func(*nats.Conn, *nats.Subscription, error),
) {
	var watchdog atomic.Pointer[time.Timer]

	cancelWatchdog := func() {
		if t := watchdog.Swap(nil); t != nil {
			t.Stop()
		}
	}

	onDisconnect = func(_ *nats.Conn, err error) {
		logger.Warn("natsmq: "+role+" disconnected", "error", err)
		t := time.AfterFunc(threshold, func() {
			// Double-check the timer is still the active one; a quick
			// reconnect could have swapped this slot to nil already.
			// time.AfterFunc handles its own Stop() race but reading
			// the pointer here is defensive against a future refactor.
			logger.Error("natsmq: "+role+" still disconnected past threshold",
				"thresholdSec", int(threshold/time.Second),
				"role", role)
		})
		if old := watchdog.Swap(t); old != nil {
			// Should not happen — nats.go serialises connection-state
			// callbacks — but cancel defensively.
			old.Stop()
		}
	}

	onReconnect = func(_ *nats.Conn) {
		cancelWatchdog()
		logger.Info("natsmq: " + role + " reconnected")
	}

	onClosed = func(c *nats.Conn) {
		cancelWatchdog()
		lastErr := ""
		if c != nil {
			if e := c.LastError(); e != nil {
				lastErr = e.Error()
			}
		}
		logger.Error("natsmq: "+role+" connection closed (terminal — no further reconnect attempts)",
			"lastError", lastErr,
			"role", role)
	}

	onAsyncErr = func(_ *nats.Conn, sub *nats.Subscription, err error) {
		subject := ""
		if sub != nil {
			subject = sub.Subject
		}
		logger.Error("natsmq: "+role+" async error",
			"subject", subject,
			"error", err,
			"role", role)
	}

	return onDisconnect, onReconnect, onClosed, onAsyncErr
}
