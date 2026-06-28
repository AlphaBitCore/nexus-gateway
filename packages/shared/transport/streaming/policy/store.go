package policy

import (
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"sync/atomic"
)

// Store is a hot-swappable holder for the global StreamingPolicy. Each
// data-plane service (compliance-proxy, agent) keeps one Store
// instance, populates it at startup with DefaultPolicy() or whatever
// LoadGlobalDefault returned, and feeds shadow-channel updates through
// ApplyShadowState. Hot paths (BumpFlow, listener.go) call Get() with
// no lock to read the current snapshot.
//
// Mirrors the shape of payloadcapture.Store so the agent's thingclient
// wiring can attach the same way (configsync.AdapterFunc(store.ApplyShadowState)).
type Store struct {
	current atomic.Pointer[Policy]
}

// NewStore returns a Store seeded with the supplied initial Policy.
// Pass DefaultPolicy() at boot if no Hub-driven config is available
// yet; the next shadow push will replace it.
func NewStore(initial Policy) *Store {
	s := &Store{}
	s.current.Store(&initial)
	return s
}

// Get returns the current Policy snapshot. Safe for concurrent reads;
// returned value is a copy so callers can mutate freely.
func (s *Store) Get() Policy {
	if s == nil {
		return DefaultPolicy()
	}
	if p := s.current.Load(); p != nil {
		return *p
	}
	return DefaultPolicy()
}

// Set replaces the current Policy snapshot atomically. Used by tests
// and any subsystem that needs to override the stream-policy without
// pushing a Hub config update (rare).
func (s *Store) Set(p Policy) {
	if s == nil {
		return
	}
	s.current.Store(&p)
}

// ApplyShadowState satisfies the configsync.ShadowApplier interface so
// agent's thingclient can wire this Store into the per-key shadow
// dispatch. The raw payload is the JSON shape DecodeGlobalPolicy
// understands. Empty payloads load DefaultPolicy() — matching the
// behaviour LoadGlobalDefault has on a missing system_metadata row.
func (s *Store) ApplyShadowState(_ context.Context, raw json.RawMessage) error {
	p, err := DecodeGlobalPolicy(raw)
	if err != nil {
		return err
	}
	s.Set(p)
	return nil
}

// RawConfigLoader fetches the admin's streaming-policy JSON blob from
// a caller-chosen source (system_metadata table over *sql.DB, pgxpool
// helper, in-memory test fixture, …). Empty or nil RawMessage signals
// "no admin config yet"; non-nil error signals a transient read
// failure.
type RawConfigLoader func(ctx context.Context) (json.RawMessage, error)

// BootStore is the canonical 3-service (agent / compliance-proxy /
// ai-gateway) helper that produces a Store seeded with the admin-
// configured Policy when available, falling back to DefaultPolicy()
// otherwise. Each data-plane service was previously hand-rolling the
// same boot-default → load-raw → decode → log → install cycle, with
// minor drift between sites; centralising the boilerplate here means:
//
//   - the same warn/info log lines fire from every service, so
//     operators see consistent boot output regardless of which
//     service log they read
//   - the same defaulting semantics (empty raw → keep default, decode
//     error → keep default + warn) apply uniformly, so an admin
//     config typo can't poison just one service
//   - new defaulting tweaks (e.g. validation upgrades) ship by editing
//     one helper instead of three
//
// nil load = no DB-driven load attempted (agent path — agents have no
// local config DB, every config arrives via Hub shadow push, so
// BootStore just installs DefaultPolicy() and returns).
//
// nil logger = slog.Default().
//
// The returned *Store is always non-nil and immediately safe to
// Set() / Get() / ApplyShadowState() against. Callers wire it into
// their shadow dispatcher (agent) or pass it into request-handler
// deps (cp / ai-gateway).
func BootStore(ctx context.Context, load RawConfigLoader, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	s := NewStore(DefaultPolicy())
	if load == nil {
		return s
	}
	raw, err := load(ctx)
	if err != nil {
		logger.Warn("streaming policy initial load failed; using defaults",
			"error", err,
		)
		return s
	}
	if len(raw) == 0 {
		// Missing admin config — keep the conservative DefaultPolicy
		// baseline. The first Hub shadow push of the
		// streaming_compliance key will install the admin value via
		// ApplyShadowState.
		return s
	}
	policy, err := DecodeGlobalPolicy(raw)
	if err != nil {
		logger.Warn("streaming policy decode failed; using defaults",
			"error", err,
		)
		return s
	}
	s.Set(policy)
	logger.Info("streaming policy loaded",
		"mode", string(policy.Mode),
		"failBehavior", string(policy.FailBehavior),
		"chunkBytes", policy.ChunkBytes,
		"maxBufferBytes", policy.MaxBufferBytes,
	)
	return s
}
