// Package configloader provides a generic dispatcher for shadow config
// keys pushed by Nexus Hub through thingclient.OnConfigChanged.
//
// Every server-side Thing (Nexus Hub, Control Plane, AI Gateway,
// Compliance Proxy) and the desktop Agent consume a set of per-key
// shadow blobs and must:
//
//  1. parse the raw bytes into a typed value;
//  2. apply the value to a subsystem (cache reload, atomic snapshot
//     swap, executor swap, ...);
//  3. record the apply outcome on the thingclient.OutcomeTracker so
//     the next shadow_report carries it back to Hub;
//  4. return the reported bytes Hub will store in
//     thing.reported[key] (which may differ from desired — e.g. the
//     kill-switch reports the live snapshot rather than the desired
//     bytes, in case the toggle was rejected locally).
//
// Without this package each service's main.go hand-rolls a 200-line
// switch statement that mixes bookkeeping (logging, OutcomeTracker
// calls, reported assembly) with the per-key apply logic. The Loader
// extracts the bookkeeping so services declare only per-key Parse +
// Apply pairs.
package configloader

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/thingclient"
)

// Puller fetches the live bytes for a Cat B shadow key from an
// upstream source (typically Hub via HTTP). Server-side Things receive
// the full bytes inline on the WS shadow tick and do not need a
// puller; the desktop Agent registers its Cat B keys via
// RegisterRawPull, and the Loader HTTP-pulls the real payload from Hub
// before applying — regardless of what bytes the WS tick carried.
// `needsPull` is the registration-time Handler flag below, NOT a field
// Hub stamps into the pushed state; the Loader never inspects the
// pushed bytes for a `{needsPull: true}` marker.
//
// The Loader invokes the puller for every key whose Handler.NeedsPull
// is true. Returning an error fails that single key without aborting
// the rest of the apply (continue-on-error invariant).
type Puller func(ctx context.Context, key string) ([]byte, error)

// Option mutates a Loader at construction time. Use New(...,
// WithPuller(p)) — direct field mutation is not supported (Loader
// fields are unexported).
type Option func(*Loader)

// WithPuller wires a Cat B puller onto the Loader. Required only when
// at least one registered Handler has NeedsPull=true. A loader with no
// pull-needing handlers can be constructed without a puller.
func WithPuller(p Puller) Option {
	return func(l *Loader) { l.puller = p }
}

// Handler describes how to apply one shadow config key.
//
// Parse is optional; when nil the Apply function receives the zero
// value of V and the apply path is expected to use the raw bytes via
// the alternate RegisterRaw entry point. Apply is mandatory.
type Handler[V any] struct {
	// Key is the shadow config key name (e.g. "killswitch",
	// "hooks"). Must be unique within a Loader.
	Key string

	// Parse converts the raw shadow bytes into a typed value. Nil =
	// no parse step — only valid when V is the empty struct and Apply
	// ignores its first argument. Most handlers should use ParseJSON
	// or supply a custom function.
	Parse func(raw []byte) (V, error)

	// Apply applies the parsed value. desiredVer is the per-key
	// version Hub assigned, useful for logging. The first return value
	// is the bytes Hub will persist in thing.reported[key]; when nil
	// the Loader echoes the desired bytes (the common case). When the
	// reported state must reflect the LIVE local view (kill-switch
	// snapshot, hot-reloaded subsystem state), return the marshalled
	// snapshot.
	Apply func(ctx context.Context, v V, desiredVer int64) ([]byte, error)

	// NeedsPull marks the key as Cat B (pull-on-signal): the WS shadow
	// tick is treated purely as a signal — whatever desired bytes Hub
	// pushed are DISCARDED, and the real payload is HTTP-pulled from Hub
	// before Apply runs. The Loader invokes its configured Puller for
	// these keys based on this flag alone; it does not look for a
	// `{needsPull: true}` marker inside the pushed state. Server-side
	// Things leave this false (Hub pushes full bytes inline); the
	// desktop Agent sets it true for `exemptions`, `hooks`,
	// `interception_domains`, etc.
	NeedsPull bool
}

// rawHandler is the type-erased wrapper stored inside the Loader.
// Each generic Register call boxes its Handler[V] into one of these so
// the dispatch loop in Apply does not need to know V.
type rawHandler struct {
	key       string
	needsPull bool
	apply     func(ctx context.Context, raw []byte, desiredVer int64) ([]byte, error)
}

// Loader dispatches shadow config updates to registered handlers,
// records outcomes onto thingclient.OutcomeTracker, and assembles the
// reported map that flows back to Hub.
//
// A Loader is safe to use after construction but is NOT safe for
// concurrent registration: services should Register every handler at
// wiring time before tc.OnConfigChanged(l.Handler()) is installed,
// then leave the registration table read-only for the lifetime of the
// process.
type Loader struct {
	logger   *slog.Logger
	outcomes *thingclient.OutcomeTracker
	handlers map[string]rawHandler

	thingID   string
	thingType string

	// puller is the Cat B HTTP-pull callback; nil for server-side
	// Things that receive full bytes inline. See WithPuller.
	puller Puller
}

// New constructs a Loader. outcomes may be nil — Record() calls become
// no-ops and the OnConfigChanged path keeps working, but the apply
// ledger that powers the Nodes-page "last good version" indicator
// stays empty. Production wiring always supplies tc.Outcomes().
//
// Options apply additional configuration (e.g. WithPuller for Cat B
// HTTP-pull semantics).
func New(logger *slog.Logger, outcomes *thingclient.OutcomeTracker, thingID, thingType string, opts ...Option) *Loader {
	if logger == nil {
		logger = slog.Default()
	}
	l := &Loader{
		logger:    logger,
		outcomes:  outcomes,
		handlers:  map[string]rawHandler{},
		thingID:   thingID,
		thingType: thingType,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Register adds a typed Handler to the Loader. Duplicate keys panic —
// a duplicate registration is always a wiring bug and silently
// shadowing the previous registration would make it untraceable.
func Register[V any](l *Loader, h Handler[V]) {
	if h.Key == "" {
		panic("configloader: Register called with empty Key")
	}
	if h.Apply == nil {
		panic(fmt.Sprintf("configloader: Register(%q): Apply is required", h.Key))
	}
	if _, dup := l.handlers[h.Key]; dup {
		panic(fmt.Sprintf("configloader: Register(%q): duplicate registration", h.Key))
	}
	l.handlers[h.Key] = rawHandler{
		key:       h.Key,
		needsPull: h.NeedsPull,
		apply: func(ctx context.Context, raw []byte, desiredVer int64) ([]byte, error) {
			var v V
			if h.Parse != nil {
				parsed, err := h.Parse(raw)
				if err != nil {
					return nil, fmt.Errorf("parse: %w", err)
				}
				v = parsed
			}
			return h.Apply(ctx, v, desiredVer)
		},
	}
}

// RegisterRaw adds a handler that consumes raw bytes directly without
// a parse step. Useful for keys whose payload is opaque to this layer
// (e.g. compliance-proxy's "hooks" key — the bytes are a stub;
// the real reload reads from the local Postgres regardless of payload).
//
// The registered key is treated as Cat A (no HTTP pull). For Cat B
// keys (agent pull-on-signal), use RegisterRawPull instead.
func RegisterRaw(l *Loader, key string, apply func(ctx context.Context, raw []byte, desiredVer int64) ([]byte, error)) {
	registerRawNeedsPull(l, key, false, apply)
}

// RegisterRawPull is RegisterRaw + NeedsPull. The Loader will HTTP-pull
// the live bytes via its configured Puller before invoking apply for
// this key. Constructing the Loader without a Puller and then calling
// RegisterRawPull is a wiring bug — Apply degrades to using the
// (stub) desired bytes and logs WARN.
func RegisterRawPull(l *Loader, key string, apply func(ctx context.Context, raw []byte, desiredVer int64) ([]byte, error)) {
	registerRawNeedsPull(l, key, true, apply)
}

func registerRawNeedsPull(l *Loader, key string, needsPull bool, apply func(ctx context.Context, raw []byte, desiredVer int64) ([]byte, error)) {
	if key == "" {
		panic("configloader: RegisterRaw called with empty key")
	}
	if apply == nil {
		panic(fmt.Sprintf("configloader: RegisterRaw(%q): apply is required", key))
	}
	if _, dup := l.handlers[key]; dup {
		panic(fmt.Sprintf("configloader: RegisterRaw(%q): duplicate registration", key))
	}
	l.handlers[key] = rawHandler{key: key, needsPull: needsPull, apply: apply}
}

// Has reports whether the Loader has a registered handler for key.
// Used by tests + services that want to layer extra logic in front of
// the loader (rare).
func (l *Loader) Has(key string) bool {
	_, ok := l.handlers[key]
	return ok
}

// Keys returns the registered shadow keys in unspecified order. Used
// by tests + introspection endpoints to expose the handler table.
func (l *Loader) Keys() []string {
	out := make([]string, 0, len(l.handlers))
	for k := range l.handlers {
		out = append(out, k)
	}
	return out
}

// Apply dispatches every key in desired to its registered handler and
// returns the reported map. Unknown keys are logged at WARN and
// skipped (the corresponding reported entry is omitted, so Hub keeps
// the previous value).
//
// Per-key failures are recorded on the OutcomeTracker and logged at
// ERROR level but do NOT short-circuit dispatch — every key still
// gets an attempt so a single bad key cannot block the rest of the
// shadow. Apply returns the FIRST per-key error it encountered (with
// the key name wrapped into the message) so the WS layer can surface
// a non-nil error to Hub; the OutcomeTracker carries the full per-key
// error picture independently.
//
// Apply is the function services pass to tc.OnConfigChanged via
// l.Handler(). Direct callers (tests, custom dispatch wrappers) can
// invoke Apply with their own context.
func (l *Loader) Apply(ctx context.Context, desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
	reported := make(map[string]thingclient.ConfigState, len(desired))
	var firstErr error
	for key, cs := range desired {
		l.logger.Info("applying config key",
			"event", "config_apply_start",
			"thing_id", l.thingID,
			"thing_type", l.thingType,
			"config_key", key,
			"desired_ver", cs.Version,
		)
		h, ok := l.handlers[key]
		if !ok {
			l.logger.Warn("unknown shadow config key",
				"thing_id", l.thingID,
				"thing_type", l.thingType,
				"config_key", key,
			)
			continue
		}
		raw := cs.State
		if h.needsPull {
			pulled, perr := l.pullFor(ctx, key)
			if perr != nil {
				l.logger.Error("config pull failed",
					"thing_id", l.thingID,
					"thing_type", l.thingType,
					"config_key", key,
					"error", perr,
				)
				l.outcomes.Record(key, cs.Version, perr)
				if firstErr == nil {
					firstErr = fmt.Errorf("configloader: pull %s: %w", key, perr)
				}
				continue
			}
			raw = pulled
		}
		reportedBytes, err := h.apply(ctx, raw, cs.Version)
		l.outcomes.Record(key, cs.Version, err)
		if err != nil {
			l.logger.Error("config apply failed",
				"thing_id", l.thingID,
				"thing_type", l.thingType,
				"config_key", key,
				"desired_ver", cs.Version,
				"error", err,
			)
			if firstErr == nil {
				firstErr = fmt.Errorf("configloader: apply %s: %w", key, err)
			}
			continue
		}
		out := reportedBytes
		if out == nil {
			out = cs.State
		}
		reported[key] = thingclient.ConfigState{State: out, Version: cs.Version}
		l.logger.Info("config apply succeeded",
			"event", "config_apply_done",
			"thing_id", l.thingID,
			"thing_type", l.thingType,
			"config_key", key,
			"applied_ver", cs.Version,
		)
	}
	return reported, firstErr
}

// Handler returns a function compatible with thingclient.Client.OnConfigChanged.
// Wiring sites should call tc.OnConfigChanged(l.Handler()) once after
// every handler has been registered.
func (l *Loader) Handler() func(map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
	return func(desired map[string]thingclient.ConfigState) (map[string]thingclient.ConfigState, error) {
		return l.Apply(context.Background(), desired)
	}
}

// pullFor invokes the configured Puller for key. Returns an error
// when the Loader has no Puller (a wiring bug — a needsPull handler
// was registered without WithPuller) or the puller itself fails.
func (l *Loader) pullFor(ctx context.Context, key string) ([]byte, error) {
	if l.puller == nil {
		return nil, fmt.Errorf("needsPull handler %q registered but no Puller wired via WithPuller", key)
	}
	return l.puller(ctx, key)
}

// PullKeys returns the registered shadow keys whose handlers are
// flagged NeedsPull (Cat B). Used by RefreshPullKeys and by callers
// that want to inspect the Cat B surface (introspection endpoints).
// Order is unspecified.
func (l *Loader) PullKeys() []string {
	out := make([]string, 0, len(l.handlers))
	for k, h := range l.handlers {
		if h.needsPull {
			out = append(out, k)
		}
	}
	return out
}

// RefreshPullKeys pulls every Cat B key from the configured Puller and applies
// it locally, ignoring whether Hub has materialised a shadow tick for the key.
// Used at boot to bring the agent's local view in sync with operator changes
// made while the agent was offline, and to cover keys Hub never materialised
// into thing.desired. Returns (applied, failed) counts.
//
// Per-key errors are logged at WARN and recorded on the
// OutcomeTracker but do not stop the loop — a single bad key must
// not block the rest of the boot.
//
// Calling RefreshPullKeys without a Puller is a no-op (logged at
// DEBUG); server-side Things do not need a boot-time refresh because
// the first WS shadow tick already carries full bytes.
func (l *Loader) RefreshPullKeys(ctx context.Context) (applied, failed int) {
	if l.puller == nil {
		l.logger.Debug("configloader: RefreshPullKeys skipped — no Puller wired",
			"thing_id", l.thingID,
			"thing_type", l.thingType,
		)
		return 0, 0
	}
	for key, h := range l.handlers {
		if !h.needsPull {
			continue
		}
		raw, err := l.puller(ctx, key)
		if err != nil {
			l.logger.Warn("configloader: startup pull failed",
				"thing_id", l.thingID,
				"thing_type", l.thingType,
				"config_key", key,
				"error", err,
			)
			l.outcomes.Record(key, 0, err)
			failed++
			continue
		}
		if _, err := h.apply(ctx, raw, 0); err != nil {
			l.logger.Warn("configloader: startup apply failed",
				"thing_id", l.thingID,
				"thing_type", l.thingType,
				"config_key", key,
				"error", err,
			)
			l.outcomes.Record(key, 0, err)
			failed++
			continue
		}
		l.logger.Info("configloader: startup pull applied",
			"thing_id", l.thingID,
			"thing_type", l.thingType,
			"config_key", key,
			"bytes", len(raw),
		)
		applied++
	}
	l.logger.Info("configloader: startup refresh complete",
		"thing_id", l.thingID,
		"thing_type", l.thingType,
		"applied", applied,
		"failed", failed,
	)
	return applied, failed
}

// ParseJSON is a generic Parse helper for the common case of
// json.Unmarshal-ing the raw bytes into V. Empty input is treated as
// "no change" and returns the zero value with no error — Hub
// occasionally pushes a tick with an empty State payload for Cat A
// keys whose value has not been materialised yet, and we must not
// surface that as a parse failure.
func ParseJSON[V any]() func(raw []byte) (V, error) {
	return func(raw []byte) (V, error) {
		var v V
		if len(raw) == 0 {
			return v, nil
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return v, err
		}
		return v, nil
	}
}
