package gemini

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// redactAll stands in for the redaction hook: it replaces every extracted
// segment with a marker, which is what a real redact pass does to the spans it
// finds.
//
// THE MARKER CARRIES ITS INDEX, and that is load-bearing. With one identical
// marker for every segment, "no original plaintext survives" is invariant under
// any PERMUTATION of Segments — a rewrite that reverses the whole list, the
// maximal violation of the pairing contract, passes. The pairing is the
// invariant this file exists to protect, so the oracle has to be able to see it:
// each slot must receive ITS OWN redaction, not merely some redaction.
func redactAll(c traffic.NormalizedContent) traffic.NormalizedContent {
	out := c
	out.Segments = make([]string, len(c.Segments))
	for i := range c.Segments {
		out.Segments[i] = redactionFor(i)
	}
	return out
}

func redactionFor(i int) string { return fmt.Sprintf("[REDACTED-%d]", i) }

func roundTripRequest(t *testing.T, body string) (string, traffic.NormalizedContent) {
	t.Helper()
	a := &Adapter{}
	ctx := context.Background()
	extracted, err := a.ExtractRequest(ctx, []byte(body), "application/json")
	if err != nil {
		t.Fatalf("ExtractRequest: %v", err)
	}
	rewritten, _, err := a.RewriteRequestBody(ctx, []byte(body), "application/json", redactAll(extracted))
	if err != nil {
		t.Fatalf("RewriteRequestBody: %v", err)
	}
	return string(rewritten), extracted
}

// TestRewriteRequest_ThinkingPartDoesNotShiftTheSlots is the leak.
//
// The extractor routes thought=true text to ReasoningSegments and off Segments;
// a rewrite that writes into every text part puts, for parts [A, thought, B],
// redact(A) in A and redact(B) into the THINKING part, then runs out of segments
// and returns — so B crosses the wire in plaintext while the pipeline records
// the request as redacted.
func TestRewriteRequest_ThinkingPartDoesNotShiftTheSlots(t *testing.T) {
	const body = `{"contents":[
		{"role":"user","parts":[{"text":"FIRST-SECRET"}]},
		{"role":"model","parts":[{"text":"INTERNAL-THOUGHT","thought":true}]},
		{"role":"user","parts":[{"text":"SECOND-SECRET"}]}
	]}`

	out, extracted := roundTripRequest(t, body)

	if len(extracted.Segments) != 2 {
		t.Fatalf("extract put %d segments on Segments, want 2 (the thought must not be one)", len(extracted.Segments))
	}
	if strings.Contains(out, "FIRST-SECRET") {
		t.Errorf("the first user message survived redaction: %s", out)
	}
	if strings.Contains(out, "SECOND-SECRET") {
		t.Errorf("the LAST user message crossed the wire in plaintext while the request was recorded as redacted: %s", out)
	}
	if !strings.Contains(out, "INTERNAL-THOUGHT") {
		t.Errorf("the thinking part was overwritten; Gemini requires it echoed back verbatim across turns: %s", out)
	}
}

// TestRewriteRequest_SnakeCaseSystemInstruction is the second slot defect.
//
// The extractor reads both spellings; the rewrite read only camelCase. So a
// snake_case request started its segment index at the first user message: the
// system prompt's redaction landed in that message and the last message was
// never rewritten at all.
func TestRewriteRequest_SnakeCaseSystemInstruction(t *testing.T) {
	const body = `{"system_instruction":{"parts":[{"text":"SYSTEM-SECRET"}]},
		"contents":[
			{"role":"user","parts":[{"text":"USER-ONE"}]},
			{"role":"user","parts":[{"text":"USER-TWO"}]}
		]}`

	out, extracted := roundTripRequest(t, body)

	if len(extracted.Segments) != 3 {
		t.Fatalf("extract found %d segments, want 3 (system + two messages)", len(extracted.Segments))
	}
	for _, plaintext := range []string{"SYSTEM-SECRET", "USER-ONE", "USER-TWO"} {
		if strings.Contains(out, plaintext) {
			t.Errorf("%q survived redaction — the walks disagreed about how the system prompt is spelled: %s", plaintext, out)
		}
	}
}

// TestRewriteRequest_CamelCaseSystemInstructionStillWorks pins that supporting
// the second spelling did not break the documented one.
func TestRewriteRequest_CamelCaseSystemInstructionStillWorks(t *testing.T) {
	const body = `{"systemInstruction":{"parts":[{"text":"SYSTEM-SECRET"}]},
		"contents":[{"role":"user","parts":[{"text":"USER-ONE"}]}]}`

	out, _ := roundTripRequest(t, body)
	for _, plaintext := range []string{"SYSTEM-SECRET", "USER-ONE"} {
		if strings.Contains(out, plaintext) {
			t.Errorf("%q survived redaction: %s", plaintext, out)
		}
	}
}

// TestRewriteResponse_ThinkingPartDoesNotShiftTheSlots — the same slot rule on
// the response side, where the consequence is unredacted assistant text
// returned to the client.
func TestRewriteResponse_ThinkingPartDoesNotShiftTheSlots(t *testing.T) {
	const body = `{"candidates":[{"content":{"parts":[
		{"text":"REPLY-ONE"},
		{"text":"MODEL-THOUGHT","thought":true},
		{"text":"REPLY-TWO"}
	]}}]}`

	a := &Adapter{}
	ctx := context.Background()
	extracted, err := a.ExtractResponse(ctx, []byte(body), "application/json")
	if err != nil {
		t.Fatalf("ExtractResponse: %v", err)
	}
	rewritten, _, err := a.RewriteResponseBody(ctx, []byte(body), "application/json", redactAll(extracted))
	if err != nil {
		t.Fatalf("RewriteResponseBody: %v", err)
	}
	out := string(rewritten)

	if strings.Contains(out, "REPLY-ONE") {
		t.Errorf("the first reply survived redaction: %s", out)
	}
	if strings.Contains(out, "REPLY-TWO") {
		t.Errorf("unredacted assistant text was returned to the client: %s", out)
	}
	if !strings.Contains(out, "MODEL-THOUGHT") {
		t.Errorf("the thinking part was overwritten: %s", out)
	}
}

// TestRewriteRequest_NoExtractedTextSurvives is the invariant behind all four
// cases above, and it asserts the PROPERTY rather than a count.
//
// Counting writes is not enough, and the difference is the whole defect: with
// the thinking-part skip removed, [A, thought, B] still produces written == 2
// for 2 extracted segments — the rewrite simply put the second one in the wrong
// slot. The number matched while B went out in plaintext. So the assertion is
// that every string the extractor pulled out is gone from the rewritten body,
// and that a part the extractor did NOT pull out is left alone.
func TestRewriteRequest_NoExtractedTextSurvives(t *testing.T) {
	cases := []struct {
		name string
		json string
		// keep is text the rewrite must NOT touch: reasoning is off Segments,
		// so overwriting it both shifts the slots and corrupts the thought text
		// Gemini requires echoed back verbatim.
		keep []string
	}{
		{name: "thought between messages", json: `{"contents":[
			{"role":"user","parts":[{"text":"AAA"}]},
			{"role":"model","parts":[{"text":"TTT","thought":true}]},
			{"role":"user","parts":[{"text":"BBB"}]}]}`, keep: []string{"TTT"}},
		{name: "snake_case system", json: `{"system_instruction":{"parts":[{"text":"SSS"}]},
			"contents":[{"role":"user","parts":[{"text":"AAA"}]}]}`},
		{name: "camelCase system", json: `{"systemInstruction":{"parts":[{"text":"SSS"}]},
			"contents":[{"role":"user","parts":[{"text":"AAA"}]}]}`},
		{name: "function response", json: `{"contents":[
			{"role":"user","parts":[{"text":"AAA"}]},
			{"role":"user","parts":[{"functionResponse":{"response":{"result":"RRR"}}}]}]}`},
		{name: "thought first", json: `{"contents":[
			{"role":"model","parts":[{"text":"TTT","thought":true}]},
			{"role":"user","parts":[{"text":"AAA"}]}]}`, keep: []string{"TTT"}},
		{name: "two thoughts around a message", json: `{"contents":[
			{"role":"model","parts":[{"text":"T1","thought":true}]},
			{"role":"user","parts":[{"text":"AAA"}]},
			{"role":"model","parts":[{"text":"T2","thought":true}]},
			{"role":"user","parts":[{"text":"BBB"}]}]}`, keep: []string{"T1", "T2"}},
	}

	a := &Adapter{}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extracted, err := a.ExtractRequest(ctx, []byte(tc.json), "application/json")
			if err != nil {
				t.Fatalf("ExtractRequest: %v", err)
			}
			rewritten, _, err := a.RewriteRequestBody(ctx, []byte(tc.json), "application/json", redactAll(extracted))
			if err != nil {
				t.Fatalf("RewriteRequestBody: %v", err)
			}
			out := string(rewritten)
			for _, seg := range extracted.Segments {
				if seg != "" && strings.Contains(out, seg) {
					t.Errorf("extracted segment %q survived the rewrite — it crosses the wire in plaintext while the request is recorded as redacted: %s", seg, out)
				}
			}
			// Every slot must receive ITS OWN redaction. Checking only that
			// plaintext is gone cannot see a mis-PAIRING, which is the defect
			// this whole file is about.
			for i := range extracted.Segments {
				if !strings.Contains(out, redactionFor(i)) {
					t.Errorf("segment %d's redaction %q is absent — the rewrite skipped its slot or wrote another segment's value into it: %s",
						i, redactionFor(i), out)
				}
			}
			for _, keep := range tc.keep {
				if !strings.Contains(out, keep) {
					t.Errorf("%q was overwritten; it is not on Segments and must be left alone: %s", keep, out)
				}
			}
		})
	}
}

// TestRewriteRequest_SlotsPairByPosition covers the three misalignments the
// survivor-only oracle above cannot see on its own, each reported by an
// adversarial review of this change:
//
//   - a `thought:true` part carrying a functionResponse and NO text. The
//     extractor puts it on Segments (the reasoning test lives inside the text
//     branch), so a rewriter that skips the whole part drops that slot and
//     shifts every later one. This one was a REGRESSION introduced by the first
//     version of this fix — the pre-existing code got it right.
//   - a part carrying BOTH functionCall and functionResponse. The extractor
//     stops at the call and emits no Segment; a rewriter with no functionCall
//     branch writes a later message's redaction into the response.
//   - `contents` as a JSON object. The extractor's ForEach walks an object's
//     values, while the rewriter's .Array() yields nothing — zero writes and a
//     nil error, i.e. a request recorded as redacted and forwarded untouched.
func TestRewriteRequest_SlotsPairByPosition(t *testing.T) {
	a := &Adapter{}
	ctx := context.Background()

	t.Run("thought part carrying a functionResponse keeps its slot", func(t *testing.T) {
		const body = `{"contents":[
			{"role":"user","parts":[{"thought":true,"functionResponse":{"name":"f","response":{"result":"TOOL-SECRET"}}}]},
			{"role":"user","parts":[{"text":"USER-SECRET"}]}]}`

		extracted, err := a.ExtractRequest(ctx, []byte(body), "application/json")
		if err != nil {
			t.Fatalf("ExtractRequest: %v", err)
		}
		if len(extracted.Segments) != 2 {
			t.Fatalf("extract produced %d segments (%v); want 2", len(extracted.Segments), extracted.Segments)
		}
		rewritten, _, err := a.RewriteRequestBody(ctx, []byte(body), "application/json", redactAll(extracted))
		if err != nil {
			t.Fatalf("RewriteRequestBody: %v", err)
		}
		out := string(rewritten)
		if strings.Contains(out, "TOOL-SECRET") {
			t.Errorf("the tool result crossed the wire in plaintext: %s", out)
		}
		if strings.Contains(out, "USER-SECRET") {
			t.Errorf("the user message crossed the wire in plaintext: %s", out)
		}
		// Pairing, not just absence: slot 0 is the tool result, slot 1 the message.
		if !strings.Contains(out, `"result":"`+redactionFor(0)+`"`) {
			t.Errorf("slot 0's redaction did not land on the tool result: %s", out)
		}
		if !strings.Contains(out, `"text":"`+redactionFor(1)+`"`) {
			t.Errorf("slot 1's redaction did not land on the user message: %s", out)
		}
	})

	t.Run("a functionCall part consumes no slot", func(t *testing.T) {
		const body = `{"contents":[
			{"role":"user","parts":[{"functionCall":{"name":"f"},"functionResponse":{"name":"f","response":{"result":"TOOL-TEXT"}}}]},
			{"role":"user","parts":[{"text":"USER-SECRET"}]}]}`

		extracted, err := a.ExtractRequest(ctx, []byte(body), "application/json")
		if err != nil {
			t.Fatalf("ExtractRequest: %v", err)
		}
		if len(extracted.Segments) != 1 {
			t.Fatalf("extract produced %d segments (%v); the functionCall part must yield none", len(extracted.Segments), extracted.Segments)
		}
		rewritten, _, err := a.RewriteRequestBody(ctx, []byte(body), "application/json", redactAll(extracted))
		if err != nil {
			t.Fatalf("RewriteRequestBody: %v", err)
		}
		out := string(rewritten)
		if strings.Contains(out, "USER-SECRET") {
			t.Errorf("the user message crossed the wire in plaintext: %s", out)
		}
		if !strings.Contains(out, `"text":"`+redactionFor(0)+`"`) {
			t.Errorf("the only segment's redaction did not land on the user message: %s", out)
		}
		if !strings.Contains(out, "TOOL-TEXT") {
			t.Errorf("a slot was spent on a part the extractor never put on Segments: %s", out)
		}
	})

	t.Run("contents as an object is refused rather than silently unrewritten", func(t *testing.T) {
		const body = `{"contents":{"m1":{"role":"user","parts":[{"text":"OBJ-SECRET"}]}}}`
		if _, _, err := a.RewriteRequestBody(ctx, []byte(body), "application/json", traffic.NormalizedContent{Segments: []string{"x"}}); err == nil {
			t.Error("a shape the rewriter cannot walk returned success; the caller would forward it as redacted")
		}
	})
}
