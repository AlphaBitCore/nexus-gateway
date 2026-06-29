package tlsbump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/bodydecompress"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// readBody reads the request body up to maxBytes and closes the original
// body. Returns nil if the body is nil. maxBytes <= 0 falls back to the
// payload-capture default cap so an unset/invalid runtime config can
// never collapse the read to zero.
func readBody(r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = payloadcapture.DefaultMaxRequestBytes
	}
	limited := io.LimitReader(r.Body, maxBytes+1)
	bodyBytes, err := io.ReadAll(limited)
	_ = r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if int64(len(bodyBytes)) > maxBytes {
		bodyBytes = bodyBytes[:maxBytes]
	}
	return bodyBytes, nil
}

// readResponseBodyBounded reads up to maxBytes from the upstream response body
// and closes it, mirroring readBody's cap on the request side so a malicious or
// compromised upstream cannot OOM the proxy by returning an unbounded buffered
// response (the buffered AI / response-hook path holds the whole body in
// memory). maxBytes <= 0 falls back to the payload-capture default response cap
// so an unset/invalid runtime config can never collapse the read to zero.
// Overflow is truncated to maxBytes rather than erroring — the partial body is
// still useful for usage extraction / capture, and the upstream stream is
// abandoned along with the connection.
func readResponseBodyBounded(body io.ReadCloser, maxBytes int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = payloadcapture.DefaultMaxResponseBytes
	}
	out, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	_ = body.Close()
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(out)) > maxBytes {
		out = out[:maxBytes]
	}
	return out, nil
}

// decompressForCapture wraps the shared bodydecompress.Decompress.
// Supports gzip / deflate / br / zstd with idempotent fallback semantics and a
// bounded decompressed size (bodydecompress.DefaultMaxDecompressedBytes) so a
// br/zstd decompression bomb from a malicious upstream cannot OOM the proxy. On
// overflow the original (still-compressed) bytes are captured opaquely and a
// warning is logged.
func decompressForCapture(body []byte, resp *http.Response, logger *slog.Logger) []byte {
	out, truncated := bodydecompress.Decompress(body, resp, 0)
	if truncated && logger != nil {
		logger.Warn("decompressForCapture: response exceeded decompressed-size bound; capturing opaque bytes",
			"contentEncoding", resp.Header.Get("Content-Encoding"),
		)
	}
	return out
}

// contentBlocksToNormalized converts hook pipeline ModifiedContent into a
// traffic.NormalizedContent.Segments slice positioned to match the
// adapter's ExtractRequest output. Only text-type blocks contribute.
// Used by the cp inflight redact path when MODIFY decisions need to
// rewrite the upstream body via adapter.RewriteRequestBody.
func contentBlocksToNormalized(blocks []core.ContentBlock) traffic.NormalizedContent {
	segments := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			continue
		}
		segments = append(segments, b.Text)
	}
	return traffic.NormalizedContent{Segments: segments}
}

// rewriteContentWithToolArgs augments contentBlocksToNormalized with the
// compliance-masked tool-call arguments — applying the pipeline's
// TransformSpans to the canonical request payload and re-marshaling each
// ContentToolUse block's Input in block order (the wire tool_calls[] order).
// nil payload or no tool-use spans → ToolCallArgs nil (zero churn).
func rewriteContentWithToolArgs(blocks []core.ContentBlock, payload *normalize.NormalizedPayload, spans []normalize.TransformSpan) traffic.NormalizedContent {
	nc := contentBlocksToNormalized(blocks)
	if payload != nil {
		nc.ToolCallArgs = normalize.ToolCallArgsFromPayload(*payload, spans)
	}
	return nc
}

// copyHeadersStrippingAuth builds a copy of headers with auth-related keys removed.
// Deep-copies slices so hook mutations cannot corrupt the live request.
func copyHeadersStrippingAuth(src http.Header) map[string][]string {
	dst := make(map[string][]string, len(src))
	for key, values := range src {
		if isAuthHeader(key) {
			continue
		}
		vc := make([]string, len(values))
		copy(vc, values)
		dst[key] = vc
	}
	return dst
}

// runtimeNormalize wraps the normalize hot path: Registry (Tier
// 1+2+3 chain) first, then the adapter's ExtractRequest/Response
// segment extraction as the PII fallback.
//
// When reg is non-nil, runtimeNormalize calls reg.Normalize which runs
// the full Tier 1+2+3 fallback, matching Hub agent_audit's BuildAuditFn.
// The Registry is the ONLY wire-format decode route — there is no
// adapter-direct Normalize dispatch; per-host adapters participate by
// registering into the same Registry at startup.
//
// Returns nil when neither path produces a normalized payload — the
// caller (hook input builder) treats nil Normalized as "any kind" so
// hooks scoped to AI traffic still run when content extraction is
// unavailable.
func runtimeNormalize(
	ctx context.Context,
	reg *normalize.Registry,
	adapter traffic.Adapter,
	body []byte,
	path, contentType string,
	direction normalize.Direction,
	logger *slog.Logger,
	transactionID string,
) (out *normalize.NormalizedPayload) {
	// When normalize falls through to nil, emit a debug line recording
	// adapter / direction / body signature so future debugging can
	// bisect (compressed body? wrong adapter ID? spec out of date?)
	// without re-instrumenting code.
	defer func() {
		if out == nil {
			adapterID := ""
			if adapter != nil {
				adapterID = adapter.ID()
			}
			previewLen := 80
			if len(body) < previewLen {
				previewLen = len(body)
			}
			logger.Debug("runtimeNormalize: no payload",
				"adapter", adapterID,
				"direction", direction,
				"contentType", contentType,
				"path", path,
				"bodySize", len(body),
				"bodyPreview", string(body[:previewLen]),
				"transactionId", transactionID,
			)
		}
	}()
	if len(body) == 0 {
		return nil
	}
	// Meta normalization — aligned with Hub BuildAuditFn so the
	// same raw bytes produce the same lookup key on both sides.
	// (1) ContentType: strip "; charset=utf-8" etc. — Registry routes by
	//     bare media type ("text/event-stream", not "text/event-stream;
	//     charset=utf-8"); leaving the params on causes a silent
	//     ErrUnsupported and an empty NormalizedPayload.
	// (2) Stream: derive from Content-Type — SSE codec for chatgpt-web /
	//     anthropic / openai responses only routes when Stream=true.
	//     Hub's BuildAuditFn receives stream from the agent envelope;
	//     here we infer it from the wire bytes.
	// (3) AdapterType: lowercase — Registry keys are lowercase.
	bareCT := normalize.StripContentTypeParams(contentType)
	stream := direction == normalize.DirectionResponse && strings.HasPrefix(bareCT, "text/event-stream")
	// Preferred path: Registry Tier 1+2+3 chain. adapterType comes from
	// the resolved adapter so reg.Normalize routes to the right Tier 1
	// spec; empty adapter (no per-host match) still flows through Tier 2
	// pattern probe + Tier 3 verbatim fallback.
	if reg != nil {
		adapterType := ""
		if adapter != nil {
			adapterType = strings.ToLower(adapter.ID())
		}
		// Body signature (first 200 bytes + top-level JSON keys) is the
		// single most useful "why didn't this tier claim" diagnostic, but it
		// echoes raw user prompt / model-output content. This is a
		// compliance/DLP product: emit it ONLY at Debug so it never lands in
		// the default (Info) prod logs, and compute it lazily so the hot path
		// pays nothing when Debug is off.
		if logger.Enabled(ctx, slog.LevelDebug) {
			logger.Debug("runtimeNormalize: Registry.Normalize ENTER",
				"adapter", adapterType,
				"direction", direction,
				"path", path,
				"contentType", bareCT,
				"stream", stream,
				"bodySize", len(body),
				"bodyPreview", previewBody(body, 200),
				"jsonTopLevelKeys", topLevelJSONKeys(body, 16),
				"transactionId", transactionID,
			)
		}
		payload, err := reg.Normalize(ctx, body, normalize.Meta{
			AdapterType:  adapterType,
			ContentType:  bareCT,
			Direction:    direction,
			EndpointPath: path,
			Stream:       stream,
		})
		if err == nil {
			// Distinguish which tier claimed: Tier 1 stamps Protocol =
			// adapter ID; Tier 2 PatternNormalizer stamps "pattern-
			// extract"; Tier 3 GenericHTTP stamps "generic-http".
			tier := "tier1"
			switch payload.Protocol {
			case "pattern-extract":
				tier = "tier2"
			case "generic-http":
				tier = "tier3"
			}
			logger.Info("runtimeNormalize: Registry.Normalize CLAIM",
				"tier", tier,
				"adapter", adapterType,
				"direction", direction,
				"protocol", payload.Protocol,
				"detectedSpec", payload.DetectedSpec,
				"kind", payload.Kind,
				"confidence", payload.Confidence,
				"transactionId", transactionID,
			)
			return &payload
		}
		// On Registry-side fall-through or hard errors, the adapter's
		// segment extraction below still runs — partial coverage is
		// better than no coverage.
		if errors.Is(err, normalize.ErrUnsupported) {
			logger.Info("runtimeNormalize: Registry.Normalize FELL-THROUGH (no tier above threshold)",
				"adapter", adapterType,
				"direction", direction,
				"transactionId", transactionID,
			)
		} else {
			logger.Warn("Registry.Normalize failed",
				"adapter", adapterType,
				"direction", direction,
				"transactionId", transactionID,
				"error", err,
			)
		}
	}
	if adapter == nil {
		return nil
	}
	// PII-extraction fallback: ExtractRequest/Response → Segments.
	// This is a different concern from wire-format decoding — the
	// adapter's per-host extractors recover hookable text segments
	// even when no decoder claimed the body shape.
	var nc traffic.NormalizedContent
	var err error
	if direction == normalize.DirectionResponse {
		nc, err = adapter.ExtractResponse(ctx, body, path)
	} else {
		nc, err = adapter.ExtractRequest(ctx, body, path)
	}
	if err != nil {
		if !errors.Is(err, traffic.ErrUnknownSchema) {
			logger.Warn("adapter content extraction failed",
				"adapter", adapter.ID(),
				"direction", direction,
				"transactionId", transactionID,
				"error", err,
			)
		}
		return nil
	}
	return core.PayloadFromTextSegments(nc.Segments)
}

// previewBody returns the first max bytes as a printable string with
// non-ASCII bytes elided. Used in runtimeNormalize diagnostic logs so
// operators can eyeball the body shape without re-dumping the spilled
// JSONB.
func previewBody(body []byte, max int) string {
	if len(body) == 0 {
		return ""
	}
	n := max
	if n > len(body) {
		n = len(body)
	}
	out := make([]byte, 0, n)
	for _, b := range body[:n] {
		switch {
		case b >= 0x20 && b < 0x7f:
			out = append(out, b)
		case b == '\n':
			out = append(out, '\\', 'n')
		case b == '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, '.')
		}
	}
	if len(body) > max {
		out = append(out, '.', '.', '.')
	}
	return string(out)
}

// topLevelJSONKeys parses raw as JSON and returns up to max top-level
// keys. The single most useful signal for "why didn't claude-web spec
// match" — the spec's SignatureFields list (parent_message_uuid,
// timezone, locale, …) can be eyeballed against the body's actual
// top-level key set in one log line. Returns nil for non-JSON / array
// roots / parse failures.
func topLevelJSONKeys(body []byte, max int) []string {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil
	}
	keys := make([]string, 0, max)
	root.ForEach(func(k, _ gjson.Result) bool {
		if len(keys) >= max {
			return false
		}
		keys = append(keys, k.String())
		return true
	})
	return keys
}

// attestationPassthrough handles the "agent-attested → no inspection"
// branch. Invoked when buildForwardHandler's attestation peek accepts
// the inner request. The contract is pure relay: forward the request
// upstream verbatim (sans hop-by-hop headers) + stream the response
// back to the client. NO hook pipelines run, NO audit row is emitted
// (the agent's own audit row is the system-of-record), NO payload
// capture, NO response markers stamped (would imply CP inspected it).
//
// The X-Nexus-Attestation header itself is stripped before forwarding
// to upstream — no benefit in leaking the signature to the provider,
// and it would surface as an unknown header in their access logs.
func attestationPassthrough(
	w http.ResponseWriter,
	r *http.Request,
	upstream *UpstreamTransport,
	logger *slog.Logger,
) {
	// Strip the attestation header from the outbound request — the
	// upstream provider has no use for it and including it would leak
	// agent identity into third-party access logs.
	if r.Header.Get(AttestationHeaderName) != "" {
		r.Header.Del(AttestationHeaderName)
	}

	resp, err := upstream.ForwardRequest(r.Context(), r) //nolint:bodyclose // copyResponse defers Body.Close()
	if err != nil {
		logger.Error("attestation passthrough: upstream forward failed",
			"target", r.URL.Host,
			"error", err,
		)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	if err := copyResponse(w, resp, nil); err != nil {
		logger.Warn("attestation passthrough: copy response failed",
			"target", r.URL.Host,
			"error", err,
		)
	}
}
