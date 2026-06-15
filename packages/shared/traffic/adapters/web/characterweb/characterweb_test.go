package characterweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Identity + configuration

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "character-web" {
		t.Errorf("ID=%q want character-web", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// TestExtractRequest_TgtOnly pins the character.ai-native shape:
// roleplay prompts arrive in `tgt` (target speaker) — the adapter
// treats it as a prompt alias so audit content reaches downstream
// hooks. `character_id` lands in Metadata.
func TestExtractRequest_TgtOnly(t *testing.T) {
	body := []byte(`{"tgt":"speak as the captain","character_id":"char-7","src":"user","model":"chat-v2"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "speak as the captain" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["character_id"] != "char-7" {
		t.Errorf("character_id meta=%q want char-7", nc.Metadata["character_id"])
	}
	if nc.Metadata["model"] != "chat-v2" {
		t.Errorf("model meta=%q want chat-v2", nc.Metadata["model"])
	}
	// `src` and `character_id` belong to requestKnownKeys → not in Extra.
	if _, ok := nc.Extra["character_id"]; ok {
		t.Errorf("character_id should not leak into Extra: %v", nc.Extra)
	}
	if _, ok := nc.Extra["src"]; ok {
		t.Errorf("src should not leak into Extra: %v", nc.Extra)
	}
}

// TestExtractRequest_MessagesArray pins the openai-chat-like shape
// where character.ai sends a `messages` array.
func TestExtractRequest_MessagesArray(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "hi captain"},
			{"role": "assistant", "content": "Ahoy, sailor!"},
			{"role": "user", "content": "tell me a story"}
		],
		"model": "chat-v3"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"hi captain", "Ahoy, sailor!", "tell me a story"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d", len(nc.Segments), len(want))
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
	if nc.Metadata["model"] != "chat-v3" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

// TestExtractRequest_PromptAliases pins that every alias key in the
// adapter's compatibility list (`prompt`, `query`, `text`, `input`,
// `tgt`) contributes a segment when present and non-empty.
func TestExtractRequest_PromptAliases(t *testing.T) {
	body := []byte(`{
		"prompt": "one",
		"query": "two",
		"text": "three",
		"input": "four",
		"tgt": "five"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"one", "two", "three", "four", "five"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_EmptyPromptAliasesSkipped pins that an alias key
// with an empty string value does NOT contribute a phantom segment.
func TestExtractRequest_EmptyPromptAliasesSkipped(t *testing.T) {
	body := []byte(`{"prompt":"","query":"","text":"real text","input":"","tgt":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "real text" {
		t.Errorf("Segments=%v want [real text]", nc.Segments)
	}
}

// TestExtractRequest_MessagesAndTgtCombined covers a body that
// includes both a `messages` array and a `tgt` prompt — both sources
// must contribute segments.
func TestExtractRequest_MessagesAndTgtCombined(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"user","content":"first turn"}],
		"tgt": "follow-up via tgt"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"first turn", "follow-up via tgt"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractRequest_NonStringContentSkipped pins that a message
// whose `content` is structured (array, e.g. multimodal parts) does
// NOT crash; only string content is captured.
func TestExtractRequest_NonStringContentSkipped(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":[{"type":"text","text":"structured"}]},
			{"role":"user","content":"plain string"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain string" {
		t.Errorf("Segments=%v want [plain string] only", nc.Segments)
	}
}

// TestExtractRequest_ModelMetaMissing pins that the model meta map is
// returned without a `model` key when the body omits one.
func TestExtractRequest_ModelMetaMissing(t *testing.T) {
	body := []byte(`{"prompt":"hi"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["model"]; ok {
		t.Errorf("model key present in Metadata=%v want absent", nc.Metadata)
	}
	if _, ok := nc.Metadata["character_id"]; ok {
		t.Errorf("character_id key present in Metadata=%v want absent", nc.Metadata)
	}
}

// TestExtractRequest_CharacterIDMissing pins that an empty character_id
// is not written to Metadata.
func TestExtractRequest_CharacterIDMissing(t *testing.T) {
	body := []byte(`{"prompt":"hi","character_id":""}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if _, ok := nc.Metadata["character_id"]; ok {
		t.Errorf("empty character_id leaked into Metadata=%v", nc.Metadata)
	}
}

// TestExtractRequest_ExtraCapturesUnknownFields pins that fields
// outside the requestKnownKeys list reach NormalizedContent.Extra —
// safety net against silent data loss when character.ai ships a new
// field.
func TestExtractRequest_ExtraCapturesUnknownFields(t *testing.T) {
	body := []byte(`{
		"prompt": "hi",
		"x_new_char_field": {"sensitive": "secret_value"},
		"voice_style": "narrator"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	x, ok := nc.Extra["x_new_char_field"]
	if !ok || !strings.Contains(x, "secret_value") {
		t.Errorf("Extra=%v missing x_new_char_field", nc.Extra)
	}
	if _, ok := nc.Extra["voice_style"]; !ok {
		t.Errorf("Extra=%v missing voice_style", nc.Extra)
	}
	if _, ok := nc.Extra["prompt"]; ok {
		t.Errorf("prompt must not leak into Extra (it is consumed)")
	}
}

func TestExtractRequest_Empty(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractRequest_BinaryBody pins that a non-JSON binary payload
// returns ErrUnknownSchema and stamps a sanitised binary preview into
// Extra for triage.
func TestExtractRequest_BinaryBody(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x7f, 0x80, 0xff, 'h', 'i', 0x05}
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	prev, ok := nc.Extra["binary_preview"]
	if !ok {
		t.Fatalf("Extra missing binary_preview: %v", nc.Extra)
	}
	if !strings.Contains(prev, "hi") {
		t.Errorf("binary_preview=%q want to contain 'hi'", prev)
	}
	if strings.ContainsAny(prev, "\x00\x01\x7f\x80\xff") { //nolint:staticcheck // SA1011: intentional bad-UTF8 test fixture
		t.Errorf("binary_preview=%q must scrub control bytes", prev)
	}
}

// TestExtractRequest_Malformed pins ErrMalformed for body bytes that
// begin like JSON but are not parseable.
func TestExtractRequest_Malformed(t *testing.T) {
	body := []byte(`{"prompt": "missing close-brace`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_UnknownJSON pins ErrUnknownSchema when the body
// is valid JSON but carries no recognised character.ai fields — the
// response still includes Extra so hooks can see the foreign payload.
func TestExtractRequest_UnknownJSON(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["foo"]; !ok {
		t.Errorf("Extra=%v missing foo on unknown-schema path", nc.Extra)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil (empty body is benign)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// TestExtractResponse_Malformed: body starts as JSON but fails to
// parse — must be classified as malformed.
func TestExtractResponse_Malformed(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"oops":`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_BinaryPayload: a body that fails gjson.ValidBytes
// (binary noise) → ErrMalformed under the response path (validity is
// checked before looksLikeJSON, unlike ExtractRequest).
func TestExtractResponse_BinaryPayload(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte{0x00, 0xff, 'x'}, "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractResponse_JSONScalarNotObject: a body that gjson considers
// valid JSON (a scalar) but whose first non-whitespace byte is neither
// '{' nor '['. Reaches the !looksLikeJSON branch after the validity
// check passes → ErrUnknownSchema.
func TestExtractResponse_JSONScalarNotObject(t *testing.T) {
	a := &Adapter{}
	for _, body := range [][]byte{
		[]byte(`42`),
		[]byte(`"a string"`),
		[]byte(`true`),
		[]byte(`null`),
	} {
		_, err := a.ExtractResponse(context.Background(), body, "/api/x")
		if !errors.Is(err, traffic.ErrUnknownSchema) {
			t.Errorf("body=%q err=%v want ErrUnknownSchema", body, err)
		}
	}
}

// TestExtractResponse_BareErrorString pins character.ai's
// `"error": "string"` shape (bare string, not an object). The adapter
// exposes the message as a segment and stamps the error metadata flag.
func TestExtractResponse_BareErrorString(t *testing.T) {
	body := []byte(`{"error":"rate limit exceeded"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limit exceeded" {
		t.Errorf("Segments=%v want [rate limit exceeded]", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// TestExtractResponse_BareMessage pins the simpler `message` field —
// some character.ai endpoints return a top-level message.
func TestExtractResponse_BareMessage(t *testing.T) {
	body := []byte(`{"message":"conversation not found"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "conversation not found" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error metadata=%q want true", nc.Metadata["error"])
	}
}

// TestExtractResponse_UnknownJSON pins the fall-through: valid JSON
// without an error envelope nor a top-level message is unknown schema.
func TestExtractResponse_UnknownJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// TestExtractResponse_EmptyErrorFallsThrough pins that an envelope
// with `error` but empty string does NOT short-circuit — falls
// through to unknown-schema rather than emitting an empty segment.
func TestExtractResponse_EmptyErrorFallsThrough(t *testing.T) {
	body := []byte(`{"error":""}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema (empty error)", err)
	}
}

// TestExtractResponse_EmptyMessageFallsThrough pins that an empty
// `message` string does NOT short-circuit.
func TestExtractResponse_EmptyMessageFallsThrough(t *testing.T) {
	body := []byte(`{"message":""}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema (empty message)", err)
	}
}

// TestExtractStreamChunk_TextField pins the typical character.ai
// streaming token shape: `{"text": "...token..."}`.
func TestExtractStreamChunk_TextField(t *testing.T) {
	chunk := []byte(`{"text":"streamed token"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "streamed token" {
		t.Errorf("Segments=%v want [streamed token]", nc.Segments)
	}
}

// TestExtractStreamChunk_AllStreamAliases pins that each stream alias
// key (`text`, `content`, `delta`, `tgt`) contributes one segment in
// the documented order.
func TestExtractStreamChunk_AllStreamAliases(t *testing.T) {
	chunk := []byte(`{
		"text": "from text",
		"content": "from content",
		"delta": "from delta",
		"tgt": "from tgt"
	}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"from text", "from content", "from delta", "from tgt"}
	if len(nc.Segments) != len(want) {
		t.Fatalf("Segments len=%d want %d: %v", len(nc.Segments), len(want), nc.Segments)
	}
	for i := range want {
		if nc.Segments[i] != want[i] {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], want[i])
		}
	}
}

// TestExtractStreamChunk_EmptyStringsSkipped pins that empty alias
// values do NOT produce phantom empty segments.
func TestExtractStreamChunk_EmptyStringsSkipped(t *testing.T) {
	chunk := []byte(`{"text":"","content":"only-content","delta":"","tgt":""}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "only-content" {
		t.Errorf("Segments=%v want [only-content]", nc.Segments)
	}
}

// TestExtractStreamChunk_EmptyChunk: zero-length chunk is a no-op.
func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/api/x")
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("non-empty content: %+v", nc)
	}
}

// TestExtractStreamChunk_DefensiveOnNonJSON pins fail-open behaviour:
// non-JSON / invalid-JSON / non-object chunks return a clean empty
// payload with no error.
func TestExtractStreamChunk_DefensiveOnNonJSON(t *testing.T) {
	a := &Adapter{}
	cases := [][]byte{
		[]byte(`not json at all`),
		[]byte(`{"oops":`),
		[]byte(`[DONE]`),
		[]byte(`  `),
	}
	for i, c := range cases {
		nc, err := a.ExtractStreamChunk(context.Background(), c, "/api/x")
		if err != nil {
			t.Errorf("case %d err=%v want nil (fail-open)", i, err)
		}
		if len(nc.Segments) != 0 {
			t.Errorf("case %d non-empty content: %+v", i, nc)
		}
	}
}

// TestExtractStreamChunk_NoAliasesPresent: an object chunk that
// matches none of the alias keys yields a clean empty payload.
func TestExtractStreamChunk_NoAliasesPresent(t *testing.T) {
	chunk := []byte(`{"finish_reason":"stop"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments leaked: %v", nc.Segments)
	}
}

// DetectRequestMeta + DetectResponseUsage

// TestDetectRequestMeta_AlwaysProvider pins character-web's minimal
// implementation — model is never set (DetectRequestMeta doesn't
// parse the body); only Provider is stamped.
func TestDetectRequestMeta_AlwaysProvider(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://character.ai/api/x", nil)
	for _, body := range [][]byte{
		[]byte(`{"model":"chat-v3"}`),
		[]byte(`not json`),
		nil,
	} {
		meta := a.DetectRequestMeta(r, body)
		if meta.Provider != "character-web" {
			t.Errorf("body=%q Provider=%q want character-web", body, meta.Provider)
		}
		if meta.Model != "" {
			t.Errorf("body=%q Model=%q want empty (adapter never sets model)", body, meta.Model)
		}
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("token pointers must be nil for character-web; got %+v", usage)
	}
}

// Rewrite contracts (must return ErrRewriteUnsupported)

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"prompt":"hi"}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged on rewrite-unsupported")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"error":"x"}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/api/x", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
	if string(out) != string(body) {
		t.Errorf("body must be returned unchanged")
	}
}

// Normalize (Tier-1 spec dispatch)

// TestNormalize_RequestChatShape pins that an openai-chat-shaped
// request body claims Tier 1 via the shared OpenAI Chat codec and stamps
// DetectedSpec = "character-web".
func TestNormalize_RequestChatShape(t *testing.T) {
	body := []byte(`{
		"model": "chat-v3",
		"messages": [
			{"role": "system", "content": "You are the captain."},
			{"role": "user", "content": "set sail"}
		]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "character-web",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/api/x",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "character-web" {
		t.Errorf("DetectedSpec=%q want character-web", payload.DetectedSpec)
	}
	if payload.Confidence < 0.5 {
		t.Errorf("Confidence=%v want >= 0.5", payload.Confidence)
	}
}

// TestNormalize_UnrecognisedShape_FallsThrough verifies a body matching
// no known spec returns ErrUnsupported so the Coordinator can fall
// through to Tier 2.
func TestNormalize_UnrecognisedShape_FallsThrough(t *testing.T) {
	body := []byte(`{"foo":"bar","baz":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "character-web",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

// Internal helpers — looksLikeJSON + preview

func TestLooksLikeJSON(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", []byte(``), false},
		{"only-whitespace", []byte("  \t\n\r"), false},
		{"object", []byte(`{"a":1}`), true},
		{"array", []byte(`[1,2,3]`), true},
		{"leading-whitespace-object", []byte("  \n\t {\"a\":1}"), true},
		{"leading-whitespace-array", []byte("\r\n[1]"), true},
		{"text-prefix", []byte(`hello`), false},
		{"number-prefix", []byte(`42`), false},
		{"string-prefix", []byte(`"x"`), false},
		{"control-byte-prefix", []byte{0x00, '{'}, false},
	}
	for _, c := range cases {
		if got := looksLikeJSON(c.in); got != c.want {
			t.Errorf("%s: looksLikeJSON(%q)=%v want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestPreview pins the binary-safe sanitisation contract:
//   - control bytes < 0x20 (except \n and \t) become '.'
//   - bytes > 0x7e become '.'
//   - printable ASCII passes through
//   - inputs longer than 256 bytes are truncated to 256
func TestPreview(t *testing.T) {
	t.Run("short-printable-passthrough", func(t *testing.T) {
		if got := preview([]byte("hello world")); got != "hello world" {
			t.Errorf("got=%q want 'hello world'", got)
		}
	})
	t.Run("preserves-newline-and-tab", func(t *testing.T) {
		got := preview([]byte("a\nb\tc"))
		if got != "a\nb\tc" {
			t.Errorf("got=%q want 'a\\nb\\tc' (newline+tab preserved)", got)
		}
	})
	t.Run("scrubs-control-bytes", func(t *testing.T) {
		got := preview([]byte{'a', 0x07, 'b', 0x0d, 'c', 0x1b, 'd'})
		if got != "a.b.c.d" {
			t.Errorf("got=%q want 'a.b.c.d'", got)
		}
	})
	t.Run("scrubs-high-bytes", func(t *testing.T) {
		got := preview([]byte{'a', 0x7f, 'b', 0x80, 'c', 0xff, 'd'})
		if got != "a.b.c.d" {
			t.Errorf("got=%q want 'a.b.c.d' (>0x7e scrubbed)", got)
		}
	})
	t.Run("truncates-over-256-bytes", func(t *testing.T) {
		body := make([]byte, 300)
		for i := range body {
			body[i] = 'A'
		}
		got := preview(body)
		if len(got) != 256 {
			t.Errorf("len=%d want 256 (truncated)", len(got))
		}
	})
	t.Run("exactly-256-bytes-passes-through", func(t *testing.T) {
		body := make([]byte, 256)
		for i := range body {
			body[i] = 'B'
		}
		got := preview(body)
		if len(got) != 256 {
			t.Errorf("len=%d want 256", len(got))
		}
	})
	t.Run("empty-input", func(t *testing.T) {
		if got := preview(nil); got != "" {
			t.Errorf("got=%q want empty", got)
		}
	})
}
