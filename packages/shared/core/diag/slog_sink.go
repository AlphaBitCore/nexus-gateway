package diag

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// TraceIDAttrKey is the canonical slog-attr key that the SlogSink lifts
// into DiagEvent.TraceID (and the Hub diag writer persists into the
// thing_diag_event.trace_id column). Producers must stamp their request-
// scoped logger with this exact key, e.g.:
//
//	logger = logger.With(diag.TraceIDAttrKey, traceID)
//
// The value MUST be a string; non-string values fall through into the
// loose Attrs map so the operator can still see the malformed log line
// but the typed column stays empty for that row.
const TraceIDAttrKey = "trace_id"

// ThingClientPusher is the subset of *thingclient.Client that SlogSink
// uses to ship diagnostic events to Hub. The real implementation is
// satisfied by *thingclient.Client.PushDiagEvent. Tests inject a fake.
type ThingClientPusher interface {
	PushDiagEvent(ctx context.Context, evt opsmetrics.DiagEvent) error
}

// LocalBufferInserter is the subset of a crash buffer that SlogSink uses
// to persist FATAL events durably so a process crash before the next WS
// flush still preserves the event for backfill. Optional — when nil, FATAL
// still pushes via ThingClientPusher. For the agent, this is satisfied by
// *LocalBuffer (SQLCipher-backed); other services may pass nil.
type LocalBufferInserter interface {
	Insert(evt opsmetrics.DiagEvent) error
}

// SlogSinkConfig configures a SlogSink.
//
// Level is the slog level threshold; defaults to slog.LevelError.
// Source defaults to "service" — subsystems can wrap the sink in their own
// slog.Logger with a "source" attribute to override per-call.
// IncludeInfo defaults to false (spec §3 Decision #6); flip to true only
// in diagnostic mode when the operator wants the firehose.
//
// ReconnectBuffer + IsWSConnected together implement spec §7.4 reconnect
// behaviour for non-fatal events: when the WebSocket is up, events flow
// directly through ThingClient.PushDiagEvent; when it's down, events are
// queued in the in-process ring buffer and flushed on reconnect by the
// caller. FATAL events ALWAYS also hit LocalBuffer (when non-nil) because
// crash buffering is at-least-once per spec §7.4.
type SlogSinkConfig struct {
	ThingClient     ThingClientPusher
	LocalBuffer     LocalBufferInserter
	Dedup           *opsmetrics.Dedup
	ReconnectBuffer *ReconnectBuffer
	IsWSConnected   func() bool
	ThingID         string
	Source          string
	IncludeInfo     bool
	Level           slog.Level

	// OpsReg, when non-nil and Dedup is nil, makes NewSlogSink auto-construct
	// an opsmetrics.Dedup with sensible defaults (60s window, 100 active
	// keys) and register the `diag.dedup_collapsed_total{thing_type, severity}`
	// counter so emit-storms collapse uniformly across every service. Pass
	// your own Dedup via the field above to override; leave OpsReg nil to
	// disable dedup at this sink entirely. The `thing_type` label is pinned
	// to Source so per-service contribution stays separable in the Prometheus
	// view.
	OpsReg *opsmetrics.Registry
}

// SlogSink is a slog.Handler that captures records at level >= Level and
// forwards them as opsmetrics.DiagEvent envelopes through Dedup (when
// configured) and ThingClient. FATAL records are also persisted via
// LocalBuffer when one is wired.
//
// withAttrs is the slice of attrs accumulated via slog.Logger.With()
// across the handler-chain returned by WithAttrs. They are prepended onto
// every record at Handle time so a request-scoped logger built via
// `logger.With("trace_id", id)` actually carries the trace_id on every
// downstream call — load-bearing for the DiagEvent.TraceID auto-extract
// contract.
//
// parent points back at the original sink for WithAttrs-clones so the
// emit-serialization mutex + dedup/route helpers stay process-wide. The
// root sink has parent=nil and uses its own mu.
type SlogSink struct {
	cfg       SlogSinkConfig
	mu        sync.Mutex
	withAttrs []slog.Attr
	parent    *SlogSink
}

// NewSlogSink applies defaults (Level=ERROR, Source="service") and returns
// a sink ready to be wrapped in slog.New.
func NewSlogSink(cfg SlogSinkConfig) *SlogSink {
	if cfg.Level == 0 {
		cfg.Level = slog.LevelError
	}
	if cfg.Source == "" {
		cfg.Source = "service"
	}
	// Auto-wire Dedup + collapsed counter when an opsmetrics registry is
	// supplied and the caller didn't already construct its own Dedup. This
	// is the default path for every service — see the OpsReg field doc on
	// SlogSinkConfig. Window + max-active are the same values the agent
	// has run with: 60s collapse window, 100 distinct active message
	// hashes (above the cap, oldest is evicted).
	if cfg.Dedup == nil && cfg.OpsReg != nil {
		cfg.Dedup = opsmetrics.NewDedup(time.Now, 60*time.Second, 100)
		collapsed := cfg.OpsReg.NewCounter("diag.dedup_collapsed_total", []string{"thing_type", "severity"})
		cfg.Dedup.SetCollapsedCounter(collapsed, cfg.Source)
	}
	return &SlogSink{cfg: cfg}
}

// Enabled gates the sink on the configured level so the slog runtime can
// short-circuit before formatting attrs.
func (s *SlogSink) Enabled(_ context.Context, level slog.Level) bool {
	return level >= s.cfg.Level
}

// Handle is invoked by slog for every record that passed Enabled. It maps
// the slog level to the opsmetrics level vocabulary, builds a DiagEvent
// with messageHash = md5(level|source|message), runs it through Dedup
// (when configured), and ships the result(s) via ThingClient + LocalBuffer.
func (s *SlogSink) Handle(ctx context.Context, r slog.Record) error {
	level := mapLevel(r.Level)
	if level == opsmetrics.LevelInfo && !s.cfg.IncludeInfo {
		return nil
	}

	// Walk both the chain-of-With attrs (accumulated via WithAttrs across
	// every slog.Logger.With(...) call up to this point) and the on-record
	// attrs. Pull the typed trace_id out into the DiagEvent's first-class
	// field so downstream consumers (Hub thing_diag_event.trace_id column
	// + btree index) can query by trace without unpacking the JSONB Attrs
	// map. The remaining attrs flow into the loose Attrs map exactly as
	// before. The trace_id key is consumed — it does NOT appear duplicated
	// in Attrs — so JSON payloads stay minimal and the typed column is
	// the single source of truth.
	//
	// On-record attrs override With-chain attrs on the same key, matching
	// slog's standard semantics ("most specific wins").
	attrs := map[string]any{}
	var traceID string
	absorb := func(a slog.Attr) {
		if a.Key == TraceIDAttrKey {
			if v, ok := a.Value.Any().(string); ok {
				traceID = v
				return
			}
			// Defensive: a non-string trace_id (e.g. logged as int) still
			// flows into Attrs so the operator can see the malformed value.
			attrs[a.Key] = a.Value.Any()
			return
		}
		attrs[a.Key] = a.Value.Any()
	}
	for _, a := range s.withAttrs {
		absorb(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		absorb(a)
		return true
	})

	hash := md5.Sum([]byte(level + "|" + s.cfg.Source + "|" + r.Message))
	evt := opsmetrics.DiagEvent{
		ThingID:     s.cfg.ThingID,
		OccurredAt:  r.Time,
		Level:       level,
		EventType:   opsmetrics.EventTypeError,
		Source:      s.cfg.Source,
		Message:     r.Message,
		MessageHash: hex.EncodeToString(hash[:]),
		TraceID:     traceID,
		Attrs:       attrs,
		RepeatCount: 1,
	}

	// Serialize Submit/emit through the root sink so concurrent goroutines
	// — including those that obtained a With-clone — agree on first-
	// occurrence ordering. Dedup itself is internally locked, but the
	// LocalBuffer + ThingClient calls below are not.
	root := s.root()
	root.mu.Lock()
	defer root.mu.Unlock()

	emit := []opsmetrics.DiagEvent{evt}
	if root.cfg.Dedup != nil {
		emit = root.cfg.Dedup.Submit(evt)
	}

	for _, e := range emit {
		if e.Level == opsmetrics.LevelFatal && root.cfg.LocalBuffer != nil {
			// Best-effort: a buffer write failure must not block the WS
			// push (which is itself best-effort). FATAL is at-least-once
			// per spec §7.4 — always persist locally regardless of WS
			// state.
			_ = root.cfg.LocalBuffer.Insert(e)
		}
		root.routeLocked(ctx, e)
	}
	return nil
}

// routeLocked decides whether to push directly via ThingClient or buffer in
// the reconnect ring. Per spec §7.4 non-fatal events go to the in-process
// ring while the WS is down; fatal events also queue in the ring (because
// the LocalBuffer drain runs only at startup). Caller must hold s.mu.
func (s *SlogSink) routeLocked(ctx context.Context, e opsmetrics.DiagEvent) {
	connected := true
	if s.cfg.IsWSConnected != nil {
		connected = s.cfg.IsWSConnected()
	}
	if connected && s.cfg.ThingClient != nil {
		// Direct push. Failures are silent — the next reconnect is the
		// recovery point; we don't double-buffer on transient send errors
		// to avoid log-storming the buffer when the outbox is briefly
		// stalled.
		_ = s.cfg.ThingClient.PushDiagEvent(ctx, e)
		return
	}
	// Disconnected (or no client wired). Buffer for the next reconnect.
	if s.cfg.ReconnectBuffer != nil {
		s.cfg.ReconnectBuffer.Add(e)
		return
	}
	// No buffer wired and no client — best-effort drop. Tests exercise
	// this path explicitly via mock pushers; production always wires at
	// least one of the two.
	if s.cfg.ThingClient != nil {
		_ = s.cfg.ThingClient.PushDiagEvent(ctx, e)
	}
}

// WithAttrs returns a clone of this sink that carries the given attrs on
// every subsequent record. This is what makes `logger.With("trace_id", id)`
// reach every downstream log line — load-bearing for the DiagEvent.TraceID
// auto-extract contract.
//
// The clone keeps the same cfg and resolves emit-serialization through
// the root sink's mutex (via parent), so dedup ordering and per-process
// route invariants stay intact across With-chains.
func (s *SlogSink) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return s
	}
	root := s.root()
	clone := &SlogSink{cfg: root.cfg, parent: root}
	clone.withAttrs = make([]slog.Attr, 0, len(s.withAttrs)+len(attrs))
	clone.withAttrs = append(clone.withAttrs, s.withAttrs...)
	clone.withAttrs = append(clone.withAttrs, attrs...)
	return clone
}

// root walks parent pointers back to the original sink. The root is the
// owner of mu, Dedup state, and the lifecycle of LocalBuffer/ReconnectBuffer.
func (s *SlogSink) root() *SlogSink {
	r := s
	for r.parent != nil {
		r = r.parent
	}
	return r
}

// WithGroup is likewise a no-op: DiagEvent.Attrs is a flat map and group
// scoping is handled by callers via the "source" attribute.
func (s *SlogSink) WithGroup(_ string) slog.Handler { return s }

// mapLevel converts a slog.Level to the opsmetrics level vocabulary.
// Conventions:
//   - >= ERROR+4 → fatal (callers using slog.LevelError+4 signal "process
//     going down")
//   - >= ERROR  → error
//   - >= WARN   → warn
//   - else       → info (suppressed by default; see SlogSinkConfig.IncludeInfo)
func mapLevel(l slog.Level) string {
	switch {
	case l >= slog.LevelError+4:
		return opsmetrics.LevelFatal
	case l >= slog.LevelError:
		return opsmetrics.LevelError
	case l >= slog.LevelWarn:
		return opsmetrics.LevelWarn
	default:
		return opsmetrics.LevelInfo
	}
}
