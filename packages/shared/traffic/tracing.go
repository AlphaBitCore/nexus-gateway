package traffic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

// PhaseSink collects per-request upstream timing for one HTTP roundtrip
// plus an optional long-tail phase breakdown stamped by deeper layers
// (e.g. the spec_adapter codec) where the request handler's PhaseTimer
// isn't reachable. Producers (the TracingRoundTripper) write `ttfbMs`
// from httptrace.ClientTrace.GotFirstResponseByte and `totalMs` from a
// response-body Close hook. Long-tail consumers call Breakdown(phase, ms)
// to add a named entry; the handler reads Breakdown() at finalize time
// and merges into the audit Record's latency_breakdown JSONB column.
//
// A PhaseSink is single-use — one per upstream request. Concurrent reads
// from multiple goroutines are safe; writes are serialised on the upstream
// roundtrip goroutine plus, for Breakdown(), any codec goroutine that
// runs after RoundTrip returns.
type PhaseSink struct {
	sendStart time.Time
	ttfbMs    atomic.Int64 // 0 until the first body byte arrives
	totalMs   atomic.Int64 // 0 until the response body is closed
	mu        sync.Mutex
	extras    map[string]int
}

// NewPhaseSink returns a fresh PhaseSink. The sendStart is set by the
// TracingRoundTripper just before delegating to the underlying transport;
// callers do not need to populate any field manually.
func NewPhaseSink() *PhaseSink {
	return &PhaseSink{}
}

// TtfbMs returns the captured TTFB in milliseconds, or nil if no first
// response byte has been observed yet (likely an upstream error before
// any body was sent). The pointer return matches the audit Record field
// shape so callers can assign directly without an intermediate variable.
func (ps *PhaseSink) TtfbMs() *int {
	if ps == nil {
		return nil
	}
	v := ps.ttfbMs.Load()
	if v <= 0 {
		return nil
	}
	out := int(v)
	return &out
}

// TotalMs returns the captured upstream-total in milliseconds (from send
// to response-body close), or nil if the body has not yet been closed.
// Streaming consumers must Close the response body for this to populate.
func (ps *PhaseSink) TotalMs() *int {
	if ps == nil {
		return nil
	}
	v := ps.totalMs.Load()
	if v <= 0 {
		return nil
	}
	out := int(v)
	return &out
}

// AddBreakdown stamps a named phase entry onto the sink. Used by codec /
// adapter layers reached after the upstream roundtrip to attribute their
// time without threading a PhaseTimer through every call. Zero / negative
// values are dropped. Multiple calls with the same key accumulate.
func (ps *PhaseSink) AddBreakdown(name string, ms int) {
	if ps == nil || name == "" || ms <= 0 {
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.extras == nil {
		ps.extras = make(map[string]int, 4)
	}
	ps.extras[name] += ms
}

// Breakdown returns a copy of the stamped extras. nil when no producer
// called AddBreakdown. Safe to call concurrently with further AddBreakdown
// calls — returned map is independent.
func (ps *PhaseSink) Breakdown() map[string]int {
	if ps == nil {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if len(ps.extras) == 0 {
		return nil
	}
	out := make(map[string]int, len(ps.extras))
	for k, v := range ps.extras {
		out[k] = v
	}
	return out
}

type phaseSinkCtxKey struct{}

// WithPhaseSink attaches a PhaseSink to ctx so the TracingRoundTripper
// can locate and populate it. The returned context inherits all values
// + cancellation from `parent`.
func WithPhaseSink(parent context.Context, ps *PhaseSink) context.Context {
	if ps == nil {
		return parent
	}
	return context.WithValue(parent, phaseSinkCtxKey{}, ps)
}

// PhaseSinkFromContext retrieves the sink attached by WithPhaseSink, or nil
// if no sink is on the context. Callers can pass any context — including
// background — without a guard.
func PhaseSinkFromContext(ctx context.Context) *PhaseSink {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(phaseSinkCtxKey{}).(*PhaseSink)
	return v
}

// NewTracingTransport wraps base so that every roundtrip whose request
// context carries a PhaseSink populates the sink's ttfbMs and totalMs.
// Requests without a sink pass through untouched (zero allocations beyond
// the existing trace machinery).
//
// Wrap the upstream Transport once at construction; the resulting
// http.RoundTripper is goroutine-safe and reusable across requests.
func NewTracingTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &tracingTransport{base: base}
}

type tracingTransport struct {
	base http.RoundTripper
}

func (t *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ps := PhaseSinkFromContext(req.Context())
	if ps == nil {
		return t.base.RoundTrip(req)
	}
	ps.sendStart = time.Now()
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			ms := time.Since(ps.sendStart).Milliseconds()
			if ms <= 0 {
				ms = 1
			}
			ps.ttfbMs.Store(ms)
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		ms := time.Since(ps.sendStart).Milliseconds()
		if ms <= 0 {
			ms = 1
		}
		ps.totalMs.Store(ms)
		return resp, err
	}
	if resp != nil && resp.Body != nil {
		sendStart := ps.sendStart
		stamp := func() {
			ms := time.Since(sendStart).Milliseconds()
			if ms <= 0 {
				ms = 1
			}
			ps.totalMs.Store(ms)
		}
		resp.Body = &phaseTrackedBody{
			ReadCloser: resp.Body,
			stamp:      stamp,
		}
	}
	return resp, nil
}

// Unwrap exposes the wrapped RoundTripper so callers that walk the
// transport chain (e.g. relay.underlyingHTTPTransport) can reach the
// underlying *http.Transport for TLS config surgery without losing the
// tracing wrap.
func (t *tracingTransport) Unwrap() http.RoundTripper {
	return t.base
}

// phaseTrackedBody wraps an io.ReadCloser so the upstream-total instant
// is refreshed on EVERY successful Read (last-write-wins on the atomic),
// in addition to a once-only stamp at Close. Read-side stamping is
// required because the streaming broker pump runs Close() in its own
// goroutine (`defer session.Close()` in broker.pump) — the request
// handler reads PhaseSink.TotalMs() during its own defer, which can
// fire before the pump goroutine has reached its defer. Without
// Read-side stamping, the audit row sees totalMs=0 (and writes NULL)
// even though the upstream stream finished successfully. Non-streaming
// reads also benefit: totalMs ends up reflecting the last-byte time
// rather than the Body.Close call site, which is closer to "upstream
// done". Close keeps sync.Once semantics so repeat-close (idempotent
// callers like SSEScanner.Close + defer Close stacks) does not
// re-stamp with a later timestamp.
type phaseTrackedBody struct {
	io.ReadCloser
	stamp     func()
	closeOnce sync.Once
}

func (b *phaseTrackedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 || err != nil {
		b.stamp()
	}
	return n, err
}

func (b *phaseTrackedBody) Close() error {
	b.closeOnce.Do(b.stamp)
	return b.ReadCloser.Close()
}
