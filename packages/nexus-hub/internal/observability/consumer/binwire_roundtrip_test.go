package consumer

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// TestBinwireRoundTripEquivalentToJSON is the load-bearing correctness gate for
// the binary audit wire: decoding the binary form of a message MUST produce the
// same consumer struct the JSON form produces — same scalar values, same
// pointer-for-NULL presence, and byte-identical persisted body columns. Any
// field-id ↔ struct-field mismatch in the ~100-field codec surfaces here.
func TestBinwireRoundTripEquivalentToJSON(t *testing.T) {
	for _, tc := range binwireRoundTripCases() {
		t.Run(tc.name, func(t *testing.T) {
			// JSON path: marshal producer struct, unmarshal into consumer struct.
			data, err := json.Marshal(&tc.msg)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var jsonDec TrafficEventMessage
			if err := json.Unmarshal(data, &jsonDec); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			// Binary path: append one record, decode it.
			binDec, err := decodeBinaryRecord(tc.msg.AppendBinary(nil))
			if err != nil {
				t.Fatalf("decodeBinaryRecord: %v", err)
			}
			assertConsumerEquivalent(t, jsonDec, binDec)
		})
	}
}

// assertConsumerEquivalent compares two decoded consumer messages: bodies via the
// persisted column form (the JSON path carries base64 in InlineBytes, the binary
// path the raw frame, so a raw DeepEqual of Body is wrong — the DB-bound output is
// what must match), then DeepEqual on everything else with the bodies zeroed.
func assertConsumerEquivalent(t *testing.T, a, b TrafficEventMessage) {
	t.Helper()
	assertBodyColumnEqual(t, "request", a.RequestBody, b.RequestBody)
	assertBodyColumnEqual(t, "response", a.ResponseBody, b.ResponseBody)
	a.RequestBody, b.RequestBody = audit.Body{}, audit.Body{}
	a.ResponseBody, b.ResponseBody = audit.Body{}, audit.Body{}
	// Collapse opaque-JSON fields to their persisted form: the Hub inserts every
	// json.RawMessage column via nullableJSON, which maps both nil and the literal
	// "null" to SQL NULL. The JSON path yields RawMessage("null") for a no-omitempty
	// nil map (Identity); the binary path simply omits it (nil). Both persist NULL,
	// so normalize null/empty → nil before comparing to assert DB-equivalence.
	normalizeOpaqueJSON(&a)
	normalizeOpaqueJSON(&b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-body fields differ:\n json=%+v\n bin =%+v", a, b)
	}
}

// normalizeOpaqueJSON nils every json.RawMessage field whose content is empty or
// the literal "null" — the equivalence class nullableJSON collapses at insert.
func normalizeOpaqueJSON(m *TrafficEventMessage) {
	rawType := reflect.TypeOf(json.RawMessage(nil))
	v := reflect.ValueOf(m).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Type() != rawType || !f.CanSet() {
			continue
		}
		rm, _ := f.Interface().(json.RawMessage)
		if len(rm) == 0 || string(rm) == "null" {
			f.Set(reflect.Zero(rawType))
		}
	}
}

func assertBodyColumnEqual(t *testing.T, label string, a, b audit.Body) {
	t.Helper()
	if a.Kind != b.Kind {
		t.Fatalf("%s body kind: json=%q bin=%q", label, a.Kind, b.Kind)
	}
	ap, ae := a.ColumnPayload()
	bp, be := b.ColumnPayload()
	if ae != be {
		t.Fatalf("%s body column encoding: json=%q bin=%q", label, ae, be)
	}
	if !bytes.Equal(ap, bp) {
		t.Fatalf("%s body column payload differs: json=%d bytes bin=%d bytes", label, len(ap), len(bp))
	}
	// And the original bytes must reconstruct identically from each column form.
	if !bytes.Equal(audit.DecodeBodyForColumn(ap, ae), audit.DecodeBodyForColumn(bp, be)) {
		t.Fatalf("%s body decoded content differs", label)
	}
}

type binwireCase struct {
	name string
	msg  mq.TrafficEventMessage
}

func binwireRoundTripCases() []binwireCase {
	ts := time.Unix(1_700_000_000, 123_000_000).UTC() // ms precision survives JSON
	bigBody := bytes.Repeat([]byte(`{"k":"v","n":42}`), 4096)
	sp := func(s string) *string { return &s }
	pi := func(i int) *int { return &i }
	pi64 := func(i int64) *int64 { return &i }
	pf := func(f float64) *float64 { return &f }

	return []binwireCase{
		{
			name: "sparse_minimal", // only the always-on fields → all pointers nil
			msg: mq.TrafficEventMessage{
				ID:        "evt-1",
				Source:    "ai-gateway",
				Timestamp: ts,
				LatencyMs: 0, // present 0 → consumer *int(0), NOT nil (no omitempty)
			},
		},
		{
			name: "full_inline_s2",
			msg: mq.TrafficEventMessage{
				ID:                     "evt-2",
				Source:                 "ai-gateway",
				Timestamp:              ts,
				LatencyMs:              123,
				TraceID:                "trace-xyz",
				SourceIP:               "10.0.0.1",
				Method:                 "POST",
				Path:                   "/v1/chat/completions",
				StatusCode:             200,
				EndpointType:           "chat",
				IngressFormat:          "openai",
				ProviderID:             "prov-1",
				ModelID:                "gpt-4o",
				PromptTokens:           1500,
				CompletionTokens:       256,
				TotalTokens:            1756,
				ReasoningTokens:        64,
				ReasoningCostUsd:       0.0012,
				EstimatedCostUsd:       0.0345,
				CacheStatus:            "MISS",
				GatewayCacheL2EntryKey: "idx:abc123",
				OriginTZ:               sp("America/New_York"),
				ErrorCode:              sp("RATE_LIMITED"),
				ComplianceTags:         []string{"pii", "secrets"},
				PassthroughFlags:       []string{"bypassHooks"},
				PassthroughReason:      "emergency",
				GatewayCacheSavingsUsd: pf(0.5),
				CacheCreationTokens:    pi64(10),
				CacheReadTokens:        pi64(20),
				NormalizedStripCount:   pi(3),
				UpstreamTtfbMs:         pi(45),
				AttestationVerified:    true,
				AttestationAgentID:     "agent-7",
				EmbeddingModelID:       "text-embedding-3",
				Identity:               map[string]any{"sub": "user-1", "tier": "pro"},
				LatencyBreakdown:       map[string]int{"upstream": 100, "hooks": 5},
				RoutingTrace:           json.RawMessage(`{"rule":"r1"}`),
				Details:                json.RawMessage(`{"note":"x"}`),
				RequestNormalized:      json.RawMessage(`{"messages":[]}`),
				RequestBlockingRule:    rawPtr(`{"pack":"pii","rule":"email"}`),
				NormalizeVersion:       "v3",
				CredentialID:           "cred-1",
				ThingID:                "thing-1",
				SourceProcess:          "ai-gateway",
				Action:                 "traffic",
				RequestBody:            mustS2Body(bigBody),
				ResponseBody:           audit.NewInlineBody([]byte(`{"choices":[{"message":{"content":"hi"}}]}`), 41, false, "application/json"),
			},
		},
		{
			name: "binary_and_text_bodies",
			msg: mq.TrafficEventMessage{
				ID:           "evt-3",
				Source:       "compliance-proxy",
				Timestamp:    ts,
				LatencyMs:    7,
				RequestBody:  audit.NewInlineBody([]byte{0xff, 0x00, 0x1b, 0x80, 0x7f}, 5, false, "application/octet-stream"),
				ResponseBody: audit.NewInlineBody([]byte("event: delta\ndata: \x1b[36mhi\n\n"), 27, true, "text/event-stream"),
			},
		},
		{
			name: "spill_body",
			msg: mq.TrafficEventMessage{
				ID:        "evt-4",
				Source:    "agent",
				Timestamp: ts,
				LatencyMs: 99,
				RequestBody: audit.NewSpillBody(&audit.SpillRef{
					Backend: "s3", Key: "2026/req.bin", Size: 9_000_000, SHA256: "deadbeef", ContentType: "application/json",
				}, 9_000_000, true, "application/json"),
			},
		},
	}
}

func rawPtr(s string) *json.RawMessage {
	r := json.RawMessage(s)
	return &r
}

// mustS2Body builds an inline body forced to the s2 codec so the round-trip
// exercises the raw-frame (binary) vs base64 (json) column-payload divergence.
func mustS2Body(b []byte) audit.Body {
	audit.SetInlineCompression(true, 1024, 3)
	return audit.Body{
		Kind:        audit.BodyInline,
		Encoding:    audit.EncodingS2,
		InlineBytes: b,
		SizeBytes:   int64(len(b)),
		ContentType: "application/json",
	}
}
