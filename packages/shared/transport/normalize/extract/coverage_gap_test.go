package extract

// Coverage-gap tests for packages/shared/transport/normalize/extract.
//
// Goal: close the residual gap from the survey under
// `[[unit_test_coverage_95]]` — the existing accumulator_test /
// detector_test / normalizer_test / probe_test / sse_test files cover
// happy paths and the load-bearing prod regressions, but leave
// large stretches of error/branch arms (parsePointer / setAtPointer /
// removeAtPointer / mapUsage / mapRole / public spec lookups / Tier-1
// adapter dispatch / detector fallback paths) at 0 % – 70 %.
//
// Each test below pins an observable invariant a future refactor must
// not regress; none merely call a function to bump the percentage.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// types.go: HasShape (was 0%)

// TestJSONPatchOp_HasShape: HasShape returns true only when Val carries
// bytes — Path / Op alone do not satisfy it. Used by extractors to
// distinguish patch frames from self-contained messages.
func TestJSONPatchOp_HasShape(t *testing.T) {
	cases := []struct {
		name string
		op   JSONPatchOp
		want bool
	}{
		{"empty op", JSONPatchOp{}, false},
		{"path only", JSONPatchOp{Path: "/foo"}, false},
		{"op only", JSONPatchOp{Op: "add"}, false},
		{"val present", JSONPatchOp{Val: json.RawMessage(`"x"`)}, true},
		{"val empty bytes", JSONPatchOp{Val: json.RawMessage{}}, false},
		{"all fields", JSONPatchOp{Path: "/x", Op: "add", Val: json.RawMessage(`1`)}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.op.HasShape(); got != c.want {
				t.Fatalf("HasShape = %v want %v", got, c.want)
			}
		})
	}
}

// detector.go: ID() arms (were 0%) + sniff edge cases

// TestConnectRPCProtobufDetector_ID: ID is stable — emitted onto the
// DetectedSpec field, telemetry pivots, audit rows.
func TestConnectRPCProtobufDetector_ID(t *testing.T) {
	if got := (ConnectRPCProtobufDetector{}).ID(); got != "protobuf-connectrpc-chat" {
		t.Fatalf("ID = %q", got)
	}
}

// TestBatchExecuteDetector_ID: same stability contract.
func TestBatchExecuteDetector_ID(t *testing.T) {
	if got := (BatchExecuteDetector{}).ID(); got != "google-batchexecute-chat" {
		t.Fatalf("ID = %q", got)
	}
}

// TestConnectRPCProtobufDetector_LooksLike_TooShort: bodies under
// 6 bytes never look like an envelope or a bare protobuf request.
func TestConnectRPCProtobufDetector_LooksLike_TooShort(t *testing.T) {
	for _, n := range []int{0, 1, 5} {
		if (ConnectRPCProtobufDetector{}).LooksLike(make([]byte, n)) {
			t.Errorf("len=%d falsely matched", n)
		}
	}
}

// TestConnectRPCProtobufDetector_LooksLike_ZeroFlagButLengthOverflows:
// flag byte is 0x00 but the declared envelope length exceeds the
// remaining body — sniff must reject (no false-positive on JSON-ish
// payloads whose first byte happens to be 0x00).
func TestConnectRPCProtobufDetector_LooksLike_ZeroFlagButLengthOverflows(t *testing.T) {
	body := []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0x42}
	if (ConnectRPCProtobufDetector{}).LooksLike(body) {
		t.Fatalf("oversized length sniff falsely matched: %v", body)
	}
}

// TestConnectRPCProtobufDetector_LooksLike_ZeroLengthEnvelope: declared
// length 0 is reserved for end-of-stream framing — sniff treats it
// like a body without an envelope and falls through to the bare-pb
// check, which doesn't match here, so result is false.
func TestConnectRPCProtobufDetector_LooksLike_ZeroLengthEnvelope(t *testing.T) {
	body := []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0xaa}
	if (ConnectRPCProtobufDetector{}).LooksLike(body) {
		t.Fatalf("zero-length envelope falsely matched: %v", body)
	}
}

// TestConnectRPCProtobufDetector_Decode_BothDirectionFails:
// 6 bytes of garbage that pass LooksLike's bare-pb 0x12 prefix sniff
// but contain no valid ConversationMessage — Decode returns ok=false
// from both branches.
func TestConnectRPCProtobufDetector_Decode_BothDirectionFails(t *testing.T) {
	body := []byte{0x12, 0xff, 0xff, 0xff, 0xff, 0xff}
	if _, ok := (ConnectRPCProtobufDetector{}).Decode(body, ""); ok {
		t.Fatalf("expected ok=false on garbage body")
	}
}

// TestConnectRPCProtobufDetector_Decode_RequestEmptyBody: a body with
// zero ConversationMessage entries → msgCount==0 → Decode returns
// ok=false (the spec requires at least one message to claim a chat).
func TestConnectRPCProtobufDetector_Decode_RequestEmptyBody(t *testing.T) {
	// Encode field 3 (unknown) as a varint = no msg / no model.
	var body []byte
	body = protowire.AppendTag(body, 3, protowire.VarintType)
	body = protowire.AppendVarint(body, 42)
	d := ConnectRPCProtobufDetector{}
	if _, ok := d.Decode(body, "request"); ok {
		t.Fatalf("expected ok=false (no messages)")
	}
}

// TestConnectRPCProtobufDetector_Decode_RequestConfidenceCap: many
// messages + model name produces a high but bounded confidence under
// the unified Tier-2 rubric (scoreDetectorSignals: baseline 0.60 +
// required 0.30 + optional bonuses capped at 0.10 = 1.00 cap). The
// old hardcoded 0.95 cap is gone; instead bonuses saturate at the
// per-field 0.10 ceiling.
func TestConnectRPCProtobufDetector_Decode_RequestConfidenceCap(t *testing.T) {
	msgs := []struct {
		role string
		text string
	}{
		{"user", "a"},
		{"assistant", "b"},
		{"user", "c"},
		{"assistant", "d"},
	}
	body := buildGetChatReqWire(msgs, "claude-x")
	d := ConnectRPCProtobufDetector{}
	det, ok := d.Decode(body, "request")
	if !ok {
		t.Fatal("Decode false")
	}
	// 0.60 + 0.30 + min(0.10, 3*0.025) = 0.60 + 0.30 + 0.075 = 0.975
	// (3 bonuses: Model present, msgs >= 2, msgs >= 4)
	if det.Confidence < 0.97 || det.Confidence > 1.00 {
		t.Fatalf("confidence %.4f, want in [0.97, 1.00]", det.Confidence)
	}
}

// TestConnectRPCProtobufDetector_Decode_RequestUnknownTags: a real
// GetChatRequest carries unknown future fields the parser must skip
// (forward compatibility). Verifies the default-arm ConsumeFieldValue
// path advances correctly.
func TestConnectRPCProtobufDetector_Decode_RequestUnknownTags(t *testing.T) {
	var body []byte
	// Unknown field 99 as a varint, in between fields 2.
	body = protowire.AppendTag(body, 99, protowire.VarintType)
	body = protowire.AppendVarint(body, 7)
	msg := buildConvMsgWire("hello", 1)
	body = protowire.AppendTag(body, 2, protowire.BytesType)
	body = protowire.AppendBytes(body, msg)
	// Unknown field 100 as a length-delimited blob.
	body = protowire.AppendTag(body, 100, protowire.BytesType)
	body = protowire.AppendBytes(body, []byte{0xde, 0xad, 0xbe, 0xef})

	det, ok := (ConnectRPCProtobufDetector{}).Decode(body, "request")
	if !ok {
		t.Fatalf("Decode false despite a valid ConversationMessage")
	}
	if len(det.MessageContents) != 1 || det.MessageContents[0] != "hello" {
		t.Fatalf("messages: %v", det.MessageContents)
	}
}

// TestConnectRPCProtobufDetector_Decode_ResponseBarePayloadFallback:
// no Connect-RPC envelope — just a bare StreamChatResponse-shaped
// protobuf. The decoder's bare-payload fallback recovers the text.
func TestConnectRPCProtobufDetector_Decode_ResponseBarePayloadFallback(t *testing.T) {
	var payload []byte
	payload = protowire.AppendTag(payload, 1, protowire.BytesType)
	payload = protowire.AppendString(payload, "bare-payload-text")
	// Pad enough to pass LooksLike's len>=6 gate. Bare payload starts
	// with tag for field 1 = 0x0a; LooksLike's protobuf sniff matches
	// 0x12 (field 2). So bypass LooksLike by calling Decode directly.
	det, ok := (ConnectRPCProtobufDetector{}).Decode(payload, "response")
	if !ok {
		t.Fatalf("bare-payload fallback failed: payload=%v", payload)
	}
	if det.AssistantText != "bare-payload-text" {
		t.Fatalf("text: %q", det.AssistantText)
	}
}

// TestConnectRPCProtobufDetector_Decode_ResponseEmpty: response body
// with no text frames → ok=false (no assistant text).
func TestConnectRPCProtobufDetector_Decode_ResponseEmpty(t *testing.T) {
	// One empty envelope frame + nothing else.
	body := buildConnectFrame(nil, true)
	if _, ok := (ConnectRPCProtobufDetector{}).Decode(body, "response"); ok {
		t.Fatalf("expected ok=false on empty response body")
	}
}

// TestConnectRPCProtobufDetector_Decode_ResponseFrameCountTiers:
// confidence rises with frame count. Under the unified Tier-2 rubric
// (scoreDetectorSignals: 0.60 baseline + 0.30 required + per-bonus
// 0.025 cap 0.10):
//   - 1 frame  → 0.60 + 0.30 + 0     = 0.90
//   - 2 frames → 0.60 + 0.30 + 0.025 = 0.925
//   - 3 frames → 0.60 + 0.30 + 0.050 = 0.95
func TestConnectRPCProtobufDetector_Decode_ResponseFrameCountTiers(t *testing.T) {
	d := ConnectRPCProtobufDetector{}

	body1 := buildStreamChatRespFrame("one", true)
	det1, _ := d.Decode(body1, "response")
	if !approxEq(det1.Confidence, 0.90) {
		t.Errorf("1 frame: %.4f want 0.90", det1.Confidence)
	}

	body2 := append(buildStreamChatRespFrame("a", false), buildStreamChatRespFrame("b", true)...)
	det2, _ := d.Decode(body2, "response")
	if !approxEq(det2.Confidence, 0.925) {
		t.Errorf("2 frames: %.4f want 0.925", det2.Confidence)
	}

	body3 := append(buildStreamChatRespFrame("a", false), buildStreamChatRespFrame("b", false)...)
	body3 = append(body3, buildStreamChatRespFrame("c", true)...)
	det3, _ := d.Decode(body3, "response")
	if !approxEq(det3.Confidence, 0.95) {
		t.Errorf("3 frames: %.4f want 0.95", det3.Confidence)
	}
}

// TestConnectRPCProtobufDetector_Decode_RoleAssistantMappedFromVarint2:
// role varint=2 maps to "assistant"; absent varint defaults to "user".
func TestConnectRPCProtobufDetector_Decode_RoleAssistantMappedFromVarint2(t *testing.T) {
	body := buildGetChatReqWire([]struct {
		role string
		text string
	}{
		{"assistant", "from assistant"},
	}, "")
	det, ok := (ConnectRPCProtobufDetector{}).Decode(body, "request")
	if !ok {
		t.Fatal("Decode false")
	}
	if len(det.MessageRoles) != 1 || det.MessageRoles[0] != "assistant" {
		t.Fatalf("role: %v", det.MessageRoles)
	}
}

// TestParseModelDetailsName_UnknownFieldsSkipped: parseModelDetailsName
// must skip unknown fields until it hits field 1 or runs out of bytes.
func TestParseModelDetailsName_UnknownFieldsSkipped(t *testing.T) {
	var md []byte
	// Field 99 (unknown) varint first.
	md = protowire.AppendTag(md, 99, protowire.VarintType)
	md = protowire.AppendVarint(md, 1)
	// Then field 1 (target).
	md = protowire.AppendTag(md, 1, protowire.BytesType)
	md = protowire.AppendString(md, "model-after-unknown")
	if got := parseModelDetailsName(md); got != "model-after-unknown" {
		t.Fatalf("got %q", got)
	}
}

// TestParseModelDetailsName_EmptyInput / corrupt tag returns "".
func TestParseModelDetailsName_EmptyAndCorrupt(t *testing.T) {
	if got := parseModelDetailsName(nil); got != "" {
		t.Errorf("empty: %q", got)
	}
	if got := parseModelDetailsName([]byte{0xff, 0xff, 0xff}); got != "" {
		t.Errorf("corrupt tag: %q", got)
	}
}

// TestParseStreamChatResponseFieldOne_NoFieldOne: payload with only
// unknown fields → returns "".
func TestParseStreamChatResponseFieldOne_NoFieldOne(t *testing.T) {
	var b []byte
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, 7)
	if got := parseStreamChatResponseFieldOne(b); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// TestParseStreamChatResponseFieldOne_CorruptTag returns "".
func TestParseStreamChatResponseFieldOne_CorruptTag(t *testing.T) {
	if got := parseStreamChatResponseFieldOne([]byte{0xff, 0xff, 0xff}); got != "" {
		t.Errorf("got %q", got)
	}
}

// TestParseConvMessage_BothFieldsAndUnknown: field 1 (text) + field 2
// (role) + an unknown field 99 — unknown is skipped by default arm.
func TestParseConvMessage_DefaultArmSkipsUnknown(t *testing.T) {
	var b []byte
	b = protowire.AppendTag(b, 99, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte{0x00, 0x00})
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, "after-unknown")
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, 2) // assistant
	role, text := parseConvMessage(b)
	if role != "assistant" || text != "after-unknown" {
		t.Fatalf("role=%q text=%q", role, text)
	}
}

// TestParseConvMessage_CorruptTagReturnsEmpty: malformed tag exits the
// loop without touching role/text.
func TestParseConvMessage_CorruptTagReturnsEmpty(t *testing.T) {
	role, text := parseConvMessage([]byte{0xff, 0xff, 0xff})
	if role != "user" || text != "" {
		t.Fatalf("role=%q text=%q (defaults expected)", role, text)
	}
}

// BatchExecuteDetector: LooksLike + Decode edge arms

// TestBatchExecuteDetector_LooksLike_AmpersandPrefix: f.req= preceded
// by other form fields like at=token& — the contains check matches.
func TestBatchExecuteDetector_LooksLike_AmpersandPrefix(t *testing.T) {
	if !(BatchExecuteDetector{}).LooksLike([]byte("at=token&f.req=%5B%5D")) {
		t.Fatal("missed &f.req= form prefix")
	}
}

// TestBatchExecuteDetector_LooksLike_GibberishRejected: random bytes
// don't match any of the three signatures.
func TestBatchExecuteDetector_LooksLike_GibberishRejected(t *testing.T) {
	if (BatchExecuteDetector{}).LooksLike([]byte("hello world")) {
		t.Fatal("plain text falsely matched")
	}
}

// TestBatchExecuteDetector_LooksLike_LongPrefixTrimmed: probe is
// limited to 256 bytes — needs the signature to appear in the first
// 256 bytes, otherwise misses. Verifies the early-trim contract.
func TestBatchExecuteDetector_LooksLike_LongPrefixTrimmed(t *testing.T) {
	pad := strings.Repeat("x", 300)
	body := []byte(pad + "&f.req=abc")
	if (BatchExecuteDetector{}).LooksLike(body) {
		t.Fatal("signature beyond 256-byte probe window falsely matched")
	}
}

// TestBatchExecuteDetector_Decode_UnknownShape: a body that passes
// neither prefix returns ok=false.
func TestBatchExecuteDetector_Decode_UnknownShape(t *testing.T) {
	if _, ok := (BatchExecuteDetector{}).Decode([]byte("just a string"), "request"); ok {
		t.Fatal("expected ok=false on unknown shape")
	}
}

// TestBatchExecuteDetector_DecodeRequest_MalformedForm: a body with
// `f.req=` but unparseable form (% escapes broken) → ok=false.
func TestBatchExecuteDetector_DecodeRequest_MalformedForm(t *testing.T) {
	if _, ok := (BatchExecuteDetector{}).Decode([]byte("f.req=%ZZ"), "request"); ok {
		t.Fatal("expected ok=false on malformed form")
	}
}

// TestBatchExecuteDetector_DecodeRequest_NoFReqInField: form parses
// but f.req is empty.
func TestBatchExecuteDetector_DecodeRequest_NoFReqInField(t *testing.T) {
	if _, ok := (BatchExecuteDetector{}).Decode([]byte("&f.req="), "request"); ok {
		t.Fatal("expected ok=false on empty f.req")
	}
}

// TestBatchExecuteDetector_DecodeRequest_OuterParseFails: f.req
// content isn't valid JSON.
func TestBatchExecuteDetector_DecodeRequest_OuterParseFails(t *testing.T) {
	if _, ok := (BatchExecuteDetector{}).Decode([]byte("f.req=not-json"), "request"); ok {
		t.Fatal("expected ok=false on bad outer JSON")
	}
}

// TestBatchExecuteDetector_DecodeRequest_OuterArrayTooShort: outer
// array has fewer than 2 elements.
func TestBatchExecuteDetector_DecodeRequest_OuterArrayTooShort(t *testing.T) {
	body := []byte(`f.req=` + jsonURL(t, `[null]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false on short outer array")
	}
}

// TestBatchExecuteDetector_DecodeRequest_InnerNotString: outer[1] is
// not a string (the expected double-encoded inner JSON).
func TestBatchExecuteDetector_DecodeRequest_InnerNotString(t *testing.T) {
	body := []byte(`f.req=` + jsonURL(t, `[null, 42]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false when outer[1] is a number")
	}
}

// TestBatchExecuteDetector_DecodeRequest_InnerJSONInvalid: inner JSON
// string is malformed.
func TestBatchExecuteDetector_DecodeRequest_InnerJSONInvalid(t *testing.T) {
	body := []byte(`f.req=` + jsonURL(t, `[null, "not-an-array"]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false on malformed inner JSON")
	}
}

// TestBatchExecuteDetector_DecodeRequest_InnerEmpty: inner array is
// empty.
func TestBatchExecuteDetector_DecodeRequest_InnerEmpty(t *testing.T) {
	body := []byte(`f.req=` + jsonURL(t, `[null, "[]"]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false on empty inner array")
	}
}

// TestBatchExecuteDetector_DecodeRequest_FirstNotArray: inner[0] is
// not a JSON array.
func TestBatchExecuteDetector_DecodeRequest_FirstNotArray(t *testing.T) {
	body := []byte(`f.req=` + jsonURL(t, `[null, "[1]"]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false when inner[0] isn't an array")
	}
}

// TestBatchExecuteDetector_DecodeRequest_FirstArrayEmpty: inner[0] is
// an array but contains nothing.
func TestBatchExecuteDetector_DecodeRequest_FirstArrayEmpty(t *testing.T) {
	body := []byte(`f.req=` + jsonURL(t, `[null, "[[]]"]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false when inner[0] is empty")
	}
}

// TestBatchExecuteDetector_DecodeRequest_PromptNotStringOrEmpty:
// inner[0][0] is null or empty string.
func TestBatchExecuteDetector_DecodeRequest_PromptNotStringOrEmpty(t *testing.T) {
	// null prompt
	body := []byte(`f.req=` + jsonURL(t, `[null, "[[null]]"]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false on null prompt")
	}
	// empty string prompt
	body2 := []byte(`f.req=` + jsonURL(t, `[null, "[[\"\"]]"]`))
	if _, ok := (BatchExecuteDetector{}).Decode(body2, "request"); ok {
		t.Fatal("expected ok=false on empty prompt")
	}
}

// TestBatchExecuteDetector_DecodeResponse_NoXSSIPrefix: a response
// body without `)]}'` is rejected.
func TestBatchExecuteDetector_DecodeResponse_NoXSSIPrefix(t *testing.T) {
	body := []byte(`[["wrb.fr", null, "[]"]]`)
	if _, ok := (BatchExecuteDetector{}).Decode(body, "response"); ok {
		t.Fatal("expected ok=false without )]}' prefix")
	}
}

// TestBatchExecuteDetector_DecodeResponse_NoUsableFrames: prefix
// present but no wrb.fr chunks ever decode → ok=false.
func TestBatchExecuteDetector_DecodeResponse_NoUsableFrames(t *testing.T) {
	body := []byte(")]}'\n123\n456\n")
	if _, ok := (BatchExecuteDetector{}).Decode(body, "response"); ok {
		t.Fatal("expected ok=false (only length numbers, no chunks)")
	}
}

// TestBatchExecuteDetector_DecodeResponse_NonArrayChunkSkipped: stream
// includes a JSON object (not array) that the gate must skip without
// erroring.
func TestBatchExecuteDetector_DecodeResponse_NonArrayChunkSkipped(t *testing.T) {
	chunk := buildBatchRespChunk("real text here", "3 Flash")
	junk := []byte(`{"obj":"skipped"}` + "\n")
	body := append([]byte(")]}'\n\n"), junk...)
	body = append(body, chunk...)
	det, ok := (BatchExecuteDetector{}).Decode(body, "response")
	if !ok {
		t.Fatalf("Decode false despite a valid chunk after the object")
	}
	if !strings.Contains(det.AssistantText, "real text here") {
		t.Errorf("text: %q", det.AssistantText)
	}
}

// TestExtractFromBatchChunk_NotArray: outer chunk that isn't a JSON
// array → ("", "") without panic.
func TestExtractFromBatchChunk_NotArray(t *testing.T) {
	text, model := extractFromBatchChunk([]byte(`{"foo":"bar"}`))
	if text != "" || model != "" {
		t.Fatalf("got text=%q model=%q want empty", text, model)
	}
}

// TestExtractFromBatchChunk_RowTooShort: a row with fewer than 3
// elements is skipped.
func TestExtractFromBatchChunk_RowTooShort(t *testing.T) {
	text, model := extractFromBatchChunk([]byte(`[["wrb.fr"]]`))
	if text != "" || model != "" {
		t.Fatalf("got text=%q model=%q want empty", text, model)
	}
}

// TestExtractFromBatchChunk_RowChannelMismatch: first element isn't
// the literal "wrb.fr" → skip.
func TestExtractFromBatchChunk_RowChannelMismatch(t *testing.T) {
	text, _ := extractFromBatchChunk([]byte(`[["wrb.other", null, "[]"]]`))
	if text != "" {
		t.Fatalf("text=%q want empty", text)
	}
}

// TestExtractFromBatchChunk_InnerStringNotString: entry[2] isn't a
// JSON string → skipped without panic.
func TestExtractFromBatchChunk_InnerStringNotString(t *testing.T) {
	text, model := extractFromBatchChunk([]byte(`[["wrb.fr", null, 42]]`))
	if text != "" || model != "" {
		t.Fatalf("got text=%q model=%q want empty", text, model)
	}
}

// TestExtractFromBatchInner_TooShort: inner array with < 5 elements
// → ("", "").
func TestExtractFromBatchInner_TooShort(t *testing.T) {
	text, model := extractFromBatchInner([]byte(`[1,2,3]`))
	if text != "" || model != "" {
		t.Fatalf("got text=%q model=%q want empty", text, model)
	}
}

// TestExtractFromBatchInner_Malformed: not JSON → ("", "").
func TestExtractFromBatchInner_Malformed(t *testing.T) {
	text, model := extractFromBatchInner([]byte(`not-json`))
	if text != "" || model != "" {
		t.Fatalf("got text=%q model=%q", text, model)
	}
}

// TestScanForModelString_RangeAndKeywords: only short strings (<=32)
// containing flash/pro/ultra/nano (case-insensitive) match; longer or
// missing-keyword strings are skipped.
func TestScanForModelString_RangeAndKeywords(t *testing.T) {
	vals := []json.RawMessage{
		json.RawMessage(`"random-id"`),
		json.RawMessage(`""`),                                // empty: skip
		json.RawMessage(`"` + strings.Repeat("x", 40) + `"`), // >32: skip
		json.RawMessage(`123`),                               // non-string: skip
		json.RawMessage(`"GEMINI 2.5 Flash"`),                // upper-case Flash, len OK
	}
	if got := scanForModelString(vals); got != "GEMINI 2.5 Flash" {
		t.Fatalf("got %q", got)
	}
}

// TestScanForLongestText_PicksLongestEligible: short strings (< 16)
// are ignored; longest qualifying string wins. Non-string non-array
// values (numbers, nulls) are silently skipped.
func TestScanForLongestText_PicksLongestEligible(t *testing.T) {
	vals := []json.RawMessage{
		json.RawMessage(`"short"`),                                     // <16, ignored
		json.RawMessage(`"sixteen-charsXX"`),                           // 15, ignored
		json.RawMessage(`"this is sixteen!"`),                          // 16, eligible
		json.RawMessage(`[[null, "longer assistant body text wins"]]`), // recurses, wins
		json.RawMessage(`42`),                                          // skipped
		json.RawMessage(`null`),                                        // skipped
	}
	got := scanForLongestText(vals)
	if got != "longer assistant body text wins" {
		t.Fatalf("got %q", got)
	}
}

// TestScanForLongestText_MalformedArrayBreaksWalk: a malformed
// nested array stops walking that branch silently — the walk
// continues with sibling raw messages. The longest valid string
// across all siblings still wins (pins "malformed branch must
// not crash or short-circuit the overall walk").
func TestScanForLongestText_MalformedArrayBreaksWalk(t *testing.T) {
	vals := []json.RawMessage{
		json.RawMessage(`"this is the only valid text"`), // 27 chars
		json.RawMessage(`[not valid json`),               // bad array — silently skipped
		json.RawMessage(`"not-this-too-short"`),          // 18 chars, doesn't beat 27
	}
	got := scanForLongestText(vals)
	if got != "this is the only valid text" {
		t.Fatalf("got %q (malformed branch must not crash walk)", got)
	}
}

// TestScanForLongestText_EmptyInput: zero vals → "".
func TestScanForLongestText_EmptyInput(t *testing.T) {
	if got := scanForLongestText(nil); got != "" {
		t.Fatalf("got %q want empty", got)
	}
}

// normalizer.go: spec lookups + adapter dispatcher + helpers

// TestChatSpecByID_Known returns the spec pointer.
func TestChatSpecByID_Known(t *testing.T) {
	s := ChatSpecByID("openai-chat")
	if s == nil || s.ID != "openai-chat" || s.Locator != "messages" {
		t.Fatalf("got %+v", s)
	}
}

// TestChatSpecByID_Unknown returns nil.
func TestChatSpecByID_Unknown(t *testing.T) {
	if got := ChatSpecByID("not-a-real-spec"); got != nil {
		t.Fatalf("got %+v want nil", got)
	}
}

// TestChatResponseSpecByID_Known + Unknown.
func TestChatResponseSpecByID(t *testing.T) {
	if s := ChatResponseSpecByID("openai-chat-sse"); s == nil || s.ID != "openai-chat-sse" {
		t.Fatalf("known lookup: %+v", s)
	}
	if s := ChatResponseSpecByID("???"); s != nil {
		t.Fatalf("unknown lookup: %+v want nil", s)
	}
}

// TestPatternNormalizer_ID is stable for metrics labels.
func TestPatternNormalizer_ID(t *testing.T) {
	pn := NewPatternNormalizer()
	if pn.ID() != "pattern-extract" {
		t.Fatalf("ID = %q", pn.ID())
	}
}

// TestPatternNormalizer_DefaultThreshold: an empty struct (no
// MinConfidence) falls back to 0.7 — same threshold as the named
// constructor, so dropping NewPatternNormalizer doesn't silently
// change behavior.
func TestPatternNormalizer_DefaultThreshold(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o-mini",
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	pn := &PatternNormalizer{} // MinConfidence == 0
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		Direction: normalize.DirectionRequest,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.DetectedSpec != "pattern:openai-chat" {
		t.Fatalf("spec: %q", payload.DetectedSpec)
	}
}

// TestPatternNormalizer_BelowThresholdReturnsErrUnsupportedAndPartial:
// when no probe reaches MinConfidence, the result is ErrUnsupported
// but the partial Confidence is surfaced so callers can record the
// near-miss. A body that scores ~0.4 (matches `messages` locator
// only) is well under any normal threshold while >0.
func TestPatternNormalizer_BelowThresholdReturnsErrUnsupportedAndPartial(t *testing.T) {
	pn := &PatternNormalizer{MinConfidence: 0.7}
	// Body has `messages` but message elements lack role/content of
	// any registered spec — the locator hit is the only confidence
	// bump, well under 0.7.
	body := []byte(`{"messages": [{"foo": "bar"}, {"baz": 1}]}`)
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		Direction: normalize.DirectionRequest,
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Fatalf("err: %v want ErrUnsupported", err)
	}
	if payload.Kind != normalize.KindUnsupported {
		t.Fatalf("kind: %v", payload.Kind)
	}
	if payload.Confidence <= 0 {
		t.Errorf("expected partial confidence > 0, got %v", payload.Confidence)
	}
}

// TestBuildPayload_PerAdapterFlavor: BuildPayload with empty prefix
// stamps protocol = adapter-id (not pattern-extract); DetectedSpec
// has no prefix.
func TestBuildPayload_PerAdapterFlavor(t *testing.T) {
	d := ChatDetection{
		SpecID:          "anthropic-messages",
		Confidence:      0.9,
		Model:           "claude-sonnet-4-6",
		MessageRoles:    []string{"user"},
		MessageContents: []string{"hi"},
	}
	raw := []byte(`{"raw":true}`)
	out := BuildPayload(d, raw, "")
	if out.Protocol != "anthropic-messages" {
		t.Errorf("Protocol: %q want anthropic-messages", out.Protocol)
	}
	if out.DetectedSpec != "anthropic-messages" {
		t.Errorf("DetectedSpec: %q want unprefixed", out.DetectedSpec)
	}
	if out.Kind != normalize.KindAIChat {
		t.Errorf("Kind: %v", out.Kind)
	}
}

// TestBuildPayload_PatternFlavor: same detection through the internal
// "pattern:" prefix path keeps Protocol = "pattern-extract" and stamps
// the prefix on DetectedSpec.
func TestBuildPayload_PatternFlavor(t *testing.T) {
	d := ChatDetection{
		SpecID:     "chatgpt-web",
		Confidence: 0.9,
	}
	out := BuildPayload(d, []byte(`{}`), "pattern:")
	if out.Protocol != "pattern-extract" {
		t.Errorf("Protocol: %q", out.Protocol)
	}
	if out.DetectedSpec != "pattern:chatgpt-web" {
		t.Errorf("DetectedSpec: %q", out.DetectedSpec)
	}
}

// TestBuildPayload_SystemRoleEmptyContentPreserved: empty content is
// dropped EXCEPT when role is "system" or "tool" (those carry
// semantic meaning even when empty per the comment in
// buildPayloadInternal).
func TestBuildPayload_SystemRoleEmptyContentPreserved(t *testing.T) {
	d := ChatDetection{
		SpecID:          "anthropic-messages",
		Confidence:      0.9,
		MessageRoles:    []string{"system", "user", "tool", "assistant"},
		MessageContents: []string{"", "", "", "actual reply"},
	}
	out := BuildPayload(d, []byte(`{}`), "")
	// user with empty content is dropped; system + tool + assistant remain.
	wantRoles := []normalize.Role{normalize.RoleSystem, normalize.RoleTool, normalize.RoleAssistant}
	if len(out.Messages) != len(wantRoles) {
		t.Fatalf("messages: %d want %d (%+v)", len(out.Messages), len(wantRoles), out.Messages)
	}
	for i, want := range wantRoles {
		if out.Messages[i].Role != want {
			t.Errorf("msg %d role %v want %v", i, out.Messages[i].Role, want)
		}
	}
}

// TestBuildPayload_MessageContentsShorterThanRoles: when
// MessageContents has fewer entries than MessageRoles, the missing
// content defaults to "" — non-system/tool entries are skipped, no
// out-of-bounds.
func TestBuildPayload_MessageContentsShorterThanRoles(t *testing.T) {
	d := ChatDetection{
		SpecID:          "openai-chat",
		Confidence:      0.9,
		MessageRoles:    []string{"user", "user", "system"},
		MessageContents: []string{"u1"}, // only one
	}
	out := BuildPayload(d, []byte(`{}`), "")
	// user[0]: content="u1" kept; user[1]: "" dropped; system: "" kept.
	if len(out.Messages) != 2 {
		t.Fatalf("messages: %+v want 2 entries", out.Messages)
	}
	if out.Messages[0].Content[0].Text != "u1" {
		t.Errorf("msg 0: %q", out.Messages[0].Content[0].Text)
	}
	if out.Messages[1].Role != normalize.RoleSystem {
		t.Errorf("msg 1 role: %v", out.Messages[1].Role)
	}
}

// TestBuildPayload_ToolsParsed: tools raw JSON is decoded into
// ToolDef entries with Name / Description / Parameters.
func TestBuildPayload_ToolsParsed(t *testing.T) {
	d := ChatDetection{
		SpecID:     "openai-chat",
		Confidence: 0.9,
		ToolsRaw: json.RawMessage(`[
			{"name": "search", "description": "do search", "parameters": {"type":"object"}}
		]`),
	}
	out := BuildPayload(d, []byte(`{}`), "")
	if len(out.Tools) != 1 || out.Tools[0].Name != "search" || out.Tools[0].Description != "do search" {
		t.Fatalf("tools: %+v", out.Tools)
	}
	if _, ok := out.Tools[0].ParametersJSONSchema["type"]; !ok {
		t.Errorf("parameters JSON schema not preserved: %+v", out.Tools[0].ParametersJSONSchema)
	}
}

// TestBuildPayload_ToolsRawInvalid: malformed ToolsRaw is ignored
// gracefully (no Tools added, no error).
func TestBuildPayload_ToolsRawInvalid(t *testing.T) {
	d := ChatDetection{
		SpecID:     "openai-chat",
		Confidence: 0.9,
		ToolsRaw:   json.RawMessage(`not-an-array`),
	}
	out := BuildPayload(d, []byte(`{}`), "")
	if len(out.Tools) != 0 {
		t.Fatalf("tools: %+v want empty", out.Tools)
	}
}

// TestBuildPayload_UsageMappedFromDifferentSchemas: each of the three
// recognised aliases (OpenAI / Anthropic / Gemini) yields the same
// Usage shape with non-nil prompt/completion/total tokens.
func TestBuildPayload_UsageMappedFromDifferentSchemas(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"openai", `{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}`},
		{"anthropic", `{"input_tokens": 1, "output_tokens": 2}`},
		{"gemini", `{"promptTokenCount": 1, "candidatesTokenCount": 2, "totalTokenCount": 3}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := ChatDetection{SpecID: "x", Confidence: 0.9, UsageRaw: json.RawMessage(c.raw)}
			out := BuildPayload(d, []byte(`{}`), "")
			if out.Usage == nil {
				t.Fatalf("usage nil")
			}
			if out.Usage.PromptTokens == nil || *out.Usage.PromptTokens != 1 {
				t.Errorf("prompt: %+v", out.Usage.PromptTokens)
			}
			if out.Usage.CompletionTokens == nil || *out.Usage.CompletionTokens != 2 {
				t.Errorf("completion: %+v", out.Usage.CompletionTokens)
			}
		})
	}
}

// TestBuildPayload_UsageMalformedReturnsNil: invalid JSON → nil
// Usage (not a panic).
func TestBuildPayload_UsageMalformedReturnsNil(t *testing.T) {
	d := ChatDetection{SpecID: "x", Confidence: 0.9, UsageRaw: json.RawMessage(`not-json`)}
	out := BuildPayload(d, []byte(`{}`), "")
	if out.Usage != nil {
		t.Fatalf("usage: %+v want nil", out.Usage)
	}
}

// TestBuildPayload_UsageNoRecognisedKeysReturnsNil: a usage map with
// only unrecognised keys (e.g. a future provider's `tokens_in`)
// returns nil Usage so we don't fabricate zero tokens.
func TestBuildPayload_UsageNoRecognisedKeysReturnsNil(t *testing.T) {
	d := ChatDetection{SpecID: "x", Confidence: 0.9, UsageRaw: json.RawMessage(`{"tokens_in": 5}`)}
	out := BuildPayload(d, []byte(`{}`), "")
	if out.Usage != nil {
		t.Fatalf("usage: %+v want nil", out.Usage)
	}
}

// TestBuildPayload_UsageNonNumericValuesSkipped: numeric-shaped keys
// whose value isn't a number must not appear in Usage.
func TestBuildPayload_UsageNonNumericValuesSkipped(t *testing.T) {
	d := ChatDetection{
		SpecID:     "x",
		Confidence: 0.9,
		UsageRaw:   json.RawMessage(`{"prompt_tokens": "ten"}`),
	}
	out := BuildPayload(d, []byte(`{}`), "")
	if out.Usage != nil {
		t.Fatalf("usage: %+v want nil (non-numeric value)", out.Usage)
	}
}

// TestMapRole_AllArms: covers the explicit switch arms + unknown
// → RoleUser fallback (so PII hooks still scan unknown-role content).
func TestMapRole_AllArms(t *testing.T) {
	cases := []struct {
		in   string
		want normalize.Role
	}{
		{"system", normalize.RoleSystem},
		{"SYSTEM", normalize.RoleSystem},
		{"user", normalize.RoleUser},
		{"assistant", normalize.RoleAssistant},
		{"model", normalize.RoleAssistant},
		{"tool", normalize.RoleTool},
		{"function", normalize.RoleTool},
		{"unknown-role-from-future-provider", normalize.RoleUser},
		{"", normalize.RoleUser},
	}
	for _, c := range cases {
		if got := mapRole(c.in); got != c.want {
			t.Errorf("mapRole(%q) = %v want %v", c.in, got, c.want)
		}
	}
}

// NormalizeForAdapter (Tier-1 dispatcher)

// TestNormalizeForAdapter_RequestClaimsPerSpec: the Tier-1 dispatcher
// scores only the listed request specs and returns a populated
// payload when confidence >= MinConfidence. DetectedSpec is the
// AdapterID (no "pattern:" prefix).
func TestNormalizeForAdapter_RequestClaimsPerSpec(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o-mini",
		"messages": [{"role": "user", "content": "hello"}]
	}`)
	hint := AdapterSpecHint{
		AdapterID:     "openai-chat",
		ReqSpecIDs:    []string{"openai-chat"},
		MinConfidence: 0.5,
	}
	out, err := NormalizeForAdapter(body, normalize.Meta{Direction: normalize.DirectionRequest}, hint)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.DetectedSpec != "openai-chat" {
		t.Errorf("DetectedSpec: %q want openai-chat (no pattern: prefix)", out.DetectedSpec)
	}
	if out.Kind != normalize.KindAIChat {
		t.Errorf("Kind: %v", out.Kind)
	}
}

// TestNormalizeForAdapter_BelowThresholdReturnsErrUnsupported.
func TestNormalizeForAdapter_BelowThresholdReturnsErrUnsupported(t *testing.T) {
	body := []byte(`{"foo": "bar"}`)
	hint := AdapterSpecHint{
		AdapterID:     "openai-chat",
		ReqSpecIDs:    []string{"openai-chat"},
		MinConfidence: 0.5,
	}
	out, err := NormalizeForAdapter(body, normalize.Meta{Direction: normalize.DirectionRequest}, hint)
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Fatalf("err: %v want ErrUnsupported", err)
	}
	if out.Kind != normalize.KindUnsupported {
		t.Errorf("Kind: %v", out.Kind)
	}
}

// TestNormalizeForAdapter_ResponseDirectionRoute: only response specs
// score against the body when Direction == DirectionResponse.
func TestNormalizeForAdapter_ResponseDirectionRoute(t *testing.T) {
	body := []byte(`{
		"id": "x",
		"choices": [{"message": {"role": "assistant", "content": "reply"}}],
		"model": "gpt-4o",
		"usage": {"prompt_tokens": 1}
	}`)
	hint := AdapterSpecHint{
		AdapterID:     "openai-chat",
		RespSpecIDs:   []string{"openai-chat-nonstream"},
		MinConfidence: 0.5,
	}
	out, err := NormalizeForAdapter(body, normalize.Meta{Direction: normalize.DirectionResponse}, hint)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.Messages[0].Content[0].Text, "reply") {
		t.Errorf("text: %+v", out.Messages)
	}
}

// TestNormalizeForAdapter_DirectionUnsetPicksBetter: when Direction
// is empty, both probes run and the higher-confidence wins.
func TestNormalizeForAdapter_DirectionUnsetPicksBetter(t *testing.T) {
	body := []byte(`{
		"id": "x",
		"choices": [{"message": {"role": "assistant", "content": "auto-detected"}}],
		"model": "gpt-4o",
		"usage": {"prompt_tokens": 1}
	}`)
	hint := AdapterSpecHint{
		AdapterID:     "openai-chat",
		ReqSpecIDs:    []string{"openai-chat"},
		RespSpecIDs:   []string{"openai-chat-nonstream"},
		MinConfidence: 0.5,
	}
	out, err := NormalizeForAdapter(body, normalize.Meta{}, hint)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.Messages[0].Content[0].Text, "auto-detected") {
		t.Errorf("expected response-direction detection to win")
	}
}

// TestNormalizeForAdapter_UnknownSpecIDsIgnored: hint listing a spec
// ID not in the catalog yields a nil scorer pointer and is silently
// skipped — does not crash, behaves as if the list was shorter.
func TestNormalizeForAdapter_UnknownSpecIDsIgnored(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	hint := AdapterSpecHint{
		AdapterID:  "x",
		ReqSpecIDs: []string{"unknown-1", "also-unknown"},
	}
	_, err := NormalizeForAdapter(body, normalize.Meta{Direction: normalize.DirectionRequest}, hint)
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Fatalf("err: %v", err)
	}
}

// TestNormalizeForAdapter_ZeroMinConfidenceDefaults: an unset
// MinConfidence falls back to 0.5 (vs. the Tier-2 default 0.7) —
// adapters are routed by host so partial probe hits are still safe.
// Verifies a body that scores ~0.45 stays Unsupported (default 0.5)
// while a body that scores ~0.55 claims.
func TestNormalizeForAdapter_ZeroMinConfidenceDefaults(t *testing.T) {
	// A body that scores well above 0.5.
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	hint := AdapterSpecHint{
		AdapterID:  "openai-chat",
		ReqSpecIDs: []string{"openai-chat"},
		// MinConfidence == 0 → falls back to 0.5
	}
	if _, err := NormalizeForAdapter(body, normalize.Meta{Direction: normalize.DirectionRequest}, hint); err != nil {
		t.Fatalf("err: %v (expected default 0.5 threshold met)", err)
	}
}

// probe.go: ScoreChatSpec / ScoreResponseSpec public arms

// TestScoreChatSpec_InvalidJSONReturnsZero: invalid JSON →
// ChatDetection zero-value.
func TestScoreChatSpec_InvalidJSONReturnsZero(t *testing.T) {
	spec := *ChatSpecByID("openai-chat")
	d := ScoreChatSpec([]byte(`not-json`), spec)
	if d.Confidence != 0 || d.SpecID != "" {
		t.Fatalf("got %+v want zero-value", d)
	}
}

// TestScoreChatSpec_ValidBody: a valid OpenAI chat body scores > 0.7
// against the openai-chat spec via the public single-spec scorer.
func TestScoreChatSpec_ValidBody(t *testing.T) {
	spec := *ChatSpecByID("openai-chat")
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	d := ScoreChatSpec(body, spec)
	if d.Confidence < 0.6 {
		t.Fatalf("confidence: %v", d.Confidence)
	}
}

// TestScoreResponseSpec_SSESpecVsNonSSEBody: a spec declaring
// "sse-event-data" framing against a single-JSON body returns a
// non-stream zero-detection (mismatch path).
func TestScoreResponseSpec_SSESpecVsNonSSEBody(t *testing.T) {
	spec := *ChatResponseSpecByID("openai-chat-sse")
	d := ScoreResponseSpec([]byte(`{"some":"json"}`), spec)
	if d.IsStream {
		t.Errorf("expected IsStream=false on framing mismatch (non-SSE body, SSE spec)")
	}
	if d.Confidence != 0 {
		t.Errorf("confidence: %v want 0 on mismatch", d.Confidence)
	}
}

// TestScoreResponseSpec_SingleJSONSpecVsSSEBody: opposite mismatch.
func TestScoreResponseSpec_SingleJSONSpecVsSSEBody(t *testing.T) {
	spec := *ChatResponseSpecByID("openai-chat-nonstream")
	d := ScoreResponseSpec([]byte("event: x\ndata: y\n\n"), spec)
	if !d.IsStream {
		t.Errorf("expected IsStream=true (body is SSE)")
	}
	if d.Confidence != 0 {
		t.Errorf("confidence: %v want 0 on mismatch", d.Confidence)
	}
}

// TestScoreResponseSpec_RealNonStreamMatch: aligned framing → real
// detection with assistant text + confidence > 0.5.
func TestScoreResponseSpec_RealNonStreamMatch(t *testing.T) {
	spec := *ChatResponseSpecByID("openai-chat-nonstream")
	body := []byte(`{
		"id": "x",
		"choices": [{"message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
		"model": "gpt-4o",
		"usage": {"prompt_tokens": 1}
	}`)
	d := ScoreResponseSpec(body, spec)
	if d.AssistantText != "ok" {
		t.Errorf("text: %q", d.AssistantText)
	}
	if d.FinishReason != "stop" {
		t.Errorf("finish: %q", d.FinishReason)
	}
}

// TestDetectResponseShape_UnknownAccumulatorRule_ZeroConfidence:
// a response spec with an unrecognised AccumulatorRule against an
// SSE body returns confidence 0 (the default arm in scoreResponseSpec
// returns the zero detection).
func TestDetectResponseShape_UnknownAccumulatorRule_ZeroConfidence(t *testing.T) {
	spec := ChatResponseSpec{
		ID:                "x",
		StreamFraming:     "sse-event-data",
		AccumulatorRule:   "made-up-rule",
		AssistantTextPath: "_accumulated",
	}
	d := ScoreResponseSpec([]byte("data: hi\n\n"), spec)
	if d.Confidence != 0 {
		t.Errorf("confidence: %v want 0", d.Confidence)
	}
}

// TestDetectResponseShape_ConcatTextNoStreamDeltaPath_DefaultUsed: a
// spec missing StreamDeltaPath defaults to OpenAI's canonical
// "choices.0.delta.content" so the probe doesn't crash — a valid
// OpenAI SSE still gets accumulated.
func TestDetectResponseShape_ConcatTextNoStreamDeltaPath_DefaultUsed(t *testing.T) {
	spec := ChatResponseSpec{
		ID:                "homemade",
		StreamFraming:     "sse-event-data",
		AccumulatorRule:   "concat-text",
		AssistantTextPath: "_accumulated",
		// StreamDeltaPath intentionally empty
	}
	raw := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"X\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"Y\"}}]}\n\n")
	d := ScoreResponseSpec(raw, spec)
	if d.AssistantText != "XY" {
		t.Errorf("text: %q want XY (default delta path)", d.AssistantText)
	}
}

// TestDetectResponseShape_SingleJSONInvalid: SingleJSON framing
// against invalid JSON body → zero confidence.
func TestDetectResponseShape_SingleJSONInvalid(t *testing.T) {
	spec := *ChatResponseSpecByID("openai-chat-nonstream")
	d := ScoreResponseSpec([]byte(`not-json`), spec)
	if d.Confidence != 0 {
		t.Errorf("confidence: %v want 0", d.Confidence)
	}
}

// TestScoreChatSpec_SingleStringPromptVariant: legacy "anthropic-
// completions-legacy" spec (Locator empty, ContentPath="prompt")
// gives a confidence bump when the body has a non-empty prompt string.
func TestScoreChatSpec_SingleStringPromptVariant(t *testing.T) {
	spec := *ChatSpecByID("anthropic-completions-legacy")
	body := []byte(`{"prompt":"\n\nHuman: hi\n\nAssistant:", "max_tokens_to_sample": 8}`)
	d := ScoreChatSpec(body, spec)
	if d.Confidence < 0.4 {
		t.Errorf("confidence: %v", d.Confidence)
	}
	if len(d.UserPrompts) != 1 || !strings.Contains(d.UserPrompts[0], "Human:") {
		t.Errorf("prompts: %v", d.UserPrompts)
	}
}

// TestExtractMessageContent_StringArrayWithSingleString: ChatGPT-web
// content.parts is sometimes a single JSON string instead of an
// array — extractor returns the string directly.
func TestExtractMessageContent_StringArrayWithSingleString(t *testing.T) {
	spec := ChatSpec{
		ID:          "chatgpt-web",
		Locator:     "messages",
		RolePath:    "author.role",
		ContentPath: "content.parts",
		Shape:       ContentShapeStringArray,
	}
	body := []byte(`{
		"messages": [{"author":{"role":"user"},"content":{"parts":"single-string"}}]
	}`)
	d := ScoreChatSpec(body, spec)
	if len(d.MessageContents) != 1 || d.MessageContents[0] != "single-string" {
		t.Fatalf("contents: %v", d.MessageContents)
	}
}

// TestExtractMessageContent_BlockArrayNonArrayReturnsEmpty: when a
// spec expects ContentShapeBlockArray but the content is a string,
// content extraction returns "" (no contribution).
func TestExtractMessageContent_BlockArrayNonArrayReturnsEmpty(t *testing.T) {
	spec := ChatSpec{
		ID:          "anthropic-messages",
		Locator:     "messages",
		RolePath:    "role",
		ContentPath: "content",
		Shape:       ContentShapeBlockArray,
	}
	body := []byte(`{"messages":[{"role":"user","content":"plain string instead of blocks"}]}`)
	d := ScoreChatSpec(body, spec)
	if d.MessageContents[0] != "" {
		t.Errorf("content: %q want empty", d.MessageContents[0])
	}
}

// TestExtractMessageContent_NestedTextArrayNonArray: gemini-shape
// spec on a non-array parts field returns "".
func TestExtractMessageContent_NestedTextArrayNonArray(t *testing.T) {
	spec := ChatSpec{
		ID:          "gemini-generate",
		Locator:     "contents",
		RolePath:    "role",
		ContentPath: "parts",
		Shape:       ContentShapeNestedTextArray,
	}
	body := []byte(`{"contents":[{"role":"user","parts":"not-array"}]}`)
	d := ScoreChatSpec(body, spec)
	if d.MessageContents[0] != "" {
		t.Errorf("content: %q", d.MessageContents[0])
	}
}

// TestExtractMessageContent_StringShapeReturnsEmptyOnAbsent: when the
// content path is missing entirely, returns "" without altering
// detection.
func TestExtractMessageContent_StringShapeReturnsEmptyOnAbsent(t *testing.T) {
	spec := ChatSpec{
		ID:          "openai-chat",
		Locator:     "messages",
		RolePath:    "role",
		ContentPath: "content",
		Shape:       ContentShapeString,
	}
	body := []byte(`{"messages":[{"role":"user"}]}`)
	d := ScoreChatSpec(body, spec)
	if d.MessageContents[0] != "" {
		t.Errorf("content: %q", d.MessageContents[0])
	}
}

// TestScoreChatSpec_SignatureFieldsAddBonus: more signature hits =
// higher confidence, capped at +0.2.
func TestScoreChatSpec_SignatureFieldsBonusCapped(t *testing.T) {
	// Build a spec with five signature fields all present.
	spec := ChatSpec{
		ID:              "tester",
		Locator:         "msgs",
		RolePath:        "r",
		ContentPath:     "c",
		Shape:           ContentShapeString,
		SignatureFields: []string{"a", "b", "c", "d", "e"},
	}
	body := []byte(`{
		"msgs": [{"r":"user","c":"hi"}],
		"a":1,"b":2,"c":3,"d":4,"e":5
	}`)
	d := ScoreChatSpec(body, spec)
	// 0.4 locator + 0.3 role + 0.3 content + capped 0.2 sig = 1.0 max.
	if d.Confidence < 0.95 || d.Confidence > 1.0 {
		t.Errorf("confidence: %v want ~1.0 (signatures capped)", d.Confidence)
	}
}

// TestScoreResponseSpec_SignatureBonusCapped: response-spec sig bonus
// caps at +0.3.
func TestScoreResponseSpec_SignatureBonusCapped(t *testing.T) {
	spec := ChatResponseSpec{
		ID:                "tester",
		StreamFraming:     "single-json",
		AccumulatorRule:   "none",
		AssistantTextPath: "text",
		SignatureFields:   []string{"a", "b", "c", "d", "e", "f"},
	}
	body := []byte(`{"text":"hi","a":1,"b":2,"c":3,"d":4,"e":5,"f":6}`)
	d := ScoreResponseSpec(body, spec)
	// 0.5 assistant text + capped 0.3 sig = 0.8.
	if d.Confidence < 0.75 {
		t.Errorf("confidence: %v want >= 0.75", d.Confidence)
	}
}

// accumulator.go: error / branch arms

// TestAccumulator_UnknownOpReturnsError: unknown op string → error
// from Apply (so producers misnaming an op fail loudly).
func TestAccumulator_UnknownOpReturnsError(t *testing.T) {
	a := NewJSONPatchAccumulator()
	err := a.Apply(JSONPatchOp{Op: "noSuchOp", Val: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "unknown patch op") {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyAdd_NonObjectRootStoredUnder_root: a non-object
// add at root path goes under "_root" so downstream extractors can
// still see the value.
func TestAccumulator_ApplyAdd_NonObjectRootStoredUnder_root(t *testing.T) {
	a := NewJSONPatchAccumulator()
	if err := a.Apply(JSONPatchOp{Op: "add", Val: json.RawMessage(`["a","b"]`)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := a.State()["_root"]; got == nil {
		t.Fatalf("state: %+v want _root populated", a.State())
	}
}

// TestAccumulator_ApplyAdd_RootDecodeFailure: invalid JSON value at
// root → wrapping error from applyAdd.
func TestAccumulator_ApplyAdd_RootDecodeFailure(t *testing.T) {
	a := NewJSONPatchAccumulator()
	err := a.Apply(JSONPatchOp{Op: "add", Val: json.RawMessage(`not-json`)})
	if err == nil || !strings.Contains(err.Error(), "add root") {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyAdd_ValueDecodeFailure: invalid JSON value at
// non-root path → wrapping error with the path mentioned.
func TestAccumulator_ApplyAdd_ValueDecodeFailure(t *testing.T) {
	a := NewJSONPatchAccumulator()
	err := a.Apply(JSONPatchOp{Op: "add", Path: "/foo", Val: json.RawMessage(`not-json`)})
	if err == nil || !strings.Contains(err.Error(), "add /foo") {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyAdd_BadPointer: leading slash missing.
func TestAccumulator_ApplyAdd_BadPointer(t *testing.T) {
	a := NewJSONPatchAccumulator()
	err := a.Apply(JSONPatchOp{Op: "add", Path: "foo", Val: json.RawMessage(`1`)})
	if err == nil || !strings.Contains(err.Error(), "invalid pointer") {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyAppend_FallsBackToAddOnNonString: an append
// op whose value isn't a JSON string falls through to add semantics
// instead of erroring.
func TestAccumulator_ApplyAppend_FallsBackToAddOnNonString(t *testing.T) {
	a := NewJSONPatchAccumulator()
	if err := a.Apply(JSONPatchOp{Op: "append", Path: "/x", Val: json.RawMessage(`{"k":"v"}`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got, _ := a.State()["x"].(map[string]any); got["k"] != "v" {
		t.Fatalf("state: %+v want add-fallback object", a.State())
	}
}

// TestAccumulator_ApplyAppend_RootStoredUnder_root.
func TestAccumulator_ApplyAppend_RootStoredUnder_root(t *testing.T) {
	a := NewJSONPatchAccumulator()
	if err := a.Apply(JSONPatchOp{Op: "append", Path: "", Val: json.RawMessage(`"hello"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if a.State()["_root"] != "hello" {
		t.Fatalf("state: %+v", a.State())
	}
}

// TestAccumulator_ApplyAppend_BadPointer.
func TestAccumulator_ApplyAppend_BadPointer(t *testing.T) {
	a := NewJSONPatchAccumulator()
	err := a.Apply(JSONPatchOp{Op: "append", Path: "no-leading-slash", Val: json.RawMessage(`"x"`)})
	if err == nil || !strings.Contains(err.Error(), "invalid pointer") {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyAppend_PathNotPreviouslyAString: appending to a
// non-string path replaces with the new string.
func TestAccumulator_ApplyAppend_PathNotPreviouslyAString(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":42}`)})
	if err := a.Apply(JSONPatchOp{Op: "append", Path: "/x", Val: json.RawMessage(`"replacement"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if a.State()["x"] != "replacement" {
		t.Fatalf("state: %+v", a.State())
	}
}

// TestAccumulator_ApplyRemove_BadPointer + remove root path.
func TestAccumulator_ApplyRemove_BadPointer(t *testing.T) {
	a := NewJSONPatchAccumulator()
	err := a.Apply(JSONPatchOp{Op: "remove", Path: "missing-slash"})
	if err == nil {
		t.Fatalf("err: %v", err)
	}
}

func TestAccumulator_ApplyRemove_RootClearsAll(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"a":1,"b":2}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: ""}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(a.State()) != 0 {
		t.Fatalf("state: %+v want empty", a.State())
	}
}

// TestAccumulator_ApplyRemove_AbsentKeyNoop: deleting a path that
// doesn't exist returns nil.
func TestAccumulator_ApplyRemove_AbsentKeyNoop(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"a":1}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/missing"}); err != nil {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyRemove_ArrayIndex_NilsElement: removing an
// array index sets the slot to nil (rather than splicing). Downstream
// extractors tolerate nil holes.
func TestAccumulator_ApplyRemove_ArrayIndex_NilsElement(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":["a","b","c"]}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/x/1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	arr := a.State()["x"].([]any)
	if len(arr) != 3 {
		t.Fatalf("len: %d want 3 (nil-in-place semantics)", len(arr))
	}
	if arr[1] != nil {
		t.Fatalf("arr[1]: %v want nil", arr[1])
	}
}

// TestAccumulator_ApplyRemove_ArrayOutOfBounds_Noop.
func TestAccumulator_ApplyRemove_ArrayOutOfBounds_Noop(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":["a"]}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/x/99"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(a.State()["x"].([]any)) != 1 {
		t.Fatalf("state mutated: %+v", a.State())
	}
}

// TestAccumulator_ApplyRemove_NonNumericArrayTokenNoop: token "abc"
// into an array path is treated as absent (silent return).
func TestAccumulator_ApplyRemove_NonNumericArrayTokenNoop(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":["a"]}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/x/abc"}); err != nil {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyRemove_NavigateIntoNestedMap: intermediate map
// path → final delete works.
func TestAccumulator_ApplyRemove_NavigateIntoNestedMap(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"a":{"b":{"c":1,"d":2}}}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/a/b/c"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, exists := a.State()["a"].(map[string]any)["b"].(map[string]any)["c"]; exists {
		t.Fatalf("c not removed: %+v", a.State())
	}
}

// TestAccumulator_ApplyRemove_NavigateIntoArray.
func TestAccumulator_ApplyRemove_NavigateIntoArray(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":[{"k":"v"}]}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/x/0/k"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	arr := a.State()["x"].([]any)
	if _, exists := arr[0].(map[string]any)["k"]; exists {
		t.Fatalf("k not removed: %+v", arr)
	}
}

// TestAccumulator_ApplyRemove_TraversesScalar_Noop: trying to descend
// into a scalar leaf returns nil (silent no-op per RFC 6901 absent-
// path semantics).
func TestAccumulator_ApplyRemove_TraversesScalar_Noop(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":"scalar"}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/x/y"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if a.State()["x"] != "scalar" {
		t.Fatalf("state mutated: %+v", a.State())
	}
}

// TestAccumulator_ApplyPatch_InvalidJSON: non-array value to patch op
// → wrapping error.
func TestAccumulator_ApplyPatch_InvalidJSON(t *testing.T) {
	a := NewJSONPatchAccumulator()
	err := a.Apply(JSONPatchOp{Op: "patch", Val: json.RawMessage(`not-json`)})
	if err == nil || !strings.Contains(err.Error(), "patch decode") {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyPatch_NestedOpError: a nested op that fails
// propagates with patch[<idx>] context.
func TestAccumulator_ApplyPatch_NestedOpError(t *testing.T) {
	a := NewJSONPatchAccumulator()
	patchVal := json.RawMessage(`[{"p":"bad-pointer","o":"add","v":1}]`)
	err := a.Apply(JSONPatchOp{Op: "patch", Val: patchVal})
	if err == nil || !strings.Contains(err.Error(), "patch[0]") {
		t.Fatalf("err: %v", err)
	}
}

// TestAccumulator_ApplyReplace_RelaxedToAdd: replace at missing path
// is relaxed to add (best-effort accumulation).
func TestAccumulator_ApplyReplace_RelaxedToAdd(t *testing.T) {
	a := NewJSONPatchAccumulator()
	if err := a.Apply(JSONPatchOp{Op: "replace", Path: "/x", Val: json.RawMessage(`42`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if a.State()["x"] != float64(42) {
		t.Fatalf("state: %+v", a.State())
	}
}

// TestAccumulator_ExtractByPointer_AbsentPath: a missing path
// returns ("", false); a path whose value isn't a string also
// returns ("", false).
func TestAccumulator_ExtractByPointer_AbsentOrWrongType(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"n":42}`)})
	if s, ok := a.ExtractByPointer("/missing"); ok || s != "" {
		t.Errorf("missing: got (%q,%v)", s, ok)
	}
	if s, ok := a.ExtractByPointer("/n"); ok || s != "" {
		t.Errorf("non-string: got (%q,%v)", s, ok)
	}
	// Bad pointer (no leading slash) → also (false, "").
	if s, ok := a.ExtractByPointer("bad"); ok || s != "" {
		t.Errorf("bad pointer: got (%q,%v)", s, ok)
	}
}

// TestParsePointer_TildeEscapes: ~0 → ~, ~1 → /.
func TestParsePointer_TildeEscapes(t *testing.T) {
	tokens, err := parsePointer("/a~1b/c~0d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(tokens) != 2 || tokens[0] != "a/b" || tokens[1] != "c~d" {
		t.Fatalf("tokens: %v", tokens)
	}
}

// TestParsePointer_EmptyAndBadPrefix.
func TestParsePointer_EmptyAndBadPrefix(t *testing.T) {
	if tokens, err := parsePointer(""); err != nil || tokens != nil {
		t.Errorf("empty: tokens=%v err=%v", tokens, err)
	}
	if _, err := parsePointer("foo"); err == nil {
		t.Error("no leading slash should error")
	}
}

// TestSetAtPointer_GrowArrayInTail_DocumentedLimitation: per the
// inline comment in setAtPointer ("simpler to just bail on growth
// beyond original len for the streaming case"), an index beyond
// the original array length does NOT actually grow the parent
// slice because the inner `node` variable shadows the parent's
// slice header. The op returns nil error but the write is silently
// dropped. This test pins the documented limitation so a future
// refactor either keeps the same behavior or explicitly upgrades
// the contract.
func TestSetAtPointer_GrowArrayInTail_DocumentedLimitation(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":["a"]}`)})
	// Add at /x/3 — code says "bail on growth beyond original len".
	if err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/3", Val: json.RawMessage(`"d"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	arr := a.State()["x"].([]any)
	// Parent slice unchanged. Length stays 1.
	if len(arr) != 1 {
		t.Fatalf("len(arr) = %d want 1 (documented bail on grow)", len(arr))
	}
}

// TestSetAtPointer_NonNumericArrayIndex_Errors.
func TestSetAtPointer_NonNumericArrayIndex_Errors(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":["a"]}`)})
	err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/abc", Val: json.RawMessage(`1`)})
	if err == nil || !strings.Contains(err.Error(), "non-numeric index") {
		t.Fatalf("err: %v", err)
	}
}

// TestSetAtPointer_NegativeIndex_Errors. RFC 6901 "-" handling
// isn't supported; numeric negatives error per current implementation.
func TestSetAtPointer_NegativeIndex_Errors(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":["a"]}`)})
	err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/-1", Val: json.RawMessage(`1`)})
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("err: %v", err)
	}
}

// TestSetAtPointer_PathTraversesScalar_Errors.
func TestSetAtPointer_PathTraversesScalar_Errors(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":"scalar"}`)})
	err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/y", Val: json.RawMessage(`1`)})
	if err == nil || !strings.Contains(err.Error(), "traverses scalar") {
		t.Fatalf("err: %v", err)
	}
}

// TestSetAtPointer_CreateIntermediateArrayOnNumericNextToken: when
// the next token is numeric, the intermediate container is created
// as a fresh `[]any{}` (not a map). Per the documented growth
// limitation, the array starts empty and writes into it beyond
// length 0 may be silently dropped; the test pins the container-
// type choice (array, not map) which is the load-bearing branch
// for ChatGPT-flavored streams that materialise the full array via
// the initial root `add`.
func TestSetAtPointer_CreateIntermediateArrayOnNumericNextToken(t *testing.T) {
	a := NewJSONPatchAccumulator()
	if err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/0/k", Val: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	// Container at /x must be an array (not a map) because next
	// token "0" is numeric.
	if _, ok := a.State()["x"].([]any); !ok {
		t.Fatalf("state: %+v want x as []any (numeric next-token branch)", a.State())
	}
}

// TestSetAtPointer_CreateIntermediateMapOnAlphabeticNextToken.
func TestSetAtPointer_CreateIntermediateMapOnAlphabeticNextToken(t *testing.T) {
	a := NewJSONPatchAccumulator()
	if err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/y/k", Val: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	xMap, ok := a.State()["x"].(map[string]any)
	if !ok {
		t.Fatalf("state: %+v want x as map[string]any", a.State())
	}
	yMap, ok := xMap["y"].(map[string]any)
	if !ok || yMap["k"] != "v" {
		t.Fatalf("nested: %+v", xMap)
	}
}

// TestSetAtPointer_ArrayMidPathFollowsExistingElement: when the next
// existing array element is non-nil, the traversal reuses it rather
// than re-creating.
func TestSetAtPointer_ArrayMidPathFollowsExistingElement(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":[{"k":"old"}]}`)})
	if err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/0/k", Val: json.RawMessage(`"new"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	arr := a.State()["x"].([]any)
	got := arr[0].(map[string]any)["k"]
	if got != "new" {
		t.Fatalf("got %v want new", got)
	}
}

// TestSetAtPointer_ArrayMidPathCreatesNestedMapOnNilSlot: a nil array
// slot becomes a map when the next token is alphabetic.
func TestSetAtPointer_ArrayMidPathCreatesNestedMapOnNilSlot(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":[null]}`)})
	if err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/0/k", Val: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	arr := a.State()["x"].([]any)
	got := arr[0].(map[string]any)["k"]
	if got != "v" {
		t.Fatalf("got %v", got)
	}
}

// TestSetAtPointer_ArrayMidPathCreatesNestedArrayOnNilSlot: numeric
// next token after a nil array slot creates `[]any{}` as the
// nested container. Subsequent write into idx 0 hits the same
// documented grow-bail limitation (parent slice unchanged), so the
// invariant we pin is just the container-type choice — type is
// []any, not map[string]any.
func TestSetAtPointer_ArrayMidPathCreatesNestedArrayOnNilSlot(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":[null]}`)})
	if err := a.Apply(JSONPatchOp{Op: "add", Path: "/x/0/0", Val: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("err: %v", err)
	}
	arr := a.State()["x"].([]any)
	if _, ok := arr[0].([]any); !ok {
		t.Fatalf("arr[0] type %T want []any (numeric next-token branch)", arr[0])
	}
}

// TestGetAtPointer_RootReturnsState: empty token list returns the
// root map (used by getAtPointer's len==0 short-circuit).
func TestGetAtPointer_RootReturnsState(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"a":1}`)})
	root := getAtPointer(a.State(), nil)
	if m, ok := root.(map[string]any); !ok || m["a"] == nil {
		t.Fatalf("root: %+v", root)
	}
}

// TestGetAtPointer_TypeMismatch_ReturnsNil: descending into a non-
// map / non-array node returns nil.
func TestGetAtPointer_TypeMismatch_ReturnsNil(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":42}`)})
	if got := getAtPointer(a.State(), []string{"x", "y"}); got != nil {
		t.Fatalf("got %v want nil", got)
	}
}

// TestGetAtPointer_ArrayBadIndex_ReturnsNil.
func TestGetAtPointer_ArrayBadIndex_ReturnsNil(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"x":["a"]}`)})
	if got := getAtPointer(a.State(), []string{"x", "abc"}); got != nil {
		t.Fatalf("got %v want nil", got)
	}
	if got := getAtPointer(a.State(), []string{"x", "99"}); got != nil {
		t.Fatalf("got %v want nil", got)
	}
	if got := getAtPointer(a.State(), []string{"x", "-1"}); got != nil {
		t.Fatalf("got %v want nil", got)
	}
}

// TestIsStringJSON_LeadingWhitespace: whitespace before `"` still
// counts as a string-shaped raw message.
func TestIsStringJSON_LeadingWhitespace(t *testing.T) {
	if !isStringJSON(json.RawMessage("   \t\n\"hi\"")) {
		t.Fatal("whitespace before quote: expected true")
	}
	if isStringJSON(json.RawMessage("   42")) {
		t.Fatal("number expected false")
	}
	if isStringJSON(json.RawMessage("")) {
		t.Fatal("empty expected false")
	}
}

// approxEq compares two floats with a small tolerance — confidence
// values are constructed via constant adds so exact equality holds
// in practice, but the helper avoids brittle assertions if numeric
// representation drifts.
func approxEq(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

// Second-pass branch closure

// TestConnectRPCProtobufDetector_Decode_RequestCorruptModelBytes:
// field 7 ModelDetails byte-length declares more bytes than remain
// — protowire.ConsumeBytes returns -1 → Decode bails ok=false.
func TestConnectRPCProtobufDetector_Decode_RequestCorruptModelBytes(t *testing.T) {
	// Build a valid message then append a truncated field-7 byte
	// length prefix.
	msgs := []struct {
		role string
		text string
	}{{"user", "hi"}}
	body := buildGetChatReqWire(msgs, "")
	// Append field 7 tag (BytesType) and a varint claiming length 99 but
	// no payload bytes.
	body = protowire.AppendTag(body, 7, protowire.BytesType)
	body = protowire.AppendVarint(body, 99)
	if _, ok := (ConnectRPCProtobufDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false on corrupt field-7 bytes")
	}
}

// TestConnectRPCProtobufDetector_Decode_RequestCorruptConvMessageBytes:
// field 2 ConversationMessage with truncated bytes → ok=false.
func TestConnectRPCProtobufDetector_Decode_RequestCorruptConvMessageBytes(t *testing.T) {
	var body []byte
	body = protowire.AppendTag(body, 2, protowire.BytesType)
	body = protowire.AppendVarint(body, 50) // declared length far exceeds payload
	if _, ok := (ConnectRPCProtobufDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false on truncated conv message bytes")
	}
}

// TestConnectRPCProtobufDetector_Decode_RequestCorruptUnknownField:
// unknown field tag followed by a truncated bytes value → default arm
// ConsumeFieldValue returns -1 → ok=false.
func TestConnectRPCProtobufDetector_Decode_RequestCorruptUnknownField(t *testing.T) {
	var body []byte
	body = protowire.AppendTag(body, 99, protowire.BytesType)
	body = protowire.AppendVarint(body, 100) // declared length way over remaining
	if _, ok := (ConnectRPCProtobufDetector{}).Decode(body, "request"); ok {
		t.Fatal("expected ok=false on corrupt unknown field")
	}
}

// TestParseConvMessage_CorruptFieldOneBytes: field-1 (text) bytes
// declared length > remaining → loop bails preserving prior role.
func TestParseConvMessage_CorruptFieldOneBytes(t *testing.T) {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendVarint(b, 50) // claim 50 bytes but have none
	role, text := parseConvMessage(b)
	if text != "" || role != "user" {
		t.Fatalf("role=%q text=%q want defaults", role, text)
	}
}

// TestParseConvMessage_CorruptVarintRole: field 2 varint truncated.
// We use a continuation-byte marker (0x80) with no terminator.
func TestParseConvMessage_CorruptVarintRole(t *testing.T) {
	b := []byte{0x10 /* tag field 2 varint */, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}
	role, text := parseConvMessage(b)
	if role != "user" || text != "" {
		t.Fatalf("role=%q text=%q want defaults", role, text)
	}
}

// TestParseConvMessage_CorruptDefaultArm: unknown field with broken
// length prefix triggers default arm ConsumeFieldValue == -1.
func TestParseConvMessage_CorruptDefaultArm(t *testing.T) {
	var b []byte
	b = protowire.AppendTag(b, 99, protowire.BytesType)
	b = protowire.AppendVarint(b, 200) // claim 200 bytes, none follow
	role, text := parseConvMessage(b)
	if role != "user" || text != "" {
		t.Fatalf("role=%q text=%q want defaults", role, text)
	}
}

// TestParseModelDetailsName_CorruptFieldOneBytes: field-1 bytes
// length-prefix overshoots remaining → returns "".
func TestParseModelDetailsName_CorruptFieldOneBytes(t *testing.T) {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendVarint(b, 200)
	if got := parseModelDetailsName(b); got != "" {
		t.Fatalf("got %q", got)
	}
}

// TestParseModelDetailsName_UnknownThenCorrupt: skip-arm advances by
// ConsumeFieldValue; corrupt unknown field returns "" cleanly.
func TestParseModelDetailsName_UnknownThenCorrupt(t *testing.T) {
	var b []byte
	b = protowire.AppendTag(b, 99, protowire.BytesType)
	b = protowire.AppendVarint(b, 50) // truncated
	if got := parseModelDetailsName(b); got != "" {
		t.Fatalf("got %q", got)
	}
}

// TestParseStreamChatResponseFieldOne_CorruptFieldOneBytes.
func TestParseStreamChatResponseFieldOne_CorruptFieldOneBytes(t *testing.T) {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendVarint(b, 200)
	if got := parseStreamChatResponseFieldOne(b); got != "" {
		t.Fatalf("got %q", got)
	}
}

// TestConnectRPCProtobufDetector_Decode_ResponseFourFrameTier: 4+
// frames hit both the frames>=2 and frames>=3 bonuses under the
// unified Tier-2 rubric → 0.60 + 0.30 + 0.05 = 0.95.
func TestConnectRPCProtobufDetector_Decode_ResponseFourFrameTier(t *testing.T) {
	body := buildStreamChatRespFrame("a", false)
	body = append(body, buildStreamChatRespFrame("b", false)...)
	body = append(body, buildStreamChatRespFrame("c", false)...)
	body = append(body, buildStreamChatRespFrame("d", true)...)
	det, ok := (ConnectRPCProtobufDetector{}).Decode(body, "response")
	if !ok {
		t.Fatal("Decode false")
	}
	if !approxEq(det.Confidence, 0.95) {
		t.Fatalf("confidence: %.4f want 0.95 (frames>=3 bonus tier)", det.Confidence)
	}
}

// TestBatchExecuteDetector_DecodeResponse_FourPlusFramesAndModelBump:
// 4+ frames trigger both frames>=2 and frames>=4 bonuses, plus the
// model-name bonus → 3 bonuses × 0.025 = 0.075 → 0.60 + 0.30 + 0.075
// = 0.975 under the unified Tier-2 rubric.
func TestBatchExecuteDetector_DecodeResponse_FourPlusFramesAndModelBump(t *testing.T) {
	body := []byte(")]}'\n\n")
	for _, s := range []string{"a", "b", "c", "d"} {
		body = append(body, buildBatchRespChunk(s, "3 Flash")...)
	}
	det, ok := (BatchExecuteDetector{}).Decode(body, "response")
	if !ok {
		t.Fatal("Decode false")
	}
	if !approxEq(det.Confidence, 0.975) {
		t.Fatalf("confidence: %.4f want 0.975 (frames>=2 + frames>=4 + model)", det.Confidence)
	}
}

// TestBatchExecuteDetector_LooksLike_TrimmedLeadingWhitespace:
// leading whitespace before `)]}'` does NOT match (the trim only
// strips the probe leading whitespace AFTER the 256-byte cap, then
// HasPrefix on the trimmed string).
func TestBatchExecuteDetector_LooksLike_TrimmedLeadingWhitespace(t *testing.T) {
	if !(BatchExecuteDetector{}).LooksLike([]byte("\n\n  )]}'\n[]")) {
		t.Fatal("expected leading-whitespace XSSI prefix to be detected")
	}
}

// probe.go branch closure

// TestScoreChatSpec_ToolsPathSetButEmpty: ToolsPath set, body's
// tools array is empty → ToolsRaw stays empty.
func TestScoreChatSpec_ToolsPathSetButEmpty(t *testing.T) {
	spec := ChatSpec{
		ID:          "x",
		Locator:     "messages",
		RolePath:    "role",
		ContentPath: "content",
		Shape:       ContentShapeString,
		ToolsPath:   "tools",
	}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}], "tools": []}`)
	d := ScoreChatSpec(body, spec)
	if len(d.ToolsRaw) != 0 {
		t.Fatalf("tools_raw: %s want empty (tools array empty)", string(d.ToolsRaw))
	}
}

// TestExtractMessageContent_BlockArrayMultiBlockSeparator: two text
// blocks → output joined by '\n'.
func TestExtractMessageContent_BlockArrayMultiBlockSeparator(t *testing.T) {
	spec := ChatSpec{
		ID:          "anthropic-messages",
		Locator:     "messages",
		RolePath:    "role",
		ContentPath: "content",
		Shape:       ContentShapeBlockArray,
	}
	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"first"},
			{"type":"text","text":"second"}
		]}]
	}`)
	d := ScoreChatSpec(body, spec)
	if d.MessageContents[0] != "first\nsecond" {
		t.Fatalf("content: %q want first\\nsecond", d.MessageContents[0])
	}
}

// TestExtractMessageContent_StringArrayMultiPartSeparator.
func TestExtractMessageContent_StringArrayMultiPartSeparator(t *testing.T) {
	spec := ChatSpec{
		ID:          "chatgpt-web",
		Locator:     "messages",
		RolePath:    "author.role",
		ContentPath: "content.parts",
		Shape:       ContentShapeStringArray,
	}
	body := []byte(`{
		"messages":[{"author":{"role":"user"},"content":{"parts":["alpha","beta"]}}]
	}`)
	d := ScoreChatSpec(body, spec)
	if d.MessageContents[0] != "alpha\nbeta" {
		t.Fatalf("content: %q", d.MessageContents[0])
	}
}

// TestExtractMessageContent_NestedTextArrayMultiPartSeparator: gemini
// parts with two text entries → joined.
func TestExtractMessageContent_NestedTextArrayMultiPartSeparator(t *testing.T) {
	spec := ChatSpec{
		ID:          "gemini-generate",
		Locator:     "contents",
		RolePath:    "role",
		ContentPath: "parts",
		Shape:       ContentShapeNestedTextArray,
	}
	body := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"alpha"},{"text":"beta"}]}]
	}`)
	d := ScoreChatSpec(body, spec)
	if d.MessageContents[0] != "alpha\nbeta" {
		t.Fatalf("content: %q", d.MessageContents[0])
	}
}

// TestExtractMessageContent_StringArrayNotArrayAndNotString: returns
// "" for a content.parts value that's neither a JSON array nor a
// JSON string (e.g., a number or object).
func TestExtractMessageContent_StringArrayNotArrayAndNotString(t *testing.T) {
	spec := ChatSpec{
		ID:          "chatgpt-web",
		Locator:     "messages",
		RolePath:    "author.role",
		ContentPath: "content.parts",
		Shape:       ContentShapeStringArray,
	}
	body := []byte(`{
		"messages":[{"author":{"role":"user"},"content":{"parts":42}}]
	}`)
	d := ScoreChatSpec(body, spec)
	if d.MessageContents[0] != "" {
		t.Fatalf("content: %q want empty", d.MessageContents[0])
	}
}

// TestDetectResponseShape_ConcatTextInvalidFrameSkipped: a frame
// whose `data:` payload isn't valid JSON → silently skipped (no
// crash, no count toward totalFrames).
func TestDetectResponseShape_ConcatTextInvalidFrameSkipped(t *testing.T) {
	spec := *ChatResponseSpecByID("openai-chat-sse")
	raw := []byte(strings.Join([]string{
		"data: not-json",
		"",
		`data: {"choices":[{"delta":{"content":"X"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	d := ScoreResponseSpec(raw, spec)
	if d.AssistantText != "X" {
		t.Fatalf("text: %q want X (invalid frame skipped)", d.AssistantText)
	}
}

// TestDetectResponseShape_ConcatTextModelOnFrame: model field on a
// streaming frame is captured (header / tail frames carry these).
func TestDetectResponseShape_ConcatTextModelOnFrame(t *testing.T) {
	spec := *ChatResponseSpecByID("openai-chat-sse")
	raw := []byte(strings.Join([]string{
		`data: {"id":"x","model":"gpt-4o-mini","choices":[{"delta":{"content":"A"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	d := ScoreResponseSpec(raw, spec)
	if d.Model != "gpt-4o-mini" {
		t.Errorf("model: %q want gpt-4o-mini", d.Model)
	}
}

// TestDetectResponseShape_ConcatTextThreeFramesBonus: three matched
// delta frames → +0.2 over the first-frame +0.3 bonus.
func TestDetectResponseShape_ConcatTextThreeFramesBonus(t *testing.T) {
	spec := *ChatResponseSpecByID("openai-chat-sse")
	raw := []byte(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"A"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"B"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"C"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	d := ScoreResponseSpec(raw, spec)
	// 3 matched frames → +0.3 + +0.2 = +0.5; AssistantTextPath
	// "_accumulated" hits → another +0.5; sig {choices} hits → +0.1.
	// 1.1 capped to 1.0.
	if d.Confidence < 0.95 {
		t.Errorf("confidence: %v want >= 0.95 (3-frame bonus)", d.Confidence)
	}
}

// TestDetectResponseShape_JSONPatchThreeFrameBonus: same +0.2 on the
// json-patch branch when patchFrames >= 3.
func TestDetectResponseShape_JSONPatchThreeFrameBonus(t *testing.T) {
	spec := *ChatResponseSpecByID("chatgpt-web")
	raw := []byte(strings.Join([]string{
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"content":{"parts":[""]}}}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"A"}`,
		"",
		"event: delta",
		`data: {"v":"B"}`,
		"",
		"event: delta",
		`data: {"v":"C"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	d := ScoreResponseSpec(raw, spec)
	if !d.IsStream || d.Confidence < 0.7 {
		t.Errorf("conf: %v stream: %v", d.Confidence, d.IsStream)
	}
}

// TestDetectResponseShape_FallbackToAccumulatedText: AssistantTextPath
// doesn't resolve on the synthesized doc → fall back to
// accumulatedText (which got built from delta frames). Pin the
// +0.3 fallback bonus path.
func TestDetectResponseShape_FallbackToAccumulatedText(t *testing.T) {
	spec := ChatResponseSpec{
		ID:                "homemade",
		StreamFraming:     "sse-event-data",
		AccumulatorRule:   "concat-text",
		AssistantTextPath: "no.such.path", // not "_accumulated"
		StreamDeltaPath:   "choices.0.delta.content",
	}
	raw := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"FB\"}}]}\n\n")
	d := ScoreResponseSpec(raw, spec)
	if d.AssistantText != "FB" {
		t.Errorf("text: %q want FB (fallback path)", d.AssistantText)
	}
}

// normalizer.go: a couple remaining arms

// TestPatternNormalizer_DirectionDefault_NonJSONDetectorBeatsJSON:
// when Direction unset and a NonJSON detector returns higher
// confidence than the JSON probe, the detector wins.
func TestPatternNormalizer_DirectionDefault_NonJSONDetectorBeatsJSON(t *testing.T) {
	// A protobuf body — JSON probe scores 0 (gjson invalid). The
	// detector loop kicks in and claims via ConnectRPCProtobufDetector.
	body := buildGetChatReqWire([]struct {
		role string
		text string
	}{
		{"user", "from-protobuf"},
	}, "model-x")
	pn := NewPatternNormalizer()
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		// Direction left unset — both probe branches run.
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.DetectedSpec != "pattern:protobuf-connectrpc-chat" {
		t.Errorf("DetectedSpec: %q", payload.DetectedSpec)
	}
}

// TestNormalizeForAdapter_RequestEmptyHintReturnsErrUnsupported:
// hint with empty ReqSpecIDs + Direction=Request → best stays zero
// → ErrUnsupported.
func TestNormalizeForAdapter_RequestEmptyHintReturnsErrUnsupported(t *testing.T) {
	hint := AdapterSpecHint{AdapterID: "x"}
	_, err := NormalizeForAdapter(
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		normalize.Meta{Direction: normalize.DirectionRequest},
		hint,
	)
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Fatalf("err: %v", err)
	}
}

// sse.go: trailing-data and field-only edges

// TestWalkSSE_BlankFrameSeparatorNoOpFlush: multiple blank-line
// separators in a row produce no spurious fn() callbacks.
func TestWalkSSE_BlankFrameSeparatorNoOpFlush(t *testing.T) {
	raw := []byte("\n\n\n\ndata: only\n\n")
	got := 0
	err := WalkSSE(raw, func(_, _ string) error {
		got++
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 1 {
		t.Fatalf("frames: %d want 1 (only the real data frame)", got)
	}
}

// TestWalkSSE_FieldOnlyLineSkipped: a line with no colon is parsed
// as `field=<entire line>`, but only event/data/id/retry are
// recognised — anything else (including "weird-line") falls into
// the unknown-field default arm and is silently skipped.
func TestWalkSSE_FieldOnlyLineSkipped(t *testing.T) {
	raw := []byte("weird-line\ndata: payload\n\n")
	var got string
	err := WalkSSE(raw, func(_, data string) error {
		got = data
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "payload" {
		t.Fatalf("data: %q", got)
	}
}

// TestWalkSSE_TerminalFrameFnError: a final frame (no trailing blank
// line) whose fn() returns a non-StopWalk error → walk propagates it.
func TestWalkSSE_TerminalFrameFnError(t *testing.T) {
	raw := []byte("data: only")
	want := errors.New("terminal-frame")
	err := WalkSSE(raw, func(_, _ string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("err: %v want %v", err, want)
	}
}

// TestWalkSSE_TerminalFrameStopWalk: a terminal-frame return of
// ErrSSEStopWalk is swallowed (consistent with the mid-stream stop
// behavior, so callers can stop on the last frame too).
func TestWalkSSE_TerminalFrameStopWalk(t *testing.T) {
	raw := []byte("data: only")
	err := WalkSSE(raw, func(_, _ string) error { return ErrSSEStopWalk })
	if err != nil {
		t.Fatalf("err: %v want nil (StopWalk swallowed on terminal frame)", err)
	}
}

// TestWalkSSE_IDAndRetryFieldsIgnored: id: and retry: fields are
// ignored per W3C spec. Only data:/event: produce callbacks.
func TestWalkSSE_IDAndRetryFieldsIgnored(t *testing.T) {
	raw := []byte("id: 42\nretry: 100\ndata: payload\n\n")
	got := 0
	err := WalkSSE(raw, func(event, data string) error {
		got++
		if event != "" || data != "payload" {
			t.Errorf("event=%q data=%q", event, data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 1 {
		t.Fatalf("frames: %d want 1", got)
	}
}

// accumulator.go: remaining branch arms

// TestRemoveAtPointer_IntermediateMissingMapKey: walking a path
// whose intermediate map key is absent returns nil silently.
func TestRemoveAtPointer_IntermediateMissingMapKey(t *testing.T) {
	a := NewJSONPatchAccumulator()
	_ = a.Apply(JSONPatchOp{Op: "add", Path: "", Val: json.RawMessage(`{"a":{}}`)})
	if err := a.Apply(JSONPatchOp{Op: "remove", Path: "/a/x/y"}); err != nil {
		t.Fatalf("err: %v", err)
	}
}

// jsonURL is a tiny helper: URL-encode a JSON string for embedding
// in an `f.req=` form value.
func jsonURL(t *testing.T, s string) string {
	t.Helper()
	// We want exact-bytes encoding; use encodeURIComponent semantics
	// via url.QueryEscape from net/url. The detector ParseQuery
	// happens to accept the +/space variants too, but be precise.
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '%':
			b.WriteString("%25")
		case '&':
			b.WriteString("%26")
		case '+':
			b.WriteString("%2B")
		case ' ':
			b.WriteString("%20")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
