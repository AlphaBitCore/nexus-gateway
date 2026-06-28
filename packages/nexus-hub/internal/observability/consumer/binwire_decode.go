package consumer

// binwire_decode.go — decodes one binary-wire audit record (see
// shared/transport/mq/binwire.go) into the consumer TrafficEventMessage. It is
// the inverse of mq.TrafficEventMessage.AppendBinary: a TLV stream where each
// field-id implies its value layout. Presence is conveyed by the id appearing, so
// only present fields are set — absent pointer fields stay nil (→ SQL NULL),
// exactly as the JSON path produced via pointer-for-NULL.
//
// Type bridging mirrors what go-json did across the two structs: the producer's
// int64 token counts land in the consumer's *int columns, its bool attestation in
// a *bool, and its map/any/*RawMessage side-fields in the consumer's value
// json.RawMessage columns.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// decodeBinaryRecordSafe wraps decodeBinaryRecord with a panic recover so a
// corrupt or hostile frame can never crash the Hub's consume goroutine. The
// length parsers are bounds-checked against the remaining buffer, so a panic
// here would mean a previously-unseen decoder bug; converting it to an error
// makes the caller treat the record as poison (dropped + counted) instead of
// taking the process down and crash-looping via JetStream redelivery.
func decodeBinaryRecordSafe(data []byte) (evt TrafficEventMessage, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("binwire: decode panic: %v", rec)
		}
	}()
	return decodeBinaryRecord(data)
}

// decodeBinaryRecord parses one binary TLV record into a TrafficEventMessage.
func decodeBinaryRecord(data []byte) (TrafficEventMessage, error) {
	var e TrafficEventMessage
	err := decodeBinaryRecordInto(&e, data, false)
	return e, err
}

// decodeBinaryRecordInto parses one binary TLV record into the caller-provided
// TrafficEventMessage, which MUST be zeroed. skipBody decodes body fields as
// metadata only (zero-copy, content skipped) for read-only consumers. Decoding
// into a caller-owned (poolable) struct keeps the hot audit/alert drain free of
// a per-record struct allocation.
func decodeBinaryRecordInto(e *TrafficEventMessage, data []byte, skipBody bool) error {
	r := recReader{b: data, skipBody: skipBody}
	for r.err == nil && r.n < len(r.b) {
		id := mq.FieldID(r.uvarint())
		if r.err != nil {
			break
		}
		switch id {
		case mq.FldID:
			e.ID = r.str()
		case mq.FldSource:
			e.Source = r.str()
		case mq.FldTimestamp:
			e.Timestamp = time.Unix(0, r.varint()).UTC()
		case mq.FldLatencyMs:
			e.LatencyMs = pInt(int(r.varint()))
		case mq.FldTraceID:
			e.TraceID = pStr(r.str())
		case mq.FldExternalRequestID:
			e.ExternalRequestID = pStr(r.str())
		case mq.FldSourceIP:
			e.SourceIP = pStr(r.str())
		case mq.FldTargetHost:
			e.TargetHost = pStr(r.str())
		case mq.FldMethod:
			e.Method = pStr(r.str())
		case mq.FldPath:
			e.Path = pStr(r.str())
		case mq.FldTargetMethod:
			e.TargetMethod = pStr(r.str())
		case mq.FldTargetPath:
			e.TargetPath = pStr(r.str())
		case mq.FldStatusCode:
			e.StatusCode = pInt(int(r.varint()))
		case mq.FldEntityType:
			e.EntityType = pStr(r.str())
		case mq.FldEntityID:
			e.EntityID = pStr(r.str())
		case mq.FldEntityName:
			e.EntityName = pStr(r.str())
		case mq.FldOrgID:
			e.OrgID = pStr(r.str())
		case mq.FldOrgName:
			e.OrgName = pStr(r.str())
		case mq.FldIdentity:
			e.Identity = r.json()
		case mq.FldEndpointType:
			e.EndpointType = r.str()
		case mq.FldIngressFormat:
			e.IngressFormat = r.str()
		case mq.FldProviderID:
			e.ProviderID = pStr(r.str())
		case mq.FldProviderName:
			e.ProviderName = pStr(r.str())
		case mq.FldModelID:
			e.ModelID = pStr(r.str())
		case mq.FldModelName:
			e.ModelName = pStr(r.str())
		case mq.FldPromptTokens:
			e.PromptTokens = pInt(int(r.varint()))
		case mq.FldCompletionTokens:
			e.CompletionTokens = pInt(int(r.varint()))
		case mq.FldTotalTokens:
			e.TotalTokens = pInt(int(r.varint()))
		case mq.FldReasoningTokens:
			e.ReasoningTokens = pInt(int(r.varint()))
		case mq.FldReasoningCostUsd:
			e.ReasoningCostUsd = pF64(r.f64())
		case mq.FldEstimatedCostUsd:
			e.EstimatedCostUSD = pF64(r.f64())
		case mq.FldCacheStatus:
			e.CacheStatus = pStr(r.str())
		case mq.FldGatewayCacheStatus:
			e.GatewayCacheStatus = pStr(r.str())
		case mq.FldGatewayCacheSkipReason:
			e.GatewayCacheSkipReason = pStr(r.str())
		case mq.FldGatewayCacheKind:
			e.GatewayCacheKind = pStr(r.str())
		case mq.FldGatewayCacheL2EntryKey:
			e.GatewayCacheL2EntryKey = r.str()
		case mq.FldProviderCacheStatus:
			e.ProviderCacheStatus = pStr(r.str())
		case mq.FldOriginTZ:
			e.OriginTZ = pStr(r.str())
		case mq.FldRoutedProviderID:
			e.RoutedProviderID = pStr(r.str())
		case mq.FldRoutedProviderName:
			e.RoutedProviderName = pStr(r.str())
		case mq.FldRoutedModelID:
			e.RoutedModelID = pStr(r.str())
		case mq.FldRoutedModelName:
			e.RoutedModelName = pStr(r.str())
		case mq.FldRoutingRuleID:
			e.RoutingRuleID = pStr(r.str())
		case mq.FldRoutingRuleName:
			e.RoutingRuleName = pStr(r.str())
		case mq.FldRequestHookDecision:
			e.RequestHookDecision = pStr(r.str())
		case mq.FldRequestHookReason:
			e.RequestHookReason = pStr(r.str())
		case mq.FldRequestHookReasonCode:
			e.RequestHookReasonCode = pStr(r.str())
		case mq.FldResponseHookDecision:
			e.ResponseHookDecision = pStr(r.str())
		case mq.FldResponseHookReason:
			e.ResponseHookReason = pStr(r.str())
		case mq.FldResponseHookReasonCode:
			e.ResponseHookReasonCode = pStr(r.str())
		case mq.FldComplianceTags:
			e.ComplianceTags = r.strSlice()
		case mq.FldGatewayCacheSavingsUsd:
			e.GatewayCacheSavingsUsd = pF64(r.f64())
		case mq.FldEmbeddingCostUsd:
			e.EmbeddingCostUsd = pF64(r.f64())
		case mq.FldEmbeddingModelID:
			e.EmbeddingModelID = r.str()
		case mq.FldAIGuardCostUsd:
			e.AIGuardCostUsd = pF64(r.f64())
		case mq.FldInternalOpsBreakdown:
			e.InternalOpsBreakdown = r.json()
		case mq.FldCacheCreationTokens:
			e.CacheCreationTokens = pI64(r.varint())
		case mq.FldCacheReadTokens:
			e.CacheReadTokens = pI64(r.varint())
		case mq.FldCacheWriteCostUsd:
			e.CacheWriteCostUsd = pF64(r.f64())
		case mq.FldCacheReadSavingsUsd:
			e.CacheReadSavingsUsd = pF64(r.f64())
		case mq.FldCacheNetSavingsUsd:
			e.CacheNetSavingsUsd = pF64(r.f64())
		case mq.FldNormalizedStripCount:
			e.NormalizedStripCount = pInt(int(r.varint()))
		case mq.FldNormalizedStripBytes:
			e.NormalizedStripBytes = pInt(int(r.varint()))
		case mq.FldCacheMarkerInjected:
			e.CacheMarkerInjected = pInt(int(r.varint()))
		case mq.FldAPIKeyClass:
			e.APIKeyClass = pStr(r.str())
		case mq.FldAPIKeyFingerprint:
			e.APIKeyFingerprint = pStr(r.str())
		case mq.FldUsageExtractionStatus:
			e.UsageExtractionStatus = pStr(r.str())
		case mq.FldErrorCode:
			e.ErrorCode = pStr(r.str())
		case mq.FldErrorReason:
			e.ErrorReason = pStr(r.str())
		case mq.FldRequestHooksPipeline:
			e.RequestHooksPipeline = r.json()
		case mq.FldResponseHooksPipeline:
			e.ResponseHooksPipeline = r.json()
		case mq.FldRoutingTrace:
			e.RoutingTrace = r.json()
		case mq.FldDetails:
			e.Details = r.json()
		case mq.FldRequestBody:
			e.RequestBody = r.body()
		case mq.FldResponseBody:
			e.ResponseBody = r.body()
		case mq.FldRequestNormalized:
			e.RequestNormalized = r.json()
		case mq.FldResponseNormalized:
			e.ResponseNormalized = r.json()
		case mq.FldRequestNormalizeStatus:
			e.RequestNormalizeStatus = r.str()
		case mq.FldRespNormalizeStatus:
			e.ResponseNormalizeStatus = r.str()
		case mq.FldRequestNormalizeError:
			e.RequestNormalizeError = r.str()
		case mq.FldResponseNormalizeError:
			e.ResponseNormalizeError = r.str()
		case mq.FldNormalizeVersion:
			e.NormalizeVersion = r.str()
		case mq.FldRequestRedactionSpans:
			e.RequestRedactionSpans = r.json()
		case mq.FldResponseRedactionSpans:
			e.ResponseRedactionSpans = r.json()
		case mq.FldBumpStatus:
			e.BumpStatus = pStr(r.str())
		case mq.FldPassthroughFlags:
			e.PassthroughFlags = r.strSlice()
		case mq.FldPassthroughReason:
			e.PassthroughReason = r.str()
		case mq.FldInternalPurpose:
			e.InternalPurpose = pStr(r.str())
		case mq.FldRequestBlockingRule:
			e.RequestBlockingRule = r.json()
		case mq.FldResponseBlockingRule:
			e.ResponseBlockingRule = r.json()
		case mq.FldCredentialID:
			e.CredentialID = pStr(r.str())
		case mq.FldThingID:
			e.ThingID = pStr(r.str())
		case mq.FldThingName:
			e.ThingName = pStr(r.str())
		case mq.FldUpstreamTtfbMs:
			e.UpstreamTtfbMs = pInt(int(r.varint()))
		case mq.FldUpstreamTotalMs:
			e.UpstreamTotalMs = pInt(int(r.varint()))
		case mq.FldRequestHooksMs:
			e.RequestHooksMs = pInt(int(r.varint()))
		case mq.FldResponseHooksMs:
			e.ResponseHooksMs = pInt(int(r.varint()))
		case mq.FldLatencyBreakdown:
			e.LatencyBreakdown = r.json()
		case mq.FldAttestationVerified:
			e.AttestationVerified = pBool(r.boolean())
		case mq.FldAttestationAgentID:
			e.AttestationAgentID = pStr(r.str())
		case mq.FldSourceProcess:
			e.SourceProcess = pStr(r.str())
		case mq.FldAction:
			e.Action = pStr(r.str())
		default:
			// An unknown id means a producer newer than this Hub (a field added
			// without the matching decode case). There is no per-field length to
			// skip, so fail the record rather than silently mis-parse the rest;
			// dual-read + coordinated gw/Hub deploy keeps this from happening in
			// practice, and the drift guard test prevents shipping such a skew.
			return fmt.Errorf("binwire: unknown field id %d", id)
		}
	}
	return r.err
}

// recReader is a sticky-error sequential reader over one binary record.
type recReader struct {
	b   []byte
	n   int
	err error
	// skipBody makes body() decode only the body's metadata (Kind/Truncated/…)
	// and skip the inline content bytes (zero-copy) — the read-only alerts path
	// needs body presence/shape, not the payload.
	skipBody bool
}

func (r *recReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b[r.n:])
	if n <= 0 {
		r.err = errors.New("binwire: bad uvarint")
		return 0
	}
	r.n += n
	return v
}

func (r *recReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b[r.n:])
	if n <= 0 {
		r.err = errors.New("binwire: bad varint")
		return 0
	}
	r.n += n
	return v
}

func (r *recReader) f64() float64 {
	if r.err != nil {
		return 0
	}
	if r.n+8 > len(r.b) {
		r.err = errors.New("binwire: short read (f64)")
		return 0
	}
	bits := binary.LittleEndian.Uint64(r.b[r.n:])
	r.n += 8
	return math.Float64frombits(bits)
}

func (r *recReader) boolean() bool {
	if r.err != nil {
		return false
	}
	if r.n >= len(r.b) {
		r.err = errors.New("binwire: short read (bool)")
		return false
	}
	v := r.b[r.n]
	r.n++
	return v != 0
}

// bytesCopy reads a uvarint length then that many bytes, copied off the source so
// the value outlives the recycled NATS message buffer.
func (r *recReader) bytesCopy() []byte {
	ln := r.uvarint()
	if r.err != nil || ln == 0 {
		return nil
	}
	// Unsigned bound: a length prefix >= 2^63 would make int(ln) negative and
	// wrap r.n+int(ln) past the check, then panic in make/slice. Compare against
	// the remaining bytes as uint64 so an oversized or corrupt length is a clean
	// short-read, never a panic. uvarint leaves r.n <= len(r.b).
	if ln > uint64(len(r.b)-r.n) {
		r.err = errors.New("binwire: short read (bytes)")
		return nil
	}
	out := make([]byte, ln)
	copy(out, r.b[r.n:r.n+int(ln)])
	r.n += int(ln)
	return out
}

// str reads a length-prefixed string with a SINGLE allocation+copy. (The prior
// string(r.bytesCopy()) paid two: bytesCopy's make+copy, then string()'s second
// alloc+copy.) Per-field strings are decoded for every record on both the
// DB-writer and alerts consumers, so halving the string allocs cuts the hub's
// GC/alloc load (its dominant cost).
func (r *recReader) str() string {
	ln := r.uvarint()
	if r.err != nil || ln == 0 {
		return ""
	}
	if ln > uint64(len(r.b)-r.n) {
		r.err = errors.New("binwire: short read (bytes)")
		return ""
	}
	s := string(r.b[r.n : r.n+int(ln)]) // string() copies the bytes — one alloc+copy
	r.n += int(ln)
	return s
}

func (r *recReader) json() json.RawMessage {
	b := r.bytesCopy()
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

func (r *recReader) strSlice() []string {
	count := r.uvarint()
	if r.err != nil || count == 0 {
		return nil
	}
	// Cap the pre-allocation to the bytes left: each element needs at least a
	// 1-byte length prefix, so the slice can never hold more than that many
	// entries. A lying count can therefore not force a huge allocation; the
	// per-element str() short-read terminates the loop on a genuine overrun.
	prealloc := count
	if rem := uint64(len(r.b) - r.n); prealloc > rem {
		prealloc = rem
	}
	out := make([]string, 0, prealloc)
	for i := uint64(0); i < count && r.err == nil; i++ {
		out = append(out, r.str())
	}
	return out
}

func (r *recReader) body() audit.Body {
	if r.err != nil {
		return audit.EmptyBody()
	}
	read := audit.ReadBodyBinary
	if r.skipBody {
		read = audit.ReadBodyBinaryMeta // zero-copy: skip the inline content
	}
	b, n, err := read(r.b[r.n:])
	if err != nil {
		r.err = err
		return audit.EmptyBody()
	}
	r.n += n
	return b
}

// Pointer-boxing helpers — the consumer struct uses pointer-for-NULL, so a present
// field is decoded into a freshly-allocated pointer (nil ⟺ absent ⟺ SQL NULL).
func pStr(s string) *string   { return &s }
func pInt(i int) *int         { return &i }
func pI64(i int64) *int64     { return &i }
func pF64(f float64) *float64 { return &f }
func pBool(b bool) *bool      { return &b }
