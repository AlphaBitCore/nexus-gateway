package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	openairesponses "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/responses"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
)

// rejectWithBespokeEnvelope emits one of the cross-format rejections below and
// records its machine-readable identity on the audit row.
//
// These four writers build their bodies through envelope.GatewayErrorBodyWith
// rather than going through writeIngressError, because each carries one field
// no other refusal supplies. Each used to set only StatusCode and
// HookReasonCode. Production therefore carried /v1/responses and
// /v1/audio/transcriptions 400s whose error_code AND error_reason were both
// null — rows nothing could group, alert on, or count, and whose only trace of
// what happened was a body nobody had stored. The body is stamped for the same
// reason writeIngressError stamps it: a gateway-generated error envelope carries
// no user content and is the most useful thing to see when a request fails.
//
// auditCode is SCREAMING_SNAKE to match error_code across the rest of the
// gateway, and the wire `code` inside the body is the same string. Two of these
// writers put no code on the wire at all while naming one on the row, and the
// other two spelled it differently there — so a caller could not branch on a
// failure the row could name.
func rejectWithBespokeEnvelope(w http.ResponseWriter, rec *audit.Record,
	status int, auditCode, reason string, body []byte) {
	rec.StatusCode = status
	rec.ErrorCode = auditCode
	rec.ErrorReason = reason
	rec.ResponseBody = body
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeNoCompatibleCapability emits a 400 error body when the capability
// pre-filter rejected every routing candidate for an embeddings request.
// The error body follows the OpenAI error envelope and includes an
// available_capabilities array so the client knows what each model supports.
func (h *Handler) writeNoCompatibleCapability(w http.ResponseWriter, rec *audit.Record, e *routingcore.NoCompatibleProviderError) {
	rec.HookReasonCode = "no_compatible_capability"
	const msg = "No routing candidate supports the requested embedding parameters. Check available_capabilities for supported values."
	body := envelope.GatewayErrorBodyWith(http.StatusBadRequest, "NO_COMPATIBLE_CAPABILITY", msg, "",
		map[string]any{"available_capabilities": e.Available})
	rejectWithBespokeEnvelope(w, rec, http.StatusBadRequest, "NO_COMPATIBLE_CAPABILITY", msg, body)
}

// SchemaMismatchRecorder records `ingress → provider` format mismatches
// that were rejected by [filterCompatibleTargets]. The production
// implementation increments the
// `schema_mismatch_total{ingress,provider}` opsmetrics counter; tests can
// leave it nil.
type SchemaMismatchRecorder interface {
	RecordSchemaMismatch(ingress, provider string)
}

// RejectedTarget captures a routing target that was filtered out by
// cross-format compatibility enforcement, together with the wire
// format the gateway detected for the upstream provider. The handler
// uses RejectedTarget.ProviderFormat to populate the error body and the
// mismatch metric.
type RejectedTarget struct {
	routingcore.RoutingTarget
	ProviderFormat provcore.Format
}

// filterCompatibleTargets splits targets by whether their provider wire
// format is compatible with the client's ingress body format.
//
// When bridge is non-nil, chat completions use the OpenAI hub matrix from
// [canonicalbridge.Bridge.EndpointRoutable]. When bridge is nil (narrow
// tests), the legacy matrix applies: same format, or ingress openairesponses.
//
// Targets whose AdapterType is empty or unknown are silently dropped
// from the compatible list — they would fail the adapter lookup
// downstream anyway; leaving them for the executor to surface would
// mask the real cause.
func filterCompatibleTargets(ingressFormat provcore.Format, targets []routingcore.RoutingTarget, ep typology.WireShape, bridge canonicalbridge.API) (compat []routingcore.RoutingTarget, rejected []RejectedTarget) {
	for _, t := range targets {
		pf := provcore.Format(t.AdapterType)
		if !pf.Valid() {
			continue
		}
		if bridge != nil {
			if bridge.EndpointRoutable(ep, ingressFormat, pf) {
				compat = append(compat, t)
				continue
			}
		} else if isSchemaCompatibleLegacy(ingressFormat, pf) {
			compat = append(compat, t)
			continue
		}
		rejected = append(rejected, RejectedTarget{RoutingTarget: t, ProviderFormat: pf})
	}
	return compat, rejected
}

// isSchemaCompatibleLegacy is the narrow-test fallback matrix (bridge is nil).
func isSchemaCompatibleLegacy(ingress, provider provcore.Format) bool {
	if ingress == provider {
		return true
	}
	return ingress == provcore.FormatOpenAI
}

// schemaMode returns the per-target mode string used by the simulate
// endpoint: "passthrough" when the formats match, "translated" when
// the gateway uses the hub (ingress→canonical→provider wire or the
// legacy openai-only codec path), "rejected" when no compatibility path exists.
func schemaMode(ingress, provider provcore.Format, ep typology.WireShape, bridge canonicalbridge.API) string {
	if bridge != nil {
		if !bridge.EndpointRoutable(ep, ingress, provider) {
			return "rejected"
		}
		if ingress == provider {
			return "passthrough"
		}
		return "translated"
	}
	switch ingress {
	case provider:
		return "passthrough"
	case provcore.FormatOpenAI:
		return "translated"
	default:
		return "rejected"
	}
}

// ResponsesCrossFormatRejection captures a field on a /v1/responses
// request body that cannot be honoured on a cross-format routing path
// (target adapter does not natively serve responses-api). Returned by
// [validateResponsesIngressForCrossFormat] so the handler emits the
// structured 400 envelope identifying the exact field.
type ResponsesCrossFormatRejection struct {
	// Param is the dotted JSON path of the offending field
	// ("previous_response_id", "store", "tools[0].type", …).
	Param string
	// Message is the human-readable explanation included in the 400
	// envelope's error.message.
	Message string
}

// validateResponsesIngressForCrossFormat inspects a /v1/responses request
// body for fields whose semantics require routing to a target that
// natively serves responses-api (today: spec_openai). Returns a non-nil
// rejection when:
//   - previous_response_id is present and non-empty
//   - store is true (any non-true value is fine, including absent)
//   - truncation is present and != "disabled"
//   - any tools[].type matches IsResponsesBuiltinTool
//
// Returns nil when the body is safe to cross-format. Callers MUST only
// invoke this on the cross-format path (i.e. when the routing target's
// adapter does not declare responses-api support); on same-shape
// passthrough every field rides through to OpenAI.
//
// Per provider-adapter-architecture.md §3a Rule 6.
func validateResponsesIngressForCrossFormat(body []byte) *ResponsesCrossFormatRejection {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	if v := gjson.GetBytes(body, "previous_response_id"); v.Exists() && strings.TrimSpace(v.String()) != "" {
		return &ResponsesCrossFormatRejection{
			Param:   "previous_response_id",
			Message: "Field 'previous_response_id' requires routing to a target that natively supports the OpenAI Responses API; configure a routing rule that resolves to OpenAI or remove the field.",
		}
	}
	if gjson.GetBytes(body, "store").Bool() {
		return &ResponsesCrossFormatRejection{
			Param:   "store",
			Message: "Field 'store=true' requires routing to a target that natively supports the OpenAI Responses API; configure a routing rule that resolves to OpenAI or set store to false.",
		}
	}
	if v := gjson.GetBytes(body, "truncation"); v.Exists() {
		if strings.TrimSpace(v.String()) != "disabled" {
			return &ResponsesCrossFormatRejection{
				Param:   "truncation",
				Message: "Field 'truncation' only accepts 'disabled' on a cross-format routing target; configure a routing rule that resolves to OpenAI to enable other modes.",
			}
		}
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		var rej *ResponsesCrossFormatRejection
		tools.ForEach(func(idx, item gjson.Result) bool {
			t := item.Get("type").String()
			if openairesponses.IsResponsesBuiltinTool(t) {
				rej = &ResponsesCrossFormatRejection{
					Param:   fmt.Sprintf("tools[%d].type", idx.Int()),
					Message: fmt.Sprintf("Built-in tool type %q requires routing to a target that natively supports the OpenAI Responses API; configure a routing rule that resolves to OpenAI or use a caller-defined function tool instead.", t),
				}
				return false
			}
			return true
		})
		if rej != nil {
			return rej
		}
	}
	return nil
}

// writeResponsesFeatureRejection emits the Responses-API-shape 400
// envelope when a request would route cross-format with a stateful or
// built-in field. error.code is the stable constant for SDK consumers
// to dispatch on.
func (h *Handler) writeResponsesFeatureRejection(w http.ResponseWriter, rec *audit.Record, rej *ResponsesCrossFormatRejection) {
	rec.HookReasonCode = "feature_requires_native_responses_target"
	body := envelope.GatewayErrorBodyWith(http.StatusBadRequest, "FEATURE_REQUIRES_NATIVE_RESPONSES_TARGET",
		rej.Message, "", map[string]any{"param": rej.Param})
	rejectWithBespokeEnvelope(w, rec, http.StatusBadRequest,
		"FEATURE_REQUIRES_NATIVE_RESPONSES_TARGET", rej.Message, body)
}

// writeNoCompatibleProvider emits the canonical 400 body for cross-format
// rejection. The message names the endpoint kind: routability is
// per-kind (a provider that serves chat fine may have no image leg), so
// a kind-less message reads as a blanket provider incompatibility and
// sends the admin down the wrong path.
func (h *Handler) writeNoCompatibleProvider(w http.ResponseWriter, rec *audit.Record, ingress provcore.Format, provider string, kind typology.EndpointKind) {
	rec.HookReasonCode = "no_compatible_provider"
	kindNoun := "these"
	if kind != "" {
		kindNoun = string(kind)
	}
	msg := fmt.Sprintf("ingress format %q cannot be routed to provider format %q for %s requests in this release", ingress, provider, kindNoun)
	// The ingress was already a parameter here and the body ignored it, so an
	// /v1/messages caller was handed the OpenAI {"error":{…}} shape instead of
	// {"type":"error",…} — a refusal an Anthropic SDK cannot deserialise.
	body := envelope.GatewayErrorBodyForIngress(ingress, http.StatusBadRequest, "NO_COMPATIBLE_PROVIDER", msg, "")
	rejectWithBespokeEnvelope(w, rec, http.StatusBadRequest, "NO_COMPATIBLE_PROVIDER", msg, body)
}

// writeCrossFormatStreamUnsupported rejects streaming when ingress and
// upstream SSE framing are not mutually compatible without a streaming
// transcoder.
func (h *Handler) writeCrossFormatStreamUnsupported(w http.ResponseWriter, rec *audit.Record, ingress, target string) {
	rec.HookReasonCode = "cross_format_stream_unsupported"
	msg := fmt.Sprintf("streaming is not supported for ingress format %q to provider format %q; use a non-streaming request or match ingress and provider wire formats", ingress, target)
	body := envelope.GatewayErrorBodyForIngress(provcore.Format(ingress), http.StatusBadRequest,
		"CROSS_FORMAT_STREAM_UNSUPPORTED", msg, "")
	rejectWithBespokeEnvelope(w, rec, http.StatusBadRequest, "CROSS_FORMAT_STREAM_UNSUPPORTED", msg, body)
}
