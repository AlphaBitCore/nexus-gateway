package streaming

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

// This file is the correctness gate for the per-frame work skipped in the three
// provider usage accumulators (finding C-20). What survives there is one change:
// the anthropic accumulator switches on the event name before spending a validity
// scan. A byte-level key-absence gate was also shipped and then REVERTED as unsound
// — see the escape-encoded key frames in usageFrameCorpus for the input that broke
// it and why the corpus now pins that class permanently.
//
// The claim under test is "same answer, less work", so each accumulator runs next to
// the ORIGINAL Feed over the same frame sequence and the accumulated state must
// match exactly. The state compared is everything Finalize can read — the two token
// counters and the fallback text buffer — because those reach the usage metering
// that feeds cost estimation, and a silent divergence there shows up as wrong money
// rather than as a failing test. That is not hypothetical: it is exactly what the
// reverted gate did.

// --- Reference (pre-change) Feed implementations. Oracles only; do not edit to
// "improve" them. ---

func referenceOpenAIFeed(a *openaiAccumulator, evt *SSEEvent) {
	if evt == nil || evt.Done || evt.Data == "" {
		return
	}
	data := evt.Data
	if !gjson.Valid(data) {
		return
	}
	if u := gjson.Get(data, "usage"); u.Exists() {
		if p := u.Get("prompt_tokens"); p.Exists() && p.Type == gjson.Number {
			v := int(p.Int())
			a.prompt = &v
		}
		if c := u.Get("completion_tokens"); c.Exists() && c.Type == gjson.Number {
			v := int(c.Int())
			a.completion = &v
		}
	}
	gjson.Get(data, "choices").ForEach(func(_, choice gjson.Result) bool {
		if t := choice.Get("delta.content"); t.Exists() && t.Type == gjson.String {
			a.textBuf.WriteString(t.Str)
		}
		return true
	})
}

func referenceAnthropicFeed(a *anthropicAccumulator, evt *SSEEvent) {
	if evt == nil || evt.Done || evt.Data == "" {
		return
	}
	data := evt.Data
	if !gjson.Valid(data) {
		return
	}
	switch evt.Event {
	case "message_start":
		if v := gjson.Get(data, "message.usage.input_tokens"); v.Exists() && v.Type == gjson.Number {
			val := int(v.Int())
			a.prompt = &val
		}
		if v := gjson.Get(data, "message.usage.output_tokens"); v.Exists() && v.Type == gjson.Number {
			val := int(v.Int())
			a.completion = &val
		}
	case "message_delta":
		if v := gjson.Get(data, "usage.output_tokens"); v.Exists() && v.Type == gjson.Number {
			val := int(v.Int())
			a.completion = &val
		}
	case "content_block_delta":
		if t := gjson.Get(data, "delta.text"); t.Exists() && t.Type == gjson.String {
			a.textBuf.WriteString(t.Str)
		}
	}
}

func referenceGeminiFeed(a *geminiAccumulator, evt *SSEEvent) {
	if evt == nil || evt.Done || evt.Data == "" {
		return
	}
	data := evt.Data
	if !gjson.Valid(data) {
		return
	}
	if u := gjson.Get(data, "usageMetadata"); u.Exists() {
		if p := u.Get("promptTokenCount"); p.Exists() && p.Type == gjson.Number {
			v := int(p.Int())
			a.prompt = &v
		}
		var candidates, thoughts int
		if c := u.Get("candidatesTokenCount"); c.Exists() && c.Type == gjson.Number {
			candidates = int(c.Int())
		}
		if t := u.Get("thoughtsTokenCount"); t.Exists() && t.Type == gjson.Number {
			thoughts = int(t.Int())
		}
		total := candidates + thoughts
		a.completion = &total
	}
	gjson.Get(data, "candidates").ForEach(func(_, cand gjson.Result) bool {
		cand.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			if t := part.Get("text"); t.Exists() && t.Type == gjson.String {
				a.textBuf.WriteString(t.Str)
			}
			return true
		})
		return true
	})
}

// accState is everything Finalize can observe. Comparing the whole state rather
// than "did the text come out" is deliberate: the token counters feed cost
// estimation, so a gate that silently stopped seeing a `usage` frame would be a
// billing defect, not a cosmetic one.
type accState struct {
	prompt     string
	completion string
	text       string
}

func ptrStr(p *int) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *p)
}

// usageFrameCorpus is a frame sequence spanning all three provider shapes, fed
// to every accumulator regardless of provider. Cross-feeding is intentional: a
// transparent MITM resolves the accumulator from detected request metadata, so a
// mis-detected provider really does get another provider's wire, and the gating
// must behave identically to the original there too.
func usageFrameCorpus() []*SSEEvent {
	return []*SSEEvent{
		// --- OpenAI ---
		{Data: `{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`},
		{Data: `{"choices":[{"index":0,"delta":{"content":"lo"}}]}`},
		{Data: `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`},
		{Data: `{"choices":[{"delta":{"content":"a"}},{"delta":{"content":"b"}}]}`},
		{Data: `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5}}`},
		{Data: `{"usage":{"prompt_tokens":"not-a-number","completion_tokens":null}}`},
		{Data: `{"usage":{}}`},
		{Data: `{"choices":[{"delta":{"content":123}}]}`},
		// A `usage` substring inside a VALUE, not a key — the gate must not be
		// fooled into skipping, and must not be fooled into finding either.
		{Data: `{"choices":[{"delta":{"content":"token usage report"}}]}`},
		{Data: `{"note":"choices and usage mentioned but no keys"}`},

		// --- Anthropic ---
		{Event: "message_start", Data: `{"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":1}}}`},
		{Event: "ping", Data: `{"type":"ping"}`},
		{Event: "content_block_start", Data: `{"type":"content_block_start","index":0}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi "}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"there"}}`},
		{Event: "content_block_delta", Data: `{"type":"content_block_delta","delta":{"text":42}}`},
		{Event: "content_block_stop", Data: `{"type":"content_block_stop","index":0}`},
		{Event: "message_delta", Data: `{"type":"message_delta","usage":{"output_tokens":77}}`},
		{Event: "message_stop", Data: `{"type":"message_stop"}`},
		// Unknown event name carrying data the switch never reads.
		{Event: "some_future_event", Data: `{"usage":{"output_tokens":999},"delta":{"text":"ignored"}}`},

		// --- Gemini ---
		{Data: `{"candidates":[{"content":{"parts":[{"text":"gem"}]}}]}`},
		{Data: `{"candidates":[{"content":{"parts":[{"text":"ini"},{"text":"!"}]}}]}`},
		{Data: `{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"thoughtsTokenCount":2}}`},
		{Data: `{"usageMetadata":{"promptTokenCount":3}}`},
		{Data: `{"candidates":[{"content":{"parts":[{"text":null}]}}]}`},

		// --- Escape-encoded and case-variant KEY names ---
		//
		// These exist because a byte-level "the key literal is absent, so the key is
		// absent" gate was shipped here and was UNSOUND: gjson decodes \uXXXX in key
		// names before matching a path, so a frame spelling `usage` as
		// `\u0075sage` was skipped and its reported tokens silently lost — on VALID
		// JSON, feeding cost estimation. 35 corpus frames and 9.5 M fuzz executions
		// missed it because not one of them contained an escape in a key, and
		// byte-level mutation essentially never synthesises one. The gate is gone;
		// these frames stay so its return would fail here immediately.
		{Data: "{\"\\u0075sage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}"},
		{Data: "{\"\\u0063hoices\":[{\"delta\":{\"content\":\"escaped-key\"}}]}"},
		{Data: "{\"\\u0075sageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":4}}"},
		{Data: "{\"\\u0063andidates\":[{\"content\":{\"parts\":[{\"text\":\"esc\"}]}}]}"},
		{Event: "content_block_delta", Data: "{\"type\":\"content_block_delta\",\"\\u0064elta\":{\"text\":\"esc\"}}"},
		// Case variants pin the other half of the premise: gjson path matching IS
		// case-sensitive, so these must NOT resolve.
		{Data: `{"USAGE":{"prompt_tokens":1}}`},
		{Data: `{"CHOICES":[{"delta":{"content":"upper"}}]}`},
		{Data: `{"usageMETADATA":{"promptTokenCount":1}}`},

		// --- Malformed / degenerate: the B9 normal case ---
		{Data: `{"choices":`},
		{Data: `{"usage":{"prompt_tokens":5`},
		{Data: `{"candidates":[`},
		{Data: `not json`},
		{Data: `[DONE]`},
		{Data: ``},
		{Data: `{}`},
		{Data: `[]`},
		{Data: `null`},
		{Data: `{"choices":[{"delta":{"content":"after-done"}}]}`, Done: true},
	}
}

// TestOpenAIAccumulator_GatingMatchesReference proves the two byte-level key
// proofs in openaiAccumulator.Feed do not change what the stream reports.
func TestOpenAIAccumulator_GatingMatchesReference(t *testing.T) {
	got, want := &openaiAccumulator{}, &openaiAccumulator{}
	for i, evt := range usageFrameCorpus() {
		got.Feed(evt)
		referenceOpenAIFeed(want, evt)
		assertAccStateEqual(t, i, evt, openAIState(got), openAIState(want))
	}
}

// TestAnthropicAccumulator_GatingMatchesReference proves hoisting the event-name
// switch above the validity scan does not change what the stream reports —
// including for frames whose event name the switch ignores but whose bytes the
// old code still validated.
func TestAnthropicAccumulator_GatingMatchesReference(t *testing.T) {
	got, want := &anthropicAccumulator{}, &anthropicAccumulator{}
	for i, evt := range usageFrameCorpus() {
		got.Feed(evt)
		referenceAnthropicFeed(want, evt)
		assertAccStateEqual(t, i, evt, anthropicState(got), anthropicState(want))
	}
}

// TestGeminiAccumulator_GatingMatchesReference is the same proof for the Gemini
// shape, where `candidatesTokenCount` contains "candidates" as a prefix so the
// candidates gate is deliberately over-inclusive on usage-only frames.
func TestGeminiAccumulator_GatingMatchesReference(t *testing.T) {
	got, want := &geminiAccumulator{}, &geminiAccumulator{}
	for i, evt := range usageFrameCorpus() {
		got.Feed(evt)
		referenceGeminiFeed(want, evt)
		assertAccStateEqual(t, i, evt, geminiState(got), geminiState(want))
	}
}

func openAIState(a *openaiAccumulator) accState {
	return accState{ptrStr(a.prompt), ptrStr(a.completion), a.textBuf.String()}
}

func anthropicState(a *anthropicAccumulator) accState {
	return accState{ptrStr(a.prompt), ptrStr(a.completion), a.textBuf.String()}
}

func geminiState(a *geminiAccumulator) accState {
	return accState{ptrStr(a.prompt), ptrStr(a.completion), a.textBuf.String()}
}

func assertAccStateEqual(t *testing.T, i int, evt *SSEEvent, got, want accState) {
	t.Helper()
	if got != want {
		t.Errorf("after frame %d (event=%q data=%q):\n  got  prompt=%s completion=%s text=%q\n  want prompt=%s completion=%s text=%q",
			i, evt.Event, evt.Data,
			got.prompt, got.completion, got.text,
			want.prompt, want.completion, want.text)
	}
}

// There is deliberately NO "the gate actually skips" test here.
//
// One existed and was deleted as inert: its OpenAI and Gemini arms never called
// production code — they evaluated a hand-rolled reimplementation of
// strings.Contains against a string literal, which is a constant assertion — and
// hard-wiring every gate open left all tests green, refuting the test's own
// docstring. The byte gates are now gone, and what remains is the Anthropic
// event-name switch hoisted above the validity scan, which is invisible to
// behaviour BY CONSTRUCTION: a frame the switch ignores was ignored before too.
// No correctness test can pin it, so it is pinned by
// BenchmarkAB_Feed_AnthropicIgnoredEvent instead. Removing the hoist cannot fail a
// test, only that benchmark — stated here so the absence reads as a decision
// rather than an oversight.

// FuzzUsageAccumulatorGating_MatchesReference covers the frame shapes the corpus
// did not think of, for all three accumulators at once.
func FuzzUsageAccumulatorGating_MatchesReference(f *testing.F) {
	for _, evt := range usageFrameCorpus() {
		f.Add(evt.Event, evt.Data)
	}
	f.Fuzz(func(t *testing.T, event, data string) {
		evt := &SSEEvent{Event: event, Data: data}

		gotO, wantO := &openaiAccumulator{}, &openaiAccumulator{}
		gotO.Feed(evt)
		referenceOpenAIFeed(wantO, evt)
		if openAIState(gotO) != openAIState(wantO) {
			t.Fatalf("openai divergence on event=%q data=%q: got %+v want %+v",
				event, data, openAIState(gotO), openAIState(wantO))
		}

		gotA, wantA := &anthropicAccumulator{}, &anthropicAccumulator{}
		gotA.Feed(evt)
		referenceAnthropicFeed(wantA, evt)
		if anthropicState(gotA) != anthropicState(wantA) {
			t.Fatalf("anthropic divergence on event=%q data=%q: got %+v want %+v",
				event, data, anthropicState(gotA), anthropicState(wantA))
		}

		gotG, wantG := &geminiAccumulator{}, &geminiAccumulator{}
		gotG.Feed(evt)
		referenceGeminiFeed(wantG, evt)
		if geminiState(gotG) != geminiState(wantG) {
			t.Fatalf("gemini divergence on event=%q data=%q: got %+v want %+v",
				event, data, geminiState(gotG), geminiState(wantG))
		}
	})
}
