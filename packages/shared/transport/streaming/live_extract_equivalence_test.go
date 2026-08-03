package streaming

import (
	"strings"
	"testing"

	stdjson "encoding/json"

	"github.com/goccy/go-json"
)

// This file is the correctness gate for the extractDeltaText cheap-reject
// (finding C-16). The optimization's whole claim is "same answer, less work", so
// the test does not re-state what the answer should be — it runs the ORIGINAL
// implementation next to the current one and requires them to agree, byte for
// byte, on every input.
//
// That shape is deliberate. The three outcomes (delta text / "" / raw verbatim)
// hinge on goccy's exact struct-decode semantics — case-insensitive field
// matching, type-mismatch becoming a decode error, an array or scalar at the top
// level failing to decode at all — and those are properties of the pinned
// library, not of anything this repository states. A hand-written expectation
// table would encode my belief about goccy; a differential test encodes goccy.

// extractDeltaTextReference is verbatim the implementation that shipped before
// the cheap-reject and the validity gate were added, wrapped in a recover.
//
// The recover is not defensive padding — it is part of the oracle's contract.
// The old implementation could PANIC rather than return, because goccy v0.10.6
// overruns its own buffer on a truncated escape sequence (finding C-31), and the
// `[]byte(data)` conversion it used was one of the allocation shapes that
// triggers it. So the oracle has two outcomes, "returned x" and "panicked", and
// the differential assertion below is stated over both.
//
// Do not "improve" this function: its only job is to be the oracle. If a future
// change to extractDeltaText is meant to alter behaviour rather than only cost,
// this reference must be updated in the same commit and the intent stated there.
func extractDeltaTextReference(evt *SSEEvent) (out string, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	if evt.Done {
		return "", false
	}
	data := evt.Data
	if data == "" {
		return "", false
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err == nil {
		if len(chunk.Choices) > 0 {
			return chunk.Choices[0].Delta.Content, false
		}
		return "", false
	}
	return data, false
}

// assertExtractEquivalent is the differential assertion. It splits on the ONE
// input class whose behaviour the change alters:
//
//   - invalid JSON → the current implementation must return the raw data verbatim.
//     The reference is NOT consulted, and that is the point rather than an
//     omission: malformed input is precisely what faults inside goccy, and under
//     -race it faults as an uncatchable runtime throw (checkptr) that would take
//     the whole test binary down. The oracle cannot be asked about the defect it
//     has. Raw-verbatim is also the answer the contract already prescribes for
//     input it cannot parse, so the only real delta is the handful of sequences
//     goccy accepted and the standard does not.
//   - valid JSON → the current implementation must return exactly what the
//     reference returned. This is the "no behaviour change" claim, and it covers
//     every frame a provider can actually emit.
//
// If the reference faults in that second branch, the validity gate is insufficient
// and the test says so — loudly, by crashing under -race. That is the intended
// signal, and it is how the trailing-backslash guard was found to be inadequate.
func assertExtractEquivalent(t *testing.T, data string, done bool) {
	t.Helper()
	evt := &SSEEvent{Data: data, Done: done}
	got := extractDeltaText(evt)

	if strings.IndexByte(data, '\\') >= 0 && !stdjson.Valid([]byte(data)) {
		// evt.Done and empty data short-circuit before the gate.
		want := data
		if done || data == "" {
			want = ""
		}
		if got != want {
			t.Errorf("extractDeltaText(%q, done=%v) = %q, want %q for backslash-bearing invalid JSON", data, done, got, want)
		}
		return
	}

	want, faulted := extractDeltaTextReference(evt)
	if faulted {
		t.Errorf("reference faulted on VALID json %q — the validity gate is not a sufficient fix", data)
		return
	}
	if got != want {
		t.Errorf("extractDeltaText(%q, done=%v) = %q, reference = %q", data, done, got, want)
	}
}

// extractEquivalenceCorpus is the hand-picked half of the proof: every frame
// shape the audit and the B12 refutation identified as behaviourally
// significant, so a regression names itself instead of surfacing as an opaque
// fuzz failure.
func extractEquivalenceCorpus() []string {
	return []string{
		// --- OpenAI chat, the one modelled shape ---
		`{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[]}`,
		`{"choices":null}`,
		// Only the FIRST choice is read, even though more exist.
		`{"choices":[{"delta":{"content":"a"}},{"delta":{"content":"b"}}]}`,
		// Tool-call-only delta: modelled shape, but no content.
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}`,
		// Usage-only trailer frame.
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,

		// --- Case-insensitive field matching (the B12 refutation) ---
		`{"CHOICES":[{"delta":{"content":"upper"}}]}`,
		`{"Choices":[{"Delta":{"Content":"mixed"}}]}`,
		`{"choices":[{"delta":{"CONTENT":"upper-content"}}]}`,

		// --- Type mismatch → decode error → raw verbatim ---
		`{"choices":{"a":1}}`,
		`{"choices":[{"delta":{"content":123}}]}`,
		`{"choices":"a string"}`,
		`{"choices":[{"delta":"a string"}]}`,
		`{"choices":[42]}`,

		// --- Valid JSON that is not an object → decode error → raw verbatim ---
		`[1,2,3]`,
		`"just a json string"`,
		`123`,
		`true`,
		`null`,
		`[]`,
		`[{"choices":[{"delta":{"content":"nested"}}]}]`,

		// --- choices-less valid objects: the B9 normal case the fast path targets ---
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":9}}}`,
		`{"type":"response.output_text.delta","delta":"responses-api"}`,
		`{"candidates":[{"content":{"parts":[{"text":"gemini"}]}}]}`,
		`{}`,
		`{"a":1}`,
		// "choices" present only inside a string VALUE — must not be mistaken for
		// a key, and must still produce the same answer either way.
		`{"text":"here are your choices"}`,
		`{"text":"CHOICES are plural"}`,
		`{"choicesLike":1}`,
		`{"nochoices":1}`,

		// --- Malformed → decode error → raw verbatim ---
		`{`,
		`{"choices":`,
		`{"choices":[{"delta":{"content":"unterminated`,
		`{"type":"content_block_delta"`,
		`{"a":1,}`,
		`{'a':1}`,
		`{"a" 1}`,
		`not json at all`,
		`[DONE]`,
		`   `,
		"\t\n",
		// Whitespace-led valid object: the fast path must see past the padding.
		"   \t\n{\"a\":1}",
		"\n\n{\"choices\":[{\"delta\":{\"content\":\"padded\"}}]}",
		// Deep nesting — both implementations must agree on whatever they decide.
		strings.Repeat(`{"a":`, 200) + `1` + strings.Repeat(`}`, 200),
	}
}

// TestExtractDeltaText_MatchesReferenceOnCorpus is the primary C-16 correctness
// proof: identical output to the pre-optimization implementation on every
// behaviourally significant frame shape.
func TestExtractDeltaText_MatchesReferenceOnCorpus(t *testing.T) {
	for _, data := range extractEquivalenceCorpus() {
		for _, done := range []bool{false, true} {
			assertExtractEquivalent(t, data, done)
		}
	}
}

// crasherFixtures returns the byte sequences that fault goccy's struct decoder,
// built by CONCATENATION rather than written as literals.
//
// That is not stylistic. These were first written as Go RAW string literals, in
// which a backslash is not an escape — so the intended 3-byte `{"` + backslash
// became a 4-byte `{"` + backslash + backslash, an *escaped* backslash, which
// decodes to a clean error and never reaches the fault. All seven fixtures passed
// while exercising nothing. The length assertions below exist so that cannot
// recur silently.
func crasherFixtures() []struct {
	name  string
	data  string
	wantN int
} {
	bs := "\\" // exactly one backslash
	return []struct {
		name  string
		data  string
		wantN int
	}{
		{"bare truncated key escape", `{"` + bs, 3},
		{"truncated value escape", `{"a":"` + bs, 7},
		{"truncated inside a content delta", `{"choices":[{"delta":{"content":"x` + bs, 35},
		{"array with truncated escape", `["` + bs, 3},
		{"bare string with truncated escape", `"` + bs, 2},
		// The fuzzer's counterexample to the cheaper trailing-backslash guard: the
		// malformed escape is NOT at the end, so no suffix check can catch it.
		{"malformed escape mid-payload", `{"` + bs + `00` + bs + `.`, 7},
	}
}

// TestExtractDeltaText_MalformedEscapeIsSafe is the C-31 regression test: these
// bytes used to fault the decoder — a recoverable panic in a normal build, an
// uncatchable checkptr throw under -race, which is how CI runs. Under -race a
// missing gate takes the test binary down rather than failing this test, so the
// deterministic guard against gate removal is
// TestExtractDeltaText_ValidityGateIsPresent below; this test pins the inputs and
// the answer.
func TestExtractDeltaText_MalformedEscapeIsSafe(t *testing.T) {
	for _, f := range crasherFixtures() {
		if len(f.data) != f.wantN {
			t.Errorf("%s: fixture is %d bytes, want %d — the escaping drifted and this fixture no longer exercises the fault (%q)",
				f.name, len(f.data), f.wantN, f.data)
			continue
		}
		if strings.Contains(f.data, "\\\\") {
			t.Errorf("%s: fixture contains a doubled backslash, so its escape is well-formed and harmless (%q)", f.name, f.data)
			continue
		}
		// The gate is only legitimate for input that really is invalid JSON.
		if stdjson.Valid([]byte(f.data)) {
			t.Errorf("%s: fixture is valid JSON, so the gate would be changing a real answer (%q)", f.name, f.data)
			continue
		}
		if got := extractDeltaText(&SSEEvent{Data: f.data}); got != f.data {
			t.Errorf("%s: extractDeltaText(%q) = %q, want the raw data verbatim", f.name, f.data, got)
		}
	}
}

// TestExtractDeltaText_ValidityGateIsPresent is the deterministic guard on the
// C-31 fix, and it exists because removing the gate is otherwise INVISIBLE to the
// whole named-test suite: for malformed input the decoder's error path returns the
// raw data too, so the answer is unchanged and only the fault distinguishes them.
//
// The discriminator has to satisfy three things at once: carry a backslash (or it
// never reaches the validator), be rejected by the validator, and still be ACCEPTED
// by the decoder. Then without the gate this function answers what the decoder says
// and with the gate it answers the raw data — a difference observable without
// needing a crash, so deleting the gate fails HERE, deterministically, in a plain
// build. The test asserts all three preconditions rather than trusting them, because
// a first attempt used an escaped quote that silently swallowed the rest of the
// frame and made the input indistinguishable.
func TestExtractDeltaText_ValidityGateIsPresent(t *testing.T) {
	// Must carry a backslash to reach the validator at all (that is the gate's
	// mechanistic precondition), be rejected by it, and still be accepted by the
	// decoder — otherwise it cannot tell gate from no-gate.
	// A well-formed \u escape (so goccy still parses the frame) plus the
	// adjacent-strings construct that stdlib rejects and goccy accepts.
	discriminator := `{"a":"\u0041","":[""""]}`

	if strings.IndexByte(discriminator, '\\') < 0 {
		t.Fatalf("%q has no backslash, so it never reaches the validator", discriminator)
	}
	if stdjson.Valid([]byte(discriminator)) {
		t.Fatalf("%q is now considered valid — it no longer discriminates gate from no-gate", discriminator)
	}
	// Pin the other half of the discriminator: the decoder really would accept it.
	ref, faulted := extractDeltaTextReference(&SSEEvent{Data: discriminator})
	if faulted {
		t.Fatalf("%q faulted the decoder — pick a discriminator the decoder accepts", discriminator)
	}
	if ref == discriminator {
		t.Fatalf("the decoder already answers the raw data for %q — it cannot distinguish gate from no-gate", discriminator)
	}
	if got := extractDeltaText(&SSEEvent{Data: discriminator}); got != discriminator {
		t.Errorf("extractDeltaText(%q) = %q, want the raw data verbatim — the validity gate is missing", discriminator, got)
	}
}

// TestExtractDeltaText_DeepNestingDoesNotOverflow is the R-7 regression test.
//
// The validity gate shipped with gjson.Valid, which is mutually recursive with no
// depth limit and burns ~128 B of stack per nesting level, so a deep payload became
// `fatal error: stack overflow` — an unrecoverable throw that kills the process,
// strictly worse than the per-connection fault the gate exists to prevent. The
// standard library's validator is iterative with a bounded depth, which is why it
// is the one on the path.
//
// The depth here is far below the ~8 M that produced the fatal, deliberately: the
// point is to run in a normal test budget while still being orders of magnitude
// past any real payload, and to fail loudly (by dying) rather than subtly if the
// recursive validator is ever restored.
func TestExtractDeltaText_DeepNestingDoesNotOverflow(t *testing.T) {
	const depth = 200000
	// The trailing backslash is what routes this through the validator; without it
	// the frame short-circuits before the gate and the test would prove nothing.
	deep := strings.Repeat(`[`, depth) + `"\`

	// Pin the premise: this is the shape a recursive validator recurses on, and it
	// is invalid JSON (unterminated), so the expected answer is raw verbatim.
	if stdjson.Valid([]byte(deep)) {
		t.Fatal("fixture is valid JSON — it no longer exercises the deep-nesting path")
	}
	if got := extractDeltaText(&SSEEvent{Data: deep}); got != deep {
		t.Errorf("extractDeltaText(<%d open brackets>) returned %d bytes, want the raw data verbatim", depth, len(got))
	}
}

// FuzzExtractDeltaText_NeverFaults is the standing search for a crasher the
// trailing-backslash guard does not cover. It asserts nothing about the answer —
// FuzzExtractDeltaText_MatchesReference does that — only that arbitrary bytes
// cannot fault the function. Run it under -race, where goccy's buffer overrun
// surfaces as an uncatchable checkptr throw rather than a recoverable panic:
//
//	go test ./transport/streaming/ -run '^$' -race \
//	  -fuzz FuzzExtractDeltaText_NeverFaults -fuzztime=10m
//
// A finding here is a production defect on a MITM data path, not a test problem.
func FuzzExtractDeltaText_NeverFaults(f *testing.F) {
	for _, data := range extractEquivalenceCorpus() {
		f.Add(data)
	}
	f.Add(`{"\\`)
	f.Fuzz(func(t *testing.T, data string) {
		_ = extractDeltaText(&SSEEvent{Data: data})
	})
}

// FuzzExtractDeltaText_MatchesReference is the other half of the proof: the
// corpus covers what the audit thought of, the fuzzer covers what it did not.
// Any input on which the cheap-reject disagrees with the reference is a
// behaviour change, which B1 forbids regardless of the speedup.
func FuzzExtractDeltaText_MatchesReference(f *testing.F) {
	for _, data := range extractEquivalenceCorpus() {
		f.Add(data)
	}
	f.Add(`{"\`) // the C-31 counterexample this fuzzer found
	f.Fuzz(func(t *testing.T, data string) {
		assertExtractEquivalent(t, data, false)
	})
}
