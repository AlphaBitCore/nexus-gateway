package proxy

import (
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// admissionGate bounds the number of in-flight proxy requests so overload
// degrades into fast, retryable 429s instead of unbounded in-heap queueing.
// Without it, an arrival rate above the box's service rate grows the
// in-flight set (goroutine stacks + request bodies + response buffers)
// without limit until the heap reaches GOMEMLIMIT and the collector's
// assist tax collapses throughput — or the kernel OOM-kills the process.
// A bounded in-flight count keeps that memory footprint bounded.
//
// The admitted path costs two atomic ops (one add on entry, one on exit).
// A shed request still traverses the outer middleware (RequestID, OTel
// span, Logger — which logs 4xx at Warn) but skips all per-request proxy
// work and gets no audit record: the shed path's observability is the
// admission_shed_total counter. Health, metrics, and admin endpoints are
// outside ServeProxy and therefore never gated — a saturated data plane
// must still answer its liveness probes.
type admissionGate struct {
	max      int64
	inflight atomic.Int64
}

// resolveAdmissionMax parses an AI_GATEWAY_MAX_INFLIGHT value:
//
//	unset / "auto" → 1024 × GOMAXPROCS. Scaled by core count so the cap
//	                 tracks the box; sized generously because an SSE stream
//	                 holds its slot for the stream's full duration, so a
//	                 tight cap would false-shed a healthy streaming-heavy
//	                 deployment. The bound exists to cap heap growth under
//	                 overload, not to throttle normal traffic.
//	0 / negative   → gate disabled (explicit operator choice)
//	garbage        → the auto default (a typo must never silently disable
//	                 overload protection); the caller logs the fallback
func resolveAdmissionMax(v string) (max int64, fellBack bool) {
	auto := int64(1024 * runtime.GOMAXPROCS(0))
	if v == "" || v == "auto" {
		return auto, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return auto, true
	}
	if n <= 0 {
		return 0, false // explicit disable
	}
	return n, false
}

var (
	admissionOnce sync.Once
	admissionMax  int64
)

// newAdmissionGate builds the gate from AI_GATEWAY_MAX_INFLIGHT (resolved
// once per process); nil when disabled.
func newAdmissionGate() *admissionGate {
	admissionOnce.Do(func() {
		v := os.Getenv("AI_GATEWAY_MAX_INFLIGHT")
		m, fellBack := resolveAdmissionMax(v)
		if fellBack {
			slog.Warn("AI_GATEWAY_MAX_INFLIGHT is not a number; using the auto default",
				"value", v, "max", m)
		}
		admissionMax = m
	})
	if admissionMax > 0 {
		return &admissionGate{max: admissionMax}
	}
	return nil
}

// acquire admits the request unless the in-flight cap is reached. The
// increment-then-check shape keeps the hot path a single atomic add; the
// decrement on rejection cannot strand capacity because every path that
// observes n > max gives its slot back before returning.
func (g *admissionGate) acquire() bool {
	if g.inflight.Add(1) > g.max {
		g.inflight.Add(-1)
		admissionShedTotal.Inc()
		return false
	}
	return true
}

// release returns the request's slot. Deferred by the ServeProxy wrapper so
// it covers every exit path, including streaming completions and panics
// unwound through the handler's defers before Recovery catches them.
func (g *admissionGate) release() { g.inflight.Add(-1) }

// admissionShedTotal counts requests rejected by the in-flight gate. The
// counter is the shed path's observability — rejected requests get no audit
// record on purpose (auditing a shed storm would itself be load).
var admissionShedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "nexus_ai_gateway_admission_shed_total",
	Help: "Requests rejected with 429 by the in-flight admission gate.",
})

// writeOverloaded writes the fast-reject response in the CALLER's ingress
// wire shape (same cross-ingress error contract as writeIngressError —
// anthropic /v1/messages gets {"type":"error",...}, gemini gets its
// envelope; OpenAI-family and unknown get the OpenAI error shape), plus
// Retry-After so generic clients back off. The shape is derived from the
// route's static BodyFormat: the gate runs before per-request state, so the
// x-nexus-aigw-body-format override (OpenAI-family niche) is not consulted.
func writeOverloaded(w http.ResponseWriter, ingress provcore.Format) {
	const msg = "gateway is at capacity, retry shortly"
	var body []byte
	if ingress != "" && !ingress.IsOpenAIFamily() {
		// CodeRateLimited maps to each envelope's semantically-correct type
		// (anthropic "rate_limit_error", gemini RESOURCE_EXHAUSTED) so SDK
		// classification matches the OpenAI-shape fallback below.
		body = envelope.EncodeErrorEnvelopeForIngress(ingress, ingress, &provcore.ProviderError{
			Status: http.StatusTooManyRequests, Code: provcore.CodeRateLimited, Message: msg,
		})
	} else {
		body = []byte(`{"error":{"message":"` + msg + `","type":"rate_limit_error","code":"gateway_overloaded"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}
