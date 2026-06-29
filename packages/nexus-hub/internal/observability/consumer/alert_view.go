package consumer

// alert_view.go — AlertView is the narrow, compiler-enforced projection of a
// traffic record that the read-only alerts engine consumes. The producer message
// carries 102 fields; the alert aggregators read only the 22 below. Decoding the
// full TrafficEventMessage on the hot alert drain copied every string and every
// json.RawMessage side-field (RequestNormalized / ResponseNormalized can be
// kilobytes) that no aggregator ever reads.
//
// AlertView fixes that two ways:
//   - decodeBinaryRecordIntoView stores ONLY the 22 view fields and skip-advances
//     past the other 80 (no string copy, no RawMessage alloc, no pointer box).
//   - Event.Traffic is *AlertView, so an aggregator that reaches for a field
//     outside this set FAILS TO COMPILE rather than silently reading a zero value
//     off a narrowed decode. Adding a field to the alert path is therefore a
//     deliberate edit here, not an accident.
//
// Field names and json tags mirror TrafficEventMessage exactly so aggregator code
// (t.EntityID, t.RequestBody.Kind, …) is unchanged and the legacy NDJSON path can
// json.Unmarshal straight into AlertView (extra keys are ignored).

import (
	"errors"
	"time"

	json "github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// AlertView is the exact set of traffic fields the alert aggregators read. Keep
// it in lockstep with the aggregator field accesses: every field here must be
// read by at least one aggregator (or by the engine, e.g. Timestamp), and every
// field an aggregator reads must be here. The drift-guard parity test asserts the
// view decode matches the full decode for each of these fields.
type AlertView struct {
	Timestamp time.Time `json:"timestamp"`

	Source        string  `json:"source"`
	SourceIP      *string `json:"sourceIp,omitempty"`
	SourceProcess *string `json:"sourceProcess,omitempty"`

	StatusCode *int `json:"statusCode,omitempty"`
	LatencyMs  *int `json:"latencyMs,omitempty"`

	EntityType *string `json:"entityType,omitempty"`
	EntityID   *string `json:"entityId,omitempty"`

	ProviderID       *string  `json:"providerId,omitempty"`
	ModelID          *string  `json:"modelId,omitempty"`
	TotalTokens      *int     `json:"totalTokens,omitempty"`
	EstimatedCostUSD *float64 `json:"estimatedCostUsd,omitempty"`

	RoutedProviderID *string `json:"routedProviderId,omitempty"`
	RoutedModelID    *string `json:"routedModelId,omitempty"`

	RequestHookDecision   *string         `json:"requestHookDecision,omitempty"`
	ResponseHookDecision  *string         `json:"responseHookDecision,omitempty"`
	RequestHooksPipeline  json.RawMessage `json:"requestHooksPipeline,omitempty"`
	ResponseHooksPipeline json.RawMessage `json:"responseHooksPipeline,omitempty"`

	ErrorCode *string `json:"errorCode,omitempty"`

	RequestBody  audit.Body `json:"requestBody,omitempty"`
	ResponseBody audit.Body `json:"responseBody,omitempty"`

	CredentialID *string `json:"credentialId,omitempty"`
}

// decodeBinaryRecordIntoView parses one binary TLV record into the caller-provided
// AlertView (which MUST be zeroed), storing the 22 view fields and skip-advancing
// past every other field. Bodies are decoded metadata-only (zero-copy), the same
// contract the full alert decode used.
//
// Forward-compat note: unlike the full decoder (which errors on an unknown field
// id), the view decoder skips an unrecognised id as a length-prefixed byte run
// (skipField's default). This gracefully ignores a future ADDITIVE str/json field
// from a newer producer without breaking the alert path. A future TYPED field
// (varint/f64/bool/strSlice) added to the producer but not classified in skipField
// would mis-advance the cursor — but that cannot ship undetected: the always-
// present terminal body fields are view fields, so the drift-guard parity test
// (TestDecodeAlertView_ParityWithFullDecode, which populates every field) fails at
// CI on the desync. The gw/Hub coordinated deploy is the second line of defence.
func decodeBinaryRecordIntoView(v *AlertView, data []byte) error {
	r := recReader{b: data, skipBody: true}
	for r.err == nil && r.n < len(r.b) {
		id := mq.FieldID(r.uvarint())
		if r.err != nil {
			break
		}
		switch id {
		case mq.FldTimestamp:
			v.Timestamp = time.Unix(0, r.varint()).UTC()
		case mq.FldSource:
			v.Source = r.str()
		case mq.FldSourceIP:
			v.SourceIP = pStr(r.str())
		case mq.FldSourceProcess:
			v.SourceProcess = pStr(r.str())
		case mq.FldStatusCode:
			v.StatusCode = pInt(int(r.varint()))
		case mq.FldLatencyMs:
			v.LatencyMs = pInt(int(r.varint()))
		case mq.FldEntityType:
			v.EntityType = pStr(r.str())
		case mq.FldEntityID:
			v.EntityID = pStr(r.str())
		case mq.FldProviderID:
			v.ProviderID = pStr(r.str())
		case mq.FldModelID:
			v.ModelID = pStr(r.str())
		case mq.FldTotalTokens:
			v.TotalTokens = pInt(int(r.varint()))
		case mq.FldEstimatedCostUsd:
			v.EstimatedCostUSD = pF64(r.f64())
		case mq.FldRoutedProviderID:
			v.RoutedProviderID = pStr(r.str())
		case mq.FldRoutedModelID:
			v.RoutedModelID = pStr(r.str())
		case mq.FldRequestHookDecision:
			v.RequestHookDecision = pStr(r.str())
		case mq.FldResponseHookDecision:
			v.ResponseHookDecision = pStr(r.str())
		case mq.FldRequestHooksPipeline:
			v.RequestHooksPipeline = r.json()
		case mq.FldResponseHooksPipeline:
			v.ResponseHooksPipeline = r.json()
		case mq.FldErrorCode:
			v.ErrorCode = pStr(r.str())
		case mq.FldRequestBody:
			v.RequestBody = r.body()
		case mq.FldResponseBody:
			v.ResponseBody = r.body()
		case mq.FldCredentialID:
			v.CredentialID = pStr(r.str())
		default:
			r.skipField(id)
		}
	}
	return r.err
}

// skipField advances past one non-view field without copying its value. The wire
// layout is implied by the field id (the producer's AppendBinary writes no
// per-field length tag), so skipField must know each id's wire kind to advance
// correctly — str and json share the uvarint-length+bytes layout and fall through
// to the default. If a NEW field id with a varint/f64/bool/strSlice layout is
// added to the producer but not listed here, the default's byte skip would
// mis-advance; the drift-guard parity test (decode-full vs decode-view over a
// fully-populated record) catches that as a value mismatch.
func (r *recReader) skipField(id mq.FieldID) {
	switch id {
	case mq.FldPromptTokens, mq.FldCompletionTokens, mq.FldReasoningTokens,
		mq.FldCacheCreationTokens, mq.FldCacheReadTokens, mq.FldNormalizedStripCount,
		mq.FldNormalizedStripBytes, mq.FldCacheMarkerInjected, mq.FldUpstreamTtfbMs,
		mq.FldUpstreamTotalMs, mq.FldRequestHooksMs, mq.FldResponseHooksMs:
		r.varint()
	case mq.FldReasoningCostUsd, mq.FldGatewayCacheSavingsUsd, mq.FldEmbeddingCostUsd,
		mq.FldAIGuardCostUsd, mq.FldCacheWriteCostUsd, mq.FldCacheReadSavingsUsd,
		mq.FldCacheNetSavingsUsd:
		r.f64()
	case mq.FldAttestationVerified:
		r.boolean()
	case mq.FldComplianceTags, mq.FldPassthroughFlags:
		r.skipStrSlice()
	default:
		// str and json fields: a uvarint length followed by that many bytes.
		r.skipBytes()
	}
}

// skipBytes advances past a length-prefixed byte run (str or json) without the
// allocation+copy that str()/json() pay. Bounds-checked: an oversized or corrupt
// length is a clean short-read error, never a panic.
func (r *recReader) skipBytes() {
	ln := r.uvarint()
	if r.err != nil || ln == 0 {
		return
	}
	if ln > uint64(len(r.b)-r.n) {
		r.err = errors.New("binwire: short read (skip bytes)")
		return
	}
	r.n += int(ln)
}

// skipStrSlice advances past a string-slice field (uvarint count then count
// length-prefixed strings) without allocating the slice or its elements.
func (r *recReader) skipStrSlice() {
	count := r.uvarint()
	for i := uint64(0); i < count && r.err == nil; i++ {
		r.skipBytes()
	}
}
