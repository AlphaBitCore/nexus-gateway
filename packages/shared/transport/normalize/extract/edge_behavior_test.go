package extract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// Accumulator: invalid-path failure modes surface as named errors so a
// malformed patch stream degrades the frame (lower coverage confidence)
// instead of silently corrupting the accumulated document.

func TestApplyJSON_PathThroughNilIntermediate_Errors(t *testing.T) {
	acc := NewJSONPatchAccumulator()
	if err := acc.ApplyJSON([]byte(`{"p":"/a","o":"add","v":null}`)); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	err := acc.ApplyJSON([]byte(`{"p":"/a/b","o":"add","v":1}`))
	if err == nil || !strings.Contains(err.Error(), "nil at intermediate") {
		t.Fatalf("descending through a null leaf must error with the nil-intermediate failure mode, got %v", err)
	}
	// The document survives: the null leaf is still addressable.
	if v, ok := acc.State()["a"]; !ok || v != nil {
		t.Fatalf("state[a] = %v, %v — failed patch must not corrupt prior state", v, ok)
	}
}

func TestApplyJSON_PathThroughNumberIntermediate_Errors(t *testing.T) {
	acc := NewJSONPatchAccumulator()
	if err := acc.ApplyJSON([]byte(`{"p":"/a","o":"add","v":5}`)); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	err := acc.ApplyJSON([]byte(`{"p":"/a/b","o":"add","v":1}`))
	if err == nil || !strings.Contains(err.Error(), "unexpected type") {
		t.Fatalf("descending through a numeric leaf must error with the unexpected-type failure mode, got %v", err)
	}
}

// Detector rubric: scoreDetectorSignals is the shared Tier-2 confidence
// rubric for non-JSON detectors. Pin its four boundary behaviors —
// they decide whether a detection clears the registry's 0.7 threshold.

func TestScoreDetectorSignals_RubricBoundaries(t *testing.T) {
	cases := []struct {
		name string
		spec detectorSignalSpec
		want float64
	}{
		{
			// Detectors that cannot enumerate required fields
			// (batchexecute) get the full required weight: a recognised
			// envelope alone must clear the 0.7 registry threshold.
			name: "no required fields gets full required weight",
			spec: detectorSignalSpec{},
			want: 0.90,
		},
		{
			// Range maximum: full required + capped bonus = exactly 1.00.
			name: "bonus capped at 0.10 yields the 1.00 ceiling",
			spec: detectorSignalSpec{requiredSeen: 1, requiredTotal: 1, bonusSeen: 40},
			want: 1.00,
		},
		{
			// The bonus cap binds: 0.60 + 0.15 + capped 0.10 = 0.85
			// (40 bonuses would otherwise add 1.00).
			name: "bonus cap binds below the ceiling",
			spec: detectorSignalSpec{requiredSeen: 1, requiredTotal: 2, bonusSeen: 40},
			want: 0.85,
		},
		{
			// Range minimum: zero required + full unknown drift =
			// exactly 0.50 — "right format, drifted fields" rows stay
			// distinguishable from outright misses (which score 0).
			name: "total unknown drift bottoms at 0.50",
			spec: detectorSignalSpec{requiredSeen: 0, requiredTotal: 2, unknownSeen: 10, observedTotal: 10},
			want: 0.50,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scoreDetectorSignals(tc.spec)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("score = %v, want %v", got, tc.want)
			}
		})
	}
}

// Connect-RPC request decode: a truncated capture (valid leading
// message field, then a corrupt tag byte) keeps the messages decoded
// before the corruption instead of dropping the row.

func TestConnectRPCDecodeRequest_TruncatedTagKeepsDecodedMessages(t *testing.T) {
	body := buildGetChatReqWire([]struct {
		role string
		text string
	}{{"user", "hello from a truncated capture"}}, "")
	// An 0x80 continuation byte with nothing after it is an incomplete
	// varint tag: protowire.ConsumeTag returns n < 0.
	body = append(body, 0x80)

	det, ok := ConnectRPCProtobufDetector{}.Decode(body, "request")
	if !ok {
		t.Fatal("Decode must still succeed on the bytes before the corruption")
	}
	if len(det.MessageContents) != 1 || det.MessageContents[0] != "hello from a truncated capture" {
		t.Fatalf("messages = %v, want the one message decoded before the corrupt tag", det.MessageContents)
	}
}

// BatchExecute: the form-encoded request marker can open the body
// (no preceding parameter), and the anti-XSSI response prefix is a
// hard requirement — a JSON array without it is some other producer.

func TestBatchExecuteLooksLike_FReqAtBodyStart(t *testing.T) {
	if !(BatchExecuteDetector{}).LooksLike([]byte(`f.req=%5B%5D&at=tok`)) {
		t.Fatal("body opening with f.req= must be recognised")
	}
}

func TestBatchExecuteDecodeResponse_MissingAntiXSSIPrefixRefused(t *testing.T) {
	det, ok := BatchExecuteDetector{}.decodeResponse([]byte(`[["wrb.fr","x","[]"]]`))
	if ok {
		t.Fatal("response without the )]}' anti-XSSI prefix must be refused")
	}
	if det.Confidence != 0 {
		t.Fatalf("confidence = %v, want 0 on refusal", det.Confidence)
	}
}

// scanForLongestText accepts caller-supplied raw fragments; malformed
// elements (empty slice, unterminated string) are skipped without
// panicking and the longest valid string still wins.

func TestScanForLongestText_SkipsMalformedElements(t *testing.T) {
	got := scanForLongestText([]json.RawMessage{
		json.RawMessage(``),
		json.RawMessage(`"unterminated string fragment`),
		json.RawMessage(`["nested",["deep",["the actual assistant answer text"]]]`),
	})
	if got != "the actual assistant answer text" {
		t.Fatalf("longest text = %q, want the valid nested string", got)
	}
}

// extractMessageContent: a string-shaped spec must not stringify object
// content — the message scores as content-missing rather than emitting
// a garbage projection.

func TestScoreChatSpec_StringShapeRejectsObjectContent(t *testing.T) {
	spec := ChatSpec{
		ID:          "test-flat-string",
		Locator:     "messages",
		RolePath:    "role",
		ContentPath: "content",
		Shape:       ContentShapeString,
	}
	d := ScoreChatSpec([]byte(`{"messages":[{"role":"user","content":{"nested":"object"}}]}`), spec)
	if len(d.MessageContents) != 1 || d.MessageContents[0] != "" {
		t.Fatalf("contents = %v, want one empty entry (object content not extractable as string)", d.MessageContents)
	}
	// Locator 0.4 + role 0.3 + content 0 — the missing content is
	// visible in the score.
	if math.Abs(d.Confidence-0.7) > 1e-9 {
		t.Fatalf("confidence = %v, want 0.7", d.Confidence)
	}
}

// WalkSSE: a single line beyond MaxSSEScanLine (8 MiB) surfaces the
// scanner error instead of silently truncating the stream — the caller
// records the row as failed rather than as a clean partial parse.

func TestWalkSSE_OversizedLineSurfacesError(t *testing.T) {
	raw := append([]byte("data: "), bytes.Repeat([]byte("x"), MaxSSEScanLine+1)...)
	err := WalkSSE(raw, func(string, string) error { return nil })
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
}
