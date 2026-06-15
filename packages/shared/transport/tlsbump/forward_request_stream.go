package tlsbump

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// isStreamingRequestBody reports whether the request body must be relayed to
// the upstream as a live stream rather than buffered to EOF first.
//
// The discriminator is ContentLength < 0 (unknown length) on a body-bearing
// method. A client that declares Content-Length: N commits to sending exactly N
// bytes, so buffering to EOF always terminates. An unknown-length body is the
// superset that includes the deadlock case: a connect-RPC / gRPC bidi call
// (e.g. Cursor's /agent.v1.AgentService/Run) opens its request stream and holds
// it open, sending more only after it reads server responses. Buffering such a
// body to EOF deadlocks — we wait for the request to end while the client waits
// for the response we will not forward until the request ends — so all
// unknown-length bodies stream (a chunked unary body streams harmlessly too;
// the only cost is it skips in-flight request-hook blocking, an acceptable
// fail-open on the agent host path).
//
// GET/HEAD/DELETE and any request whose ContentLength is 0 or known carry no
// deadlock risk and take the buffered compliance path unchanged.
func isStreamingRequestBody(r *http.Request) bool {
	if r.Body == nil || r.ContentLength >= 0 {
		return false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// boundedCapture is an io.Writer that accumulates up to max bytes and silently
// drops the overflow, so tee-ing an unbounded streaming request body into it
// can never OOM the proxy. It is concurrency-safe because the upstream
// transport writes to it (while reading the request body during RoundTrip) on a
// different goroutine than the post-upstream audit emit that reads Bytes().
type boundedCapture struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func newBoundedCapture(max int64) *boundedCapture {
	return &boundedCapture{max: int(max)}
}

// Write always reports the full length as written (never a short write) so the
// io.TeeReader driving it never fails the underlying body read; bytes beyond
// the cap are dropped and the capture is flagged truncated.
func (c *boundedCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	rem := c.max - len(c.buf)
	if rem > 0 {
		take := p
		if len(take) > rem {
			take = take[:rem]
		}
		c.buf = append(c.buf, take...)
	}
	if len(p) > rem {
		c.truncated = true
	}
	c.mu.Unlock()
	return len(p), nil
}

// Bytes returns a copy of the bytes captured so far.
func (c *boundedCapture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf...)
}

// Truncated reports whether the captured body was cut off at the cap (the
// stored audit copy is a partial-by-cap prefix, not the whole request).
func (c *boundedCapture) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// teeReadCloser tees every read of the wrapped body into the capture writer and
// closes the original body on Close. onClose, when set, runs once on Close
// (before the underlying close) — used to surface a truncated capture once the
// body is fully drained.
type teeReadCloser struct {
	r       io.Reader
	c       io.Closer
	onClose func()
}

func (t *teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *teeReadCloser) Close() error {
	if t.onClose != nil {
		t.onClose()
		t.onClose = nil
	}
	return t.c.Close()
}

// runStreamingRequestPhase handles a streaming (unknown-length) request body:
// it forwards the body to the upstream as a live stream — never buffering to
// EOF — while teeing a bounded copy into an audit capture. This is the
// deadlock-free path for connect-RPC / gRPC bidi calls.
//
// The request-stage compliance hook pipeline is intentionally NOT run here: a
// streaming body is not available in full before it must be forwarded, so it
// cannot be pre-inspected or blocked in flight. The flow is therefore
// audit-only (the captured request bytes + the response-stage capture still
// land on the traffic_event for visibility). The response side keeps its full
// streaming-aware handling (isStreamingContentType → handleSSEResponse).
//
// Always returns false: a streaming request is never answered here, it is
// always forwarded upstream.
func (x *bumpedExchange) runStreamingRequestPhase() bool {
	bo, logger := x.flow.bo, x.flow.logger

	maxBytes := x.pcCfg.MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = payloadcapture.DefaultMaxRequestBytes
	}
	capture := newBoundedCapture(maxBytes)

	// Tee the live body into the bounded capture; upstream reads drive both.
	orig := x.r.Body
	x.r.Body = &teeReadCloser{
		r: io.TeeReader(orig, capture),
		c: orig,
		onClose: func() {
			if capture.Truncated() {
				logger.Warn("streaming request: captured body truncated at cap (audit copy is a partial prefix)",
					"target", x.flow.targetHost,
					"path", x.r.URL.Path,
					"transactionId", x.txID,
					"capture_cap", maxBytes,
				)
			}
		},
	}
	// ContentLength stays -1 so the upstream transport streams the body
	// (chunked / h2 DATA frames) instead of waiting to learn a fixed length.

	sanitisedHeaders := copyHeadersStrippingAuth(x.r.Header)

	// Resolve the adapter (for response-side normalize + provider attribution)
	// but do NOT normalize the request body — it is not available yet.
	var resolvedAdapter traffic.Adapter
	var reqMeta traffic.RequestMeta
	if bo.adapterRegistry != nil && x.matchedDomain != nil && x.matchedDomain.AdapterID != "" {
		if factory := bo.adapterRegistry.Get(x.matchedDomain.AdapterID); factory != nil {
			resolvedAdapter = factory()
			reqMeta = resolvedAdapter.DetectRequestMeta(x.r, nil)
		}
	}

	if connSetupMs := int(time.Since(x.connSetupStart).Milliseconds()); connSetupMs > 0 {
		x.phaseBreakdown["conn_setup_ms"] = connSetupMs
	}

	reqInput := &core.HookInput{
		Stage:             "request",
		SourceIP:          bo.sourceIP,
		TargetHost:        x.flow.targetHost,
		Method:            x.r.Method,
		Path:              x.r.URL.Path,
		IngressType:       "COMPLIANCE_PROXY",
		ContentType:       x.r.Header.Get("Content-Type"),
		DetectedProvider:  reqMeta.Provider,
		DetectedModel:     reqMeta.Model,
		ApiKeyClass:       reqMeta.ApiKeyClass,
		ApiKeyFingerprint: reqMeta.ApiKeyFingerprint,
		EndpointType:      x.endpointType,
	}
	auditInfo := compliance.AuditInfo{
		TransactionID:       x.txID,
		ConnectionID:        bo.connectionID,
		TraceID:             x.traceID,
		Headers:             sanitisedHeaders,
		RequestMeta:         reqMeta,
		PhaseSink:           x.phaseSink,
		LatencyBreakdown:    x.phaseBreakdown,
		DomainRuleID:        x.domainRuleID,
		PathAction:          string(x.resolvedPathAction),
		SourceProcess:       bo.procName,
		SourceProcessBundle: bo.procBundle,
		SourceUser:          bo.procUser,
	}

	logger.Info("streaming request: relaying body without buffering",
		"target", x.flow.targetHost,
		"path", x.r.URL.Path,
		"content_type", x.r.Header.Get("Content-Type"),
		"transactionId", x.txID,
		"capture_cap", maxBytes,
	)

	// Record an explicit APPROVE request decision. The streaming path forwards
	// without running the blocking hook pipeline (the body is not buffered), so
	// "approved, not blocked" is the accurate decision — and, load-bearing, it
	// makes the audit classifier treat this captured flow as Processed rather
	// than Inspect. At the default trafficUploadLevel="processed" an Inspect row
	// is NOT uploaded to Hub, so without this every streaming (bidi /
	// unknown-length) AI capture would land only in the local audit DB and never
	// reach the central store. stampCPMarker also reads x.reqHookResult.
	x.reqHookResult = &core.CompliancePipelineResult{Decision: compliance.Approve}

	x.r = x.r.WithContext(context.WithValue(x.r.Context(), requestAuditKey{}, &requestAuditCtx{
		input:                 reqInput,
		info:                  auditInfo,
		requestCapture:        capture,
		storeRequestBody:      x.pcCfg.StoreRequestBody,
		storeResponseBody:     x.pcCfg.StoreResponseBody,
		requestPipelineResult: x.reqHookResult,
		matchedDomain:         x.matchedDomain,
		adapter:               resolvedAdapter,
	}))
	return false
}

// normalizeStreamingRequest runs the request-side adapter normalize against the
// tee-captured streaming request body, post-upstream, once the body has been
// drained by the upstream forward. The streaming path cannot normalize before
// forwarding (the body is not buffered yet), so without this a structured
// request — e.g. an OpenAI Responses body on a chunked/unknown-length POST —
// would land on the traffic_event as tier3 generic-http even though its adapter
// parses it cleanly. For a request that completes before its response (the
// common stream:true chat case) the capture is whole here; for a true bidi
// stream it is the first-message prefix, still better than http-binary.
//
// No-op on the buffered path (requestCapture nil, RequestNormalized already
// set) and when no adapter matched or the capture is empty.
func (x *bumpedExchange) normalizeStreamingRequest(audCtx *requestAuditCtx) {
	if audCtx == nil || audCtx.requestCapture == nil || audCtx.adapter == nil {
		return
	}
	if len(audCtx.info.RequestNormalized) > 0 {
		return
	}
	body := audCtx.requestCapture.Bytes()
	if len(body) == 0 {
		return
	}
	bo, logger := x.flow.bo, x.flow.logger
	content := runtimeNormalize(context.Background(), bo.normalizeRegistry, audCtx.adapter, body,
		audCtx.input.Path, audCtx.input.ContentType, normalize.DirectionRequest, logger, x.txID)
	if content != nil {
		if b, err := json.Marshal(content); err == nil {
			audCtx.info.RequestNormalized = b
		}
	}
	// The body is available now, so provider/model detection can be refined
	// beyond the header-only guess made when the audit ctx was built.
	meta := audCtx.adapter.DetectRequestMeta(x.r, body)
	if meta.Provider != "" {
		audCtx.input.DetectedProvider = meta.Provider
	}
	if meta.Model != "" {
		audCtx.input.DetectedModel = meta.Model
	}
	audCtx.info.RequestMeta = meta
}
