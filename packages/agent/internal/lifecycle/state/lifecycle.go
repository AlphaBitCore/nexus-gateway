// Package lifecycle emits user / system lifecycle events from the agent
// to Hub via the opsmetrics diag-event pipeline (EventType=lifecycle).
//
// This replaces the previous practice of writing agent.startup /
// agent.shutdown rows into the audit_events table — that table is the
// NETransparentProxyProvider's per-connection decision log, where
// lifecycle rows were structurally out of place (different column
// shape, different consumer, different retention policy). Routing
// lifecycle through diag_event aligns admin's CP UI view
// (infrastructure/errors filtered to type=lifecycle) with the agent's
// own Dashboard "Activity" view of "what this daemon has done
// recently".
//
// All emits are best-effort: a failed WS push or a disconnected client
// silently drops the event. Process exit and lifecycle UX must not
// depend on the event reaching Hub.
package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// Pusher is the subset of *thingclient.Client used to ship lifecycle
// events. Matches the existing diag.ThingClientPusher contract so an
// agent already holding a *thingclient.Client can pass it directly.
type Pusher interface {
	PushDiagEvent(ctx context.Context, evt registry.DiagEvent) error
}

// Recorder is the subset of *audit.Queue used to mirror lifecycle
// events into the agent's local SQLCipher store. Satisfied by
// *audit.Queue.RecordLifecycle. The local mirror exists so the
// Dashboard "Activity" page can render a usable timeline without
// querying Hub. Optional — when nil the emitter still pushes via the
// Pusher and just skips the local insert.
type Recorder interface {
	RecordLifecycle(id string, occurredAt time.Time, action, message, level string, attrs map[string]any) error
}

// Emitter is the per-process handle the agent's main() wires once
// after enrollment + thingclient.Start. All methods are safe to call
// from any goroutine.
type Emitter struct {
	pusher       Pusher
	recorder     Recorder
	thingID      string
	agentVersion string
	osInfo       map[string]any
	logger       *slog.Logger
}

// Config bundles the static fields stamped on every emitted event.
// ThingID / AgentVersion / OSInfo travel as DiagEvent metadata so the
// admin-side CP UI can scope filters without rejoining against the
// thing table on every read.
//
// Recorder is the local SQLCipher mirror — when present every emit
// also lands a row in the agent's lifecycle_event table for the
// Dashboard "Activity" page to read. Nil disables the local mirror
// (e.g. in tests, or for callers that don't want a local record).
type Config struct {
	Pusher       Pusher
	Recorder     Recorder
	ThingID      string
	AgentVersion string
	OSInfo       map[string]any
	Logger       *slog.Logger
}

// New returns an Emitter ready to ship lifecycle events. nil Pusher
// produces an emitter whose methods are no-ops — useful for the
// pre-enrollment cmdRun path where the daemon has not yet connected
// to Hub but still wants to emit at unenroll/quit time.
func New(cfg Config) *Emitter {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Emitter{
		pusher:       cfg.Pusher,
		recorder:     cfg.Recorder,
		thingID:      cfg.ThingID,
		agentVersion: cfg.AgentVersion,
		osInfo:       cfg.OSInfo,
		logger:       logger,
	}
}

// Action names — stable wire identifiers consumed by CP UI filters.
// Don't rename without coordinating a CP UI deploy.
const (
	ActionStartup   = "agent.startup"
	ActionShutdown  = "agent.shutdown"
	ActionPaused    = "agent.paused"
	ActionResumed   = "agent.resumed"
	ActionSSOLogin  = "agent.sso_login"
	ActionSSOLogout = "agent.sso_logout"
)

// Startup signals that this daemon process has finished bootstrap and
// is now accepting traffic. Called once per process lifetime, after
// the thing client is healthy and the platform shim has been started.
func (e *Emitter) Startup() {
	e.emit(ActionStartup, "agent started", nil)
}

// Shutdown signals an intentional / graceful daemon exit. Reason
// distinguishes operator-initiated quit (user_quit_flag), signal-
// driven shutdown (signal=15), and context cancel (ctx_done). Crashes
// emit a separate FATAL diag_event via diag.Recover and do NOT call
// here.
func (e *Emitter) Shutdown(reason string) {
	attrs := map[string]any{"reason": reason}
	e.emit(ActionShutdown, "agent shutdown", attrs)
}

// Paused signals user-initiated protection pause. seconds=0 means
// indefinite ("until I resume"). The admin-side filter sees the
// duration so they can distinguish a 5-minute coffee break from a
// suspicious indefinite suppression.
func (e *Emitter) Paused(seconds int) {
	attrs := map[string]any{"durationSec": seconds}
	e.emit(ActionPaused, "protection paused", attrs)
}

// Resumed signals user-initiated protection resume — covers explicit
// menu-driven Resume only. Auto-resume from the Pause timer firing is
// NOT emitted today (would require a hook inside the timer callback);
// admin can infer auto-resume time from the matching Paused event's
// durationSec attribute.
func (e *Emitter) Resumed() {
	e.emit(ActionResumed, "protection resumed", nil)
}

// SSOLogin signals successful SSO enrollment / re-authentication.
// Currently called from the enroll-sso flow; future Switch identity
// menu path will call here too.
func (e *Emitter) SSOLogin(email string) {
	attrs := map[string]any{"email": email}
	e.emit(ActionSSOLogin, "user signed in via SSO", attrs)
}

// SSOLogout signals user-initiated sign-out. Wired when the menu's
// Sign Out affordance lands (separate task).
func (e *Emitter) SSOLogout() {
	e.emit(ActionSSOLogout, "user signed out", nil)
}

// emit is the single fan-out point: builds the DiagEvent, calls the
// pusher (no-op when nil), mirrors to the local recorder (no-op when
// nil), and logs the outcome. Best-effort throughout — any error
// path logs a Debug line and returns silently so lifecycle UX never
// stalls on a transient WS hiccup or a disk-full SQLCipher write.
//
// Hub push and local mirror are INDEPENDENT: a failure in one does
// not block the other. The two paths serve different audiences (CP
// UI infrastructure pages vs the agent's own Activity tab), so a
// Hub outage must not leave the user staring at an empty timeline
// on their own machine.
func (e *Emitter) emit(action, message string, attrs map[string]any) {
	if e == nil {
		return
	}
	mergedAttrs := mergeAttrs(map[string]any{"action": action}, attrs)
	now := time.Now().UTC()
	evtID := newEventID()

	// Local mirror — write FIRST so the Activity tab never lags Hub
	// in the common case where the WS push is fast. Best-effort:
	// the SQLCipher INSERT could fail (disk full, locked DB) and we
	// still want to attempt the Hub push.
	if e.recorder != nil {
		if err := e.recorder.RecordLifecycle(evtID, now, action, message, registry.LevelInfo, mergedAttrs); err != nil {
			e.logger.Debug("lifecycle local mirror failed",
				"action", action,
				"error", err,
			)
		}
	}

	// Hub push — fire even when recorder failed; the two paths are
	// independent. A nil pusher (pre-enrollment / HubURL absent)
	// silently skips this branch.
	if e.pusher != nil {
		evt := registry.DiagEvent{
			ThingID:      e.thingID,
			OccurredAt:   now,
			Level:        registry.LevelInfo,
			EventType:    registry.EventTypeLifecycle,
			Source:       "agent",
			Message:      message,
			Attrs:        mergedAttrs,
			RepeatCount:  1,
			AgentVersion: e.agentVersion,
			OSInfo:       e.osInfo,
			// MessageHash left empty — Hub-side insertDiagDrainEvent
			// fills it via ComputeMessageHash so dedup works against
			// the canonical (level|source|message) tuple.
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := e.pusher.PushDiagEvent(ctx, evt); err != nil {
			// Debug, not Warn: the WS outbox is sometimes briefly
			// stalled and lifecycle events are not critical.
			// Surfacing as Warn would spam logs every time the user
			// clicks Pause and the WS happened to be reconnecting.
			e.logger.Debug("lifecycle emit failed",
				"action", action,
				"error", err,
			)
			return
		}
	}
	e.logger.Debug("lifecycle event emitted",
		"action", action,
	)
}

// newEventID returns a 16-byte random hex string. Used as the primary
// key on both the local mirror and (when forwarded) the Hub
// thing_diag_event row, so the same logical event has the same ID at
// both ends — making it trivial to correlate a row a user sees in
// their Activity tab with the matching row in admin's CP UI without
// guessing.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Astronomically unlikely; fall back to a time-based id so
		// the emit still works even if the entropy source is broken.
		return "lc-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// mergeAttrs merges the per-action attrs into a fresh map seeded with
// the base "action" tag. The action key is the load-bearing one for
// CP UI filtering; per-action attrs are diagnostic colour (reason,
// duration, email, etc).
func mergeAttrs(base, extra map[string]any) map[string]any {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
