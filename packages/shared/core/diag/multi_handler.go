// Package diag provides diagnostic-event producers and the slog fan-out
// handler used by all gateway services. SlogSink captures ERROR+ slog records
// and ships them as DiagEvent envelopes via thingclient. ReconnectBuffer
// queues events while the WebSocket is disconnected. MultiHandler fans out
// a single slog record to multiple underlying handlers. Recover is a deferred
// panic handler that captures crash events before re-panicking.
package diag

import (
	"context"
	"log/slog"
)

// MultiHandler fans out a slog.Record to every wrapped slog.Handler. It
// keeps the existing JSON/text handler (stdout + log file) writing exactly
// as before while ALSO routing ERROR+ records into a SlogSink that emits
// them as DiagEvents to Hub. Order matters only insofar as a downstream
// handler that mutates Record.Attrs (slog records pass attrs by reference)
// could leak into the next; today neither handler does.
//
// MultiHandler.Enabled returns true when ANY underlying handler is enabled
// for the level — slog short-circuits the per-Handle dispatch otherwise, so
// keeping the gate permissive lets each handler apply its own threshold
// (the SlogSink suppresses below LevelError; the file/stdout handler keeps
// printing INFO and DEBUG as configured).
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler returns a MultiHandler that fans out to the supplied
// handlers. nil entries are filtered.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	out := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			out = append(out, h)
		}
	}
	return &MultiHandler{handlers: out}
}

// Enabled reports whether any underlying handler accepts the level.
func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle dispatches to every underlying handler whose Enabled returns true.
// Errors from one handler do not stop the dispatch — a single returned error
// (the first one) is enough for slog's contract; the rest are best-effort
// ignored.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WithAttrs propagates attrs to each underlying handler.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: out}
}

// WithGroup propagates the group to each underlying handler.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		out[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: out}
}
