package cohere

import (
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func encodeChat(t *testing.T, in string) []byte {
	t.Helper()
	got, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, []byte(in), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest returned %v", err)
	}
	return got.Body
}

// TestEncodeRequest_DropsEveryFieldCohereRejects is the regression for the whole
// family, not for its members. Each name below was sent to api.cohere.com/v2/chat
// on 2026-08-05 and answered with HTTP 422 "unknown field"; `nexus` is our own
// extension namespace, which the Responses ingress stamps onto every canonical
// body and which reached production as five models' worth of 422s within hours of
// the previous single-field fix going out.
func TestEncodeRequest_DropsEveryFieldCohereRejects(t *testing.T) {
	rejected := []string{
		"nexus", "n", "top_logprobs", "user", "store", "metadata",
		"stream_options", "parallel_tool_calls", "reasoning_effort",
		"modalities", "prediction", "service_tier", "logit_bias",
	}
	in := `{"model":"command-a-03-2025","messages":[{"role":"user","content":"hi"}],` +
		`"nexus":{"ext":{"openai":{"responses":{"instructions":"x"}}}},` +
		`"n":1,"top_logprobs":2,"user":"u1","store":false,"metadata":{"a":"b"},` +
		`"stream_options":{"include_usage":true},"parallel_tool_calls":true,` +
		`"reasoning_effort":"low","modalities":["text"],"prediction":{"type":"content"},` +
		`"service_tier":"auto","logit_bias":{"1":1}}`

	body := encodeChat(t, in)
	for _, f := range rejected {
		if gjson.GetBytes(body, f).Exists() {
			t.Errorf("%q reached the Cohere wire; it answers 422 unknown field", f)
		}
	}
	// The request must still be a request.
	if gjson.GetBytes(body, "model").Str != "command-a-03-2025" {
		t.Error("model did not survive the projection")
	}
	if !gjson.GetBytes(body, "messages").IsArray() {
		t.Error("messages did not survive the projection")
	}
}

// TestEncodeRequest_RenamesTheSpellingsCohereDiffersOn covers the defect the old
// header comment denied: it claimed "top_p stays top_p (matches Cohere's `p`
// field via OpenAI alias)". Measured, top_p is a 422 — so every request carrying
// the most common sampling parameter in the canonical shape was failing under a
// comment saying it was handled.
func TestEncodeRequest_RenamesTheSpellingsCohereDiffersOn(t *testing.T) {
	body := encodeChat(t, `{"model":"m","messages":[],"top_p":0.9,"top_k":5,"stop":["END"],"max_completion_tokens":64}`)

	for _, canonical := range []string{"top_p", "top_k", "stop", "max_completion_tokens"} {
		if gjson.GetBytes(body, canonical).Exists() {
			t.Errorf("canonical spelling %q reached the wire; Cohere answers 422", canonical)
		}
	}
	if v := gjson.GetBytes(body, "p"); v.Float() != 0.9 {
		t.Errorf("p = %v, want 0.9 — top_p must arrive, not vanish", v.Raw)
	}
	if v := gjson.GetBytes(body, "k"); v.Int() != 5 {
		t.Errorf("k = %v, want 5", v.Raw)
	}
	if v := gjson.GetBytes(body, "stop_sequences"); v.Raw != `["END"]` {
		t.Errorf("stop_sequences = %v, want [\"END\"]", v.Raw)
	}
	if v := gjson.GetBytes(body, "max_tokens"); v.Int() != 64 {
		t.Errorf("max_tokens = %v, want 64", v.Raw)
	}
}

// TestEncodeRequest_KeepsEveryFieldCohereAccepts is the other half: a projection
// that dropped a supported field would silently discard the caller's intent —
// a quieter defect than a 422 and a worse one.
func TestEncodeRequest_KeepsEveryFieldCohereAccepts(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"temperature":0.5,"max_tokens":8,"p":0.8,"k":3,"stop_sequences":["S"],` +
		`"presence_penalty":0.1,"frequency_penalty":0.2,"seed":7,` +
		`"safety_mode":"CONTEXTUAL","response_format":{"type":"text"},` +
		`"citation_options":{"mode":"OFF"},"tools":[],"tool_choice":"NONE",` +
		`"documents":["d"],"strict_tools":false,"logprobs":true,` +
		`"thinking":{"type":"disabled"},"priority":1}`

	body := encodeChat(t, in)
	for _, f := range []string{
		"model", "messages", "stream", "temperature", "max_tokens", "p", "k",
		"stop_sequences", "presence_penalty", "frequency_penalty", "seed",
		"safety_mode", "response_format", "citation_options", "tools",
		"tool_choice", "documents", "strict_tools", "logprobs",
		"thinking", "priority",
	} {
		if !gjson.GetBytes(body, f).Exists() {
			t.Errorf("%q was dropped; Cohere accepts it, so dropping it discards caller intent", f)
		}
	}
}

// logprobs earns its place by a different verdict than the rest: Cohere answers
// 400 "logprobs is not supported with the specified model", not 422 unknown
// field. The MODEL refuses it, not the wire — filtering it here would replace a
// real per-model capability answer with our own silence.
func TestEncodeRequest_KeepsLogprobsSoTheModelAnswersForIt(t *testing.T) {
	body := encodeChat(t, `{"model":"m","messages":[],"logprobs":true}`)
	if !gjson.GetBytes(body, "logprobs").Exists() {
		t.Error("logprobs was filtered; the model's 400 is the honest answer, not our silence")
	}
}

// A rename wins over a value already sitting under the target name, matching the
// anthropic and gemini codecs. Disagreeing per-provider would make one request
// mean different things depending on where it routed.
func TestEncodeRequest_RenameWinsOverTheTargetSpelling(t *testing.T) {
	body := encodeChat(t, `{"model":"m","messages":[],"max_tokens":16,"max_completion_tokens":128,"p":0.1,"top_p":0.9}`)
	if v := gjson.GetBytes(body, "max_tokens"); v.Int() != 128 {
		t.Errorf("max_tokens = %d, want 128", v.Int())
	}
	if v := gjson.GetBytes(body, "p"); v.Float() != 0.9 {
		t.Errorf("p = %v, want 0.9 (top_p wins)", v.Raw)
	}
}

// A body that is not a JSON object is returned untouched: EncodeRequest has
// already rejected invalid JSON, and a non-object is not something to project.
func TestProjectToCohereChat_NonObjectIsUntouched(t *testing.T) {
	for _, in := range []string{`[1,2]`, `"a string"`, `not json at all`} {
		if got := string(projectToCohereChat([]byte(in))); got != in {
			t.Errorf("projectToCohereChat(%q) = %q, want it untouched", in, got)
		}
	}
}

// escapeJSONPath keeps sjson's path syntax out of the key names. No field in the
// declared set contains one, which is exactly why the property belongs in a test
// rather than in a reader's memory.
func TestEscapeJSONPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"model", "model"},
		{"a.b", `a\.b`},
		{"a*b", `a\*b`},
		{"a?b", `a\?b`},
		{"a|b", `a\|b`},
		{"a#b", `a\#b`},
		{"a@b", `a\@b`},
	} {
		if got := escapeJSONPath(tc.in); got != tc.want {
			t.Errorf("escapeJSONPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
