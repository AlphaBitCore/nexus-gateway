package mq

// binwire.go — the binary wire codec for TrafficEventMessage on the audit MQ
// path (gw → Hub). It replaces the per-record JSON form with a compact
// tag-length-value (TLV) layout:
//
//	record = [field-id uvarint][value]…   (repeated; the frame's record-length
//	                                        prefix delimits one record)
//
// The value layout is implied by the field-id (the decoder knows that, e.g.,
// FldPromptTokens is a varint), so no per-field type byte is carried. Only
// PRESENT fields are written — presence is conveyed by the field-id appearing,
// which reproduces the producer struct's `omitempty` semantics and the consumer
// struct's pointer-for-NULL semantics without a separate bitmap. Always-on wire
// fields (id/source/timestamp/latencyMs/bodies) are written unconditionally.
//
// Why binary: on the audit hot path the JSON envelope is the dominant single-box
// CPU cost — Hub-side go-json key matching (skipObject / mapassign / aeshashbody)
// + body base64 decode, gw-side marshal + body base64 encode. TLV removes key
// matching entirely (a switch on the integer id), and bodies travel as the raw
// (already-compressed) frame with no base64, which also shrinks the NATS message
// ~25% on large bodies and ~50% on small ones (the field-name envelope collapses
// from ~25 bytes/key to a 1–2 byte id).
//
// WIRE STABILITY (binding): field-ids are an append-only registry. Never reorder,
// never reuse a retired id, never change a field's implied value type. Adding a
// message field => add a new id at the end + handle it on both encode and decode.
// TestBinwireFieldRegistryNoDrift enforces that every json-tagged struct field
// has a registered id.

import (
	"encoding/binary"
	"math"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// BinwireMagic is the first byte of a binary audit frame. A frame that does not
// begin with this byte is a legacy NDJSON-of-JSON frame, so the Hub can dual-read
// both forms during a rollout and drain any in-flight JSON messages. '{' (0x7b),
// the first byte of a JSON record, and '\n' can never collide with this value.
const BinwireMagic = 0x01

// FieldID identifies a TrafficEventMessage field on the binary wire. Append-only.
type FieldID uint32

// Field-id registry — append-only; the numeric values are the wire contract.
const (
	FldID                     FieldID = 1
	FldSource                 FieldID = 2
	FldTraceID                FieldID = 3
	FldExternalRequestID      FieldID = 4
	FldTimestamp              FieldID = 5
	FldSourceIP               FieldID = 6
	FldTargetHost             FieldID = 7
	FldMethod                 FieldID = 8
	FldPath                   FieldID = 9
	FldTargetMethod           FieldID = 10
	FldTargetPath             FieldID = 11
	FldStatusCode             FieldID = 12
	FldLatencyMs              FieldID = 13
	FldEntityType             FieldID = 14
	FldEntityID               FieldID = 15
	FldEntityName             FieldID = 16
	FldOrgID                  FieldID = 17
	FldOrgName                FieldID = 18
	FldIdentity               FieldID = 19
	FldEndpointType           FieldID = 20
	FldIngressFormat          FieldID = 21
	FldProviderID             FieldID = 22
	FldProviderName           FieldID = 23
	FldModelID                FieldID = 24
	FldModelName              FieldID = 25
	FldPromptTokens           FieldID = 26
	FldCompletionTokens       FieldID = 27
	FldTotalTokens            FieldID = 28
	FldReasoningTokens        FieldID = 29
	FldReasoningCostUsd       FieldID = 30
	FldEstimatedCostUsd       FieldID = 31
	FldCacheStatus            FieldID = 32
	FldGatewayCacheStatus     FieldID = 33
	FldGatewayCacheSkipReason FieldID = 34
	FldGatewayCacheKind       FieldID = 35
	FldGatewayCacheL2EntryKey FieldID = 36
	FldProviderCacheStatus    FieldID = 37
	FldOriginTZ               FieldID = 38
	FldRoutedProviderID       FieldID = 39
	FldRoutedProviderName     FieldID = 40
	FldRoutedModelID          FieldID = 41
	FldRoutedModelName        FieldID = 42
	FldRoutingRuleID          FieldID = 43
	FldRoutingRuleName        FieldID = 44
	FldRequestHookDecision    FieldID = 45
	FldRequestHookReason      FieldID = 46
	FldRequestHookReasonCode  FieldID = 47
	FldResponseHookDecision   FieldID = 48
	FldResponseHookReason     FieldID = 49
	FldResponseHookReasonCode FieldID = 50
	FldComplianceTags         FieldID = 51
	FldGatewayCacheSavingsUsd FieldID = 52
	FldEmbeddingCostUsd       FieldID = 53
	FldEmbeddingModelID       FieldID = 54
	FldAIGuardCostUsd         FieldID = 55
	FldInternalOpsBreakdown   FieldID = 56
	FldCacheCreationTokens    FieldID = 57
	FldCacheReadTokens        FieldID = 58
	FldCacheWriteCostUsd      FieldID = 59
	FldCacheReadSavingsUsd    FieldID = 60
	FldCacheNetSavingsUsd     FieldID = 61
	FldNormalizedStripCount   FieldID = 62
	FldNormalizedStripBytes   FieldID = 63
	FldCacheMarkerInjected    FieldID = 64
	FldAPIKeyClass            FieldID = 65
	FldAPIKeyFingerprint      FieldID = 66
	FldUsageExtractionStatus  FieldID = 67
	FldErrorCode              FieldID = 68
	FldErrorReason            FieldID = 69
	FldRequestHooksPipeline   FieldID = 70
	FldResponseHooksPipeline  FieldID = 71
	FldRoutingTrace           FieldID = 72
	FldDetails                FieldID = 73
	FldRequestBody            FieldID = 74
	FldResponseBody           FieldID = 75
	FldRequestNormalized      FieldID = 76
	FldResponseNormalized     FieldID = 77
	FldRequestNormalizeStatus FieldID = 78
	FldRespNormalizeStatus    FieldID = 79
	FldRequestNormalizeError  FieldID = 80
	FldResponseNormalizeError FieldID = 81
	FldNormalizeVersion       FieldID = 82
	FldRequestRedactionSpans  FieldID = 83
	FldResponseRedactionSpans FieldID = 84
	FldBumpStatus             FieldID = 85
	FldPassthroughFlags       FieldID = 86
	FldPassthroughReason      FieldID = 87
	FldInternalPurpose        FieldID = 88
	FldRequestBlockingRule    FieldID = 89
	FldResponseBlockingRule   FieldID = 90
	FldCredentialID           FieldID = 91
	FldThingID                FieldID = 92
	FldThingName              FieldID = 93
	FldUpstreamTtfbMs         FieldID = 94
	FldUpstreamTotalMs        FieldID = 95
	FldRequestHooksMs         FieldID = 96
	FldResponseHooksMs        FieldID = 97
	FldLatencyBreakdown       FieldID = 98
	FldAttestationVerified    FieldID = 99
	FldAttestationAgentID     FieldID = 100
	FldSourceProcess          FieldID = 101
	FldAction                 FieldID = 102
	// Additive microsecond hook aggregates (siblings of FldRequestHooksMs(96) /
	// FldResponseHooksMs(97)). New ids — see the FORWARD-INCOMPATIBLE note on
	// AllFieldIDs: a producer emitting these requires a Hub that decodes them
	// (deploy order: schema → Hub → producers).
	FldRequestHooksUs  FieldID = 103
	FldResponseHooksUs FieldID = 104
)

// AllFieldIDs returns every registered field-id in wire order. It exists so the
// drift guard (TestBinwireFieldRegistryNoDrift) can assert the registry stays in
// lockstep with the struct: a TrafficEventMessage field added without a matching
// id — which the binary codec would silently drop — fails the count check.
func AllFieldIDs() []FieldID {
	return []FieldID{
		FldID, FldSource, FldTraceID, FldExternalRequestID, FldTimestamp,
		FldSourceIP, FldTargetHost, FldMethod, FldPath, FldTargetMethod,
		FldTargetPath, FldStatusCode, FldLatencyMs, FldEntityType, FldEntityID,
		FldEntityName, FldOrgID, FldOrgName, FldIdentity, FldEndpointType,
		FldIngressFormat, FldProviderID, FldProviderName, FldModelID, FldModelName,
		FldPromptTokens, FldCompletionTokens, FldTotalTokens, FldReasoningTokens,
		FldReasoningCostUsd, FldEstimatedCostUsd, FldCacheStatus, FldGatewayCacheStatus,
		FldGatewayCacheSkipReason, FldGatewayCacheKind, FldGatewayCacheL2EntryKey,
		FldProviderCacheStatus, FldOriginTZ, FldRoutedProviderID, FldRoutedProviderName,
		FldRoutedModelID, FldRoutedModelName, FldRoutingRuleID, FldRoutingRuleName,
		FldRequestHookDecision, FldRequestHookReason, FldRequestHookReasonCode,
		FldResponseHookDecision, FldResponseHookReason, FldResponseHookReasonCode,
		FldComplianceTags, FldGatewayCacheSavingsUsd, FldEmbeddingCostUsd,
		FldEmbeddingModelID, FldAIGuardCostUsd, FldInternalOpsBreakdown,
		FldCacheCreationTokens, FldCacheReadTokens, FldCacheWriteCostUsd,
		FldCacheReadSavingsUsd, FldCacheNetSavingsUsd, FldNormalizedStripCount,
		FldNormalizedStripBytes, FldCacheMarkerInjected, FldAPIKeyClass,
		FldAPIKeyFingerprint, FldUsageExtractionStatus, FldErrorCode, FldErrorReason,
		FldRequestHooksPipeline, FldResponseHooksPipeline, FldRoutingTrace, FldDetails,
		FldRequestBody, FldResponseBody, FldRequestNormalized, FldResponseNormalized,
		FldRequestNormalizeStatus, FldRespNormalizeStatus, FldRequestNormalizeError,
		FldResponseNormalizeError, FldNormalizeVersion, FldRequestRedactionSpans,
		FldResponseRedactionSpans, FldBumpStatus, FldPassthroughFlags, FldPassthroughReason,
		FldInternalPurpose, FldRequestBlockingRule, FldResponseBlockingRule,
		FldCredentialID, FldThingID, FldThingName, FldUpstreamTtfbMs, FldUpstreamTotalMs,
		FldRequestHooksMs, FldResponseHooksMs, FldLatencyBreakdown, FldAttestationVerified,
		FldAttestationAgentID, FldSourceProcess, FldAction,
		FldRequestHooksUs, FldResponseHooksUs,
	}
}

// --- low-level value appenders (id-tagged). Value layout is implied by the id. ---

func putID(dst []byte, id FieldID) []byte { return binary.AppendUvarint(dst, uint64(id)) }

func aStr(dst []byte, id FieldID, s string) []byte {
	dst = putID(dst, id)
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func aBytes(dst []byte, id FieldID, b []byte) []byte {
	dst = putID(dst, id)
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// aI64 writes a signed integer zig-zag varint (compact for small magnitudes).
func aI64(dst []byte, id FieldID, v int64) []byte {
	dst = putID(dst, id)
	return binary.AppendVarint(dst, v)
}

func aF64(dst []byte, id FieldID, f float64) []byte {
	dst = putID(dst, id)
	return binary.LittleEndian.AppendUint64(dst, math.Float64bits(f))
}

func aBool(dst []byte, id FieldID, v bool) []byte {
	dst = putID(dst, id)
	if v {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func aTimeNanos(dst []byte, id FieldID, nanos int64) []byte {
	dst = putID(dst, id)
	return binary.AppendVarint(dst, nanos)
}

func aStrSlice(dst []byte, id FieldID, ss []string) []byte {
	dst = putID(dst, id)
	dst = binary.AppendUvarint(dst, uint64(len(ss)))
	for _, s := range ss {
		dst = binary.AppendUvarint(dst, uint64(len(s)))
		dst = append(dst, s...)
	}
	return dst
}

// aJSON writes a length-prefixed raw-JSON blob for the schema-opaque fields
// (Identity / *HooksPipeline / RoutingTrace / Details / *Normalized /
// *RedactionSpans / *BlockingRule / InternalOpsBreakdown / LatencyBreakdown).
// The decoder stores these back verbatim as json.RawMessage.
func aJSON(dst []byte, id FieldID, raw []byte) []byte { return aBytes(dst, id, raw) }

// AppendBinary appends m as one binary TLV record to dst and returns the grown
// slice. Only present fields are written (mirroring the producer's omitempty);
// the always-on wire fields (id/source/timestamp/latencyMs/bodies) are written
// unconditionally so the consumer's non-pointer columns are never left unset.
func (m *TrafficEventMessage) AppendBinary(dst []byte) []byte {
	// Always-on scalar header.
	dst = aStr(dst, FldID, m.ID)
	dst = aStr(dst, FldSource, m.Source)
	dst = aTimeNanos(dst, FldTimestamp, m.Timestamp.UnixNano())
	dst = aI64(dst, FldLatencyMs, int64(m.LatencyMs))

	// Optional strings (emit if non-empty).
	dst = optStr(dst, FldTraceID, m.TraceID)
	dst = optStr(dst, FldExternalRequestID, m.ExternalRequestID)
	dst = optStr(dst, FldSourceIP, m.SourceIP)
	dst = optStr(dst, FldTargetHost, m.TargetHost)
	dst = optStr(dst, FldMethod, m.Method)
	dst = optStr(dst, FldPath, m.Path)
	dst = optStr(dst, FldTargetMethod, m.TargetMethod)
	dst = optStr(dst, FldTargetPath, m.TargetPath)
	dst = optStr(dst, FldEntityType, m.EntityType)
	dst = optStr(dst, FldEntityID, m.EntityID)
	dst = optStr(dst, FldEntityName, m.EntityName)
	dst = optStr(dst, FldOrgID, m.OrgID)
	dst = optStr(dst, FldOrgName, m.OrgName)
	dst = optStr(dst, FldEndpointType, m.EndpointType)
	dst = optStr(dst, FldIngressFormat, m.IngressFormat)
	dst = optStr(dst, FldProviderID, m.ProviderID)
	dst = optStr(dst, FldProviderName, m.ProviderName)
	dst = optStr(dst, FldModelID, m.ModelID)
	dst = optStr(dst, FldModelName, m.ModelName)
	dst = optStr(dst, FldCacheStatus, m.CacheStatus)
	dst = optStr(dst, FldGatewayCacheStatus, m.GatewayCacheStatus)
	dst = optStr(dst, FldGatewayCacheSkipReason, m.GatewayCacheSkipReason)
	dst = optStr(dst, FldGatewayCacheKind, m.GatewayCacheKind)
	dst = optStr(dst, FldGatewayCacheL2EntryKey, m.GatewayCacheL2EntryKey)
	dst = optStr(dst, FldProviderCacheStatus, m.ProviderCacheStatus)
	dst = optStr(dst, FldRoutedProviderID, m.RoutedProviderID)
	dst = optStr(dst, FldRoutedProviderName, m.RoutedProviderName)
	dst = optStr(dst, FldRoutedModelID, m.RoutedModelID)
	dst = optStr(dst, FldRoutedModelName, m.RoutedModelName)
	dst = optStr(dst, FldRoutingRuleID, m.RoutingRuleID)
	dst = optStr(dst, FldRoutingRuleName, m.RoutingRuleName)
	dst = optStr(dst, FldRequestHookDecision, m.RequestHookDecision)
	dst = optStr(dst, FldRequestHookReason, m.RequestHookReason)
	dst = optStr(dst, FldRequestHookReasonCode, m.RequestHookReasonCode)
	dst = optStr(dst, FldResponseHookDecision, m.ResponseHookDecision)
	dst = optStr(dst, FldResponseHookReason, m.ResponseHookReason)
	dst = optStr(dst, FldResponseHookReasonCode, m.ResponseHookReasonCode)
	dst = optStr(dst, FldEmbeddingModelID, m.EmbeddingModelID)
	dst = optStr(dst, FldAPIKeyClass, m.APIKeyClass)
	dst = optStr(dst, FldAPIKeyFingerprint, m.APIKeyFingerprint)
	dst = optStr(dst, FldUsageExtractionStatus, m.UsageExtractionStatus)
	dst = optStr(dst, FldRequestNormalizeStatus, m.RequestNormalizeStatus)
	dst = optStr(dst, FldRespNormalizeStatus, m.ResponseNormalizeStatus)
	dst = optStr(dst, FldRequestNormalizeError, m.RequestNormalizeError)
	dst = optStr(dst, FldResponseNormalizeError, m.ResponseNormalizeError)
	dst = optStr(dst, FldNormalizeVersion, m.NormalizeVersion)
	dst = optStr(dst, FldBumpStatus, m.BumpStatus)
	dst = optStr(dst, FldPassthroughReason, m.PassthroughReason)
	dst = optStr(dst, FldCredentialID, m.CredentialID)
	dst = optStr(dst, FldThingID, m.ThingID)
	dst = optStr(dst, FldThingName, m.ThingName)
	dst = optStr(dst, FldAttestationAgentID, m.AttestationAgentID)
	dst = optStr(dst, FldSourceProcess, m.SourceProcess)
	dst = optStr(dst, FldAction, m.Action)

	// Pointer strings (emit if non-nil).
	dst = optPtrStr(dst, FldOriginTZ, m.OriginTZ)
	dst = optPtrStr(dst, FldErrorCode, m.ErrorCode)
	dst = optPtrStr(dst, FldErrorReason, m.ErrorReason)
	dst = optPtrStr(dst, FldInternalPurpose, m.InternalPurpose)

	// Integers (emit if non-zero — mirrors omitempty).
	dst = optI64(dst, FldStatusCode, int64(m.StatusCode))
	dst = optI64(dst, FldPromptTokens, m.PromptTokens)
	dst = optI64(dst, FldCompletionTokens, m.CompletionTokens)
	dst = optI64(dst, FldTotalTokens, m.TotalTokens)
	dst = optI64(dst, FldReasoningTokens, m.ReasoningTokens)

	// Pointer integers (emit if non-nil).
	dst = optPtrI64(dst, FldCacheCreationTokens, m.CacheCreationTokens)
	dst = optPtrI64(dst, FldCacheReadTokens, m.CacheReadTokens)
	dst = optPtrIntAsI64(dst, FldNormalizedStripCount, m.NormalizedStripCount)
	dst = optPtrIntAsI64(dst, FldNormalizedStripBytes, m.NormalizedStripBytes)
	dst = optPtrIntAsI64(dst, FldCacheMarkerInjected, m.CacheMarkerInjected)
	dst = optPtrIntAsI64(dst, FldUpstreamTtfbMs, m.UpstreamTtfbMs)
	dst = optPtrIntAsI64(dst, FldUpstreamTotalMs, m.UpstreamTotalMs)
	dst = optPtrIntAsI64(dst, FldRequestHooksMs, m.RequestHooksMs)
	dst = optPtrIntAsI64(dst, FldResponseHooksMs, m.ResponseHooksMs)
	dst = optPtrIntAsI64(dst, FldRequestHooksUs, m.RequestHooksUs)
	dst = optPtrIntAsI64(dst, FldResponseHooksUs, m.ResponseHooksUs)

	// Floats (emit if non-zero).
	dst = optF64(dst, FldReasoningCostUsd, m.ReasoningCostUsd)
	dst = optF64(dst, FldEstimatedCostUsd, m.EstimatedCostUsd)

	// Pointer floats (emit if non-nil).
	dst = optPtrF64(dst, FldGatewayCacheSavingsUsd, m.GatewayCacheSavingsUsd)
	dst = optPtrF64(dst, FldEmbeddingCostUsd, m.EmbeddingCostUsd)
	dst = optPtrF64(dst, FldAIGuardCostUsd, m.AIGuardCostUsd)
	dst = optPtrF64(dst, FldCacheWriteCostUsd, m.CacheWriteCostUsd)
	dst = optPtrF64(dst, FldCacheReadSavingsUsd, m.CacheReadSavingsUsd)
	dst = optPtrF64(dst, FldCacheNetSavingsUsd, m.CacheNetSavingsUsd)

	// Bool (emit if true).
	if m.AttestationVerified {
		dst = aBool(dst, FldAttestationVerified, true)
	}

	// String slices (emit if non-empty).
	if len(m.ComplianceTags) > 0 {
		dst = aStrSlice(dst, FldComplianceTags, m.ComplianceTags)
	}
	if len(m.PassthroughFlags) > 0 {
		dst = aStrSlice(dst, FldPassthroughFlags, m.PassthroughFlags)
	}

	// Schema-opaque JSON blobs (emit if non-empty). map/any are marshalled once;
	// json.RawMessage rides verbatim.
	dst = optJSONValue(dst, FldIdentity, m.Identity)
	dst = optJSONValue(dst, FldLatencyBreakdown, m.LatencyBreakdown)
	dst = optJSONValue(dst, FldRequestHooksPipeline, m.RequestHooksPipeline)
	dst = optJSONValue(dst, FldResponseHooksPipeline, m.ResponseHooksPipeline)
	dst = optJSONValue(dst, FldRoutingTrace, m.RoutingTrace)
	dst = optJSONValue(dst, FldDetails, m.Details)
	dst = optRaw(dst, FldInternalOpsBreakdown, m.InternalOpsBreakdown)
	dst = optRaw(dst, FldRequestNormalized, m.RequestNormalized)
	dst = optRaw(dst, FldResponseNormalized, m.ResponseNormalized)
	dst = optRaw(dst, FldRequestRedactionSpans, m.RequestRedactionSpans)
	dst = optRaw(dst, FldResponseRedactionSpans, m.ResponseRedactionSpans)
	dst = optPtrRaw(dst, FldRequestBlockingRule, m.RequestBlockingRule)
	dst = optPtrRaw(dst, FldResponseBlockingRule, m.ResponseBlockingRule)

	// Bodies — always on the wire (the consumer columns are non-pointer Body).
	dst = putID(dst, FldRequestBody)
	dst = audit.AppendBodyBinary(dst, m.RequestBody)
	dst = putID(dst, FldResponseBody)
	dst = audit.AppendBodyBinary(dst, m.ResponseBody)

	return dst
}

// --- present-check wrappers (keep AppendBinary readable) ---

func optStr(dst []byte, id FieldID, s string) []byte {
	if s == "" {
		return dst
	}
	return aStr(dst, id, s)
}

func optPtrStr(dst []byte, id FieldID, p *string) []byte {
	if p == nil {
		return dst
	}
	return aStr(dst, id, *p)
}

func optI64(dst []byte, id FieldID, v int64) []byte {
	if v == 0 {
		return dst
	}
	return aI64(dst, id, v)
}

func optPtrI64(dst []byte, id FieldID, p *int64) []byte {
	if p == nil {
		return dst
	}
	return aI64(dst, id, *p)
}

func optPtrIntAsI64(dst []byte, id FieldID, p *int) []byte {
	if p == nil {
		return dst
	}
	return aI64(dst, id, int64(*p))
}

func optF64(dst []byte, id FieldID, f float64) []byte {
	if f == 0 {
		return dst
	}
	return aF64(dst, id, f)
}

func optPtrF64(dst []byte, id FieldID, p *float64) []byte {
	if p == nil {
		return dst
	}
	return aF64(dst, id, *p)
}

func optRaw(dst []byte, id FieldID, raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return dst
	}
	return aJSON(dst, id, raw)
}

func optPtrRaw(dst []byte, id FieldID, p *json.RawMessage) []byte {
	if p == nil || len(*p) == 0 {
		return dst
	}
	return aJSON(dst, id, *p)
}

// optJSONValue marshals a schema-opaque value (map/any) and writes it as a JSON
// blob when it is non-nil and not the literal null. A marshal error drops the
// field (it would have produced an unreadable column anyway); audit never fails
// the surrounding record over one opaque side-field.
func optJSONValue(dst []byte, id FieldID, v any) []byte {
	if v == nil {
		return dst
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return dst
	}
	return aJSON(dst, id, raw)
}
