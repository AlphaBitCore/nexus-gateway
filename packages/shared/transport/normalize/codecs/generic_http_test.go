package codecs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestGenericHTTP_JSON_Object(t *testing.T) {
	n := NewGenericHTTPNormalizer()
	body := []byte(`{"name": "Alice", "age": 30}`)
	got, err := n.Normalize(context.Background(), body, core.Meta{ContentType: "application/json"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPJSON {
		t.Fatalf("Kind: %v want http-json", got.Kind)
	}
	if got.Protocol != "generic-http" {
		t.Fatalf("Protocol: %q", got.Protocol)
	}
	if got.HTTP == nil || got.HTTP.BodyView == nil || got.HTTP.BodyView.JSON == nil {
		t.Fatalf("BodyView.JSON not populated: %+v", got.HTTP)
	}
	m, ok := got.HTTP.BodyView.JSON.(map[string]any)
	if !ok || m["name"] != "Alice" {
		t.Fatalf("JSON shape wrong: %+v", got.HTTP.BodyView.JSON)
	}
}

func TestGenericHTTP_JSON_PlusSuffix(t *testing.T) {
	// application/vnd.api+json must also route to JSON.
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte(`{"data":{"id":"1"}}`),
		core.Meta{ContentType: "application/vnd.api+json"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPJSON {
		t.Fatalf("Kind: %v", got.Kind)
	}
}

func TestGenericHTTP_JSON_Malformed_FallsThroughToText(t *testing.T) {
	// A body claiming application/json that fails to decode and
	// looks like UTF-8 text routes to http-text instead of remaining a
	// half-populated http-json. The previous behaviour kept Kind=http-json
	// + only BodyView.Text — the UI's http-json renderer reads BodyView.JSON
	// and showed "(empty)".
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte(`{"trailing":`),
		core.Meta{ContentType: "application/json"},
	)
	if err != nil {
		t.Fatalf("unexpected err for json-looks-like-text fallback: %v", err)
	}
	if got.Kind != core.KindHTTPText {
		t.Fatalf("Kind: %v want http-text", got.Kind)
	}
	if got.HTTP.BodyView.Text != `{"trailing":` {
		t.Fatalf("Text projection lost, got %+v", got.HTTP.BodyView)
	}
}

func TestGenericHTTP_JSON_Malformed_BinaryFallback(t *testing.T) {
	// Genuinely non-text bytes claiming to be JSON: fall through to a
	// binary ref (size + sha256), don't inline the body. Keep the
	// non-nil error so the audit row records "partial".
	body := []byte{0x00, 0x01, 0xff, 0xfe, 0xab, 0xcd, 0xef}
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "application/json"},
	)
	if err == nil {
		t.Fatal("expected non-nil error for binary-with-json-CT")
	}
	if got.Kind != core.KindHTTPBinary {
		t.Fatalf("Kind: %v want http-binary", got.Kind)
	}
	if got.HTTP.BodyView.BinaryRef == nil {
		t.Fatalf("BinaryRef not populated: %+v", got.HTTP.BodyView)
	}
	if got.HTTP.BodyView.BinaryRef.Size != int64(len(body)) {
		t.Fatalf("Size: %d want %d", got.HTTP.BodyView.BinaryRef.Size, len(body))
	}
}

func TestGenericHTTP_Text(t *testing.T) {
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte("hello, world"),
		core.Meta{ContentType: "text/plain"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPText {
		t.Fatalf("Kind: %v", got.Kind)
	}
	if got.HTTP.BodyView.Text != "hello, world" {
		t.Fatalf("Text: %q", got.HTTP.BodyView.Text)
	}
}

func TestGenericHTTP_Text_NoContentType_Sniffed(t *testing.T) {
	// Producer didn't set Content-Type. Bytes look like UTF-8 text →
	// route to http-text.
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte("plain ascii payload"),
		core.Meta{},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPText {
		t.Fatalf("Kind: %v", got.Kind)
	}
}

func TestGenericHTTP_Form(t *testing.T) {
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte("name=Alice&age=30&tag=a&tag=b"),
		core.Meta{ContentType: "application/x-www-form-urlencoded"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPForm {
		t.Fatalf("Kind: %v", got.Kind)
	}
	form := got.HTTP.BodyView.Form
	if form["name"] != "Alice" || form["age"] != "30" {
		t.Fatalf("Form: %+v", form)
	}
	if form["tag"] != "a\nb" {
		t.Fatalf("multi-valued form key: %q want %q", form["tag"], "a\nb")
	}
}

func TestGenericHTTP_Multipart(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("name", "Alice"); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("avatar", "pic.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nFAKE-PNG-BYTES"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	ct := "multipart/form-data; boundary=" + w.Boundary()
	got, err := NewGenericHTTPNormalizer().Normalize(context.Background(), buf.Bytes(), core.Meta{ContentType: ct})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPMultipart {
		t.Fatalf("Kind: %v", got.Kind)
	}
	form := got.HTTP.BodyView.Form
	if form["name"] != "Alice" {
		t.Fatalf("text field: %q", form["name"])
	}
	if !strings.HasPrefix(form["avatar"], "<file len=") {
		t.Fatalf("file part should decay to <file len=...>, got: %q", form["avatar"])
	}
}

func TestGenericHTTP_Multipart_NoBoundary_FallsToBinary(t *testing.T) {
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte("--xx\r\nContent-Disposition: form-data; name=\"x\"\r\n\r\nhi\r\n--xx--\r\n"),
		core.Meta{ContentType: "multipart/form-data"}, // missing boundary param
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPMultipart {
		t.Fatalf("Kind: %v", got.Kind)
	}
	if got.HTTP.BodyView.BinaryRef == nil {
		t.Fatalf("expected core.BinaryRef fallback when boundary missing")
	}
}

func TestGenericHTTP_Binary(t *testing.T) {
	body := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "image/png"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPBinary {
		t.Fatalf("Kind: %v", got.Kind)
	}
	if got.HTTP.BodyView.BinaryRef == nil {
		t.Fatalf("BinaryRef nil")
	}
	if got.HTTP.BodyView.BinaryRef.ContentType != "image/png" {
		t.Fatalf("ContentType: %q", got.HTTP.BodyView.BinaryRef.ContentType)
	}
	expectedSum := sha256.Sum256(body)
	if got.HTTP.BodyView.BinaryRef.SHA256 != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("SHA256 mismatch")
	}
}

func TestGenericHTTP_Binary_NoContentType(t *testing.T) {
	// Bytes contain a NUL → sniffer should classify as binary.
	body := []byte{0x00, 0x01, 0x02, 0x03}
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPBinary {
		t.Fatalf("Kind: %v want http-binary", got.Kind)
	}
	if got.HTTP.BodyView.BinaryRef.ContentType != "application/octet-stream" {
		t.Fatalf("default ContentType: %q", got.HTTP.BodyView.BinaryRef.ContentType)
	}
}

func TestGenericHTTP_OversizeNonText_BinaryOnly(t *testing.T) {
	n := NewGenericHTTPNormalizer()
	n.MaxInlineTextBytes = 8
	body := []byte("xxxxxxxxxxxxxxxxxxxxxxxxx")
	got, err := n.Normalize(context.Background(), body, core.Meta{ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPBinary {
		t.Fatalf("Kind: %v want http-binary", got.Kind)
	}
}

func TestGenericHTTP_OversizeText_StillInlined(t *testing.T) {
	// A text content-type larger than MaxInlineTextBytes should still
	// be inlined as text (the cap only triggers binary fallback for
	// non-text media types — text is the canonical projection we want).
	n := NewGenericHTTPNormalizer()
	n.MaxInlineTextBytes = 8
	body := []byte("hello world this is text")
	got, err := n.Normalize(context.Background(), body, core.Meta{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPText {
		t.Fatalf("Kind: %v want http-text", got.Kind)
	}
	if got.HTTP.BodyView.Text != string(body) {
		t.Fatalf("Text: %q", got.HTTP.BodyView.Text)
	}
}

func TestGenericHTTP_EmptyBody(t *testing.T) {
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		nil,
		core.Meta{ContentType: "application/json"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPText {
		t.Fatalf("Kind: %v want http-text (zero-body fallback)", got.Kind)
	}
	if got.HTTP.BodyView.Text != "" || got.HTTP.BodyView.JSON != nil || got.HTTP.BodyView.BinaryRef != nil {
		t.Fatalf("expected empty BodyView, got %+v", got.HTTP.BodyView)
	}
}

func TestGenericHTTP_ID(t *testing.T) {
	if id := NewGenericHTTPNormalizer().ID(); id != "generic-http" {
		t.Fatalf("ID: %q", id)
	}
}

func TestGenericHTTP_ContentTypeWithCharset(t *testing.T) {
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		[]byte(`{"x":1}`),
		core.Meta{ContentType: "application/json; charset=utf-8"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPJSON {
		t.Fatalf("Kind: %v", got.Kind)
	}
}

func TestGenericHTTP_RegistryFallback(t *testing.T) {
	reg := core.NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	// Empty AdapterType with plain JSON content-type should hit the
	// "*:*:*" fallback and resolve to the generic normalizer.
	n := reg.Resolve(core.Meta{AdapterType: "", ContentType: "application/json", EndpointPath: "/foo"})
	if n == nil {
		t.Fatal("expected generic-http fallback to resolve")
	}
	if n.ID() != "generic-http" {
		t.Fatalf("got %q, want generic-http", n.ID())
	}
}

func TestGenericHTTP_RegistryNormalize_NonAITraffic(t *testing.T) {
	reg := core.NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	payload, err := reg.Normalize(context.Background(),
		[]byte(`{"slack_webhook":"value"}`),
		core.Meta{AdapterType: "", ContentType: "application/json", EndpointPath: "/hooks/services/x"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if payload.Kind != core.KindHTTPJSON {
		t.Fatalf("Kind: %v want http-json", payload.Kind)
	}
	if payload.Protocol != "generic-http" {
		t.Fatalf("Protocol: %q", payload.Protocol)
	}
}

func TestGenericHTTP_NoFalsePositives_AnthropicStillBeatsFallback(t *testing.T) {
	// Sanity: when an adapter type IS provided, it must win over the
	// "*:*:*" fallback. Registers anthropic + generic; resolves with
	// AdapterType="anthropic" and expects anthropic-messages.
	reg := core.NewRegistry()
	RegisterDefaultAIBuiltins(reg)
	reg.Freeze()

	n := reg.Resolve(core.Meta{AdapterType: "anthropic", ContentType: "application/json"})
	if n == nil || n.ID() != "anthropic-messages" {
		t.Fatalf("expected anthropic-messages, got %v", n)
	}
}

// Compile-time pin: GenericHTTPNormalizer must satisfy the core.Normalizer
// interface so the registry can store it under the *:*:* key.
var _ core.Normalizer = (*GenericHTTPNormalizer)(nil)

// Smoke: the registered fallback must successfully produce a non-nil
// payload for every kind branch a real prod consumer might hit.
func TestGenericHTTP_AllBranchesProduceValidJSONPayload(t *testing.T) {
	type tc struct {
		name string
		ct   string
		body []byte
		kind core.Kind
	}
	cases := []tc{
		{"json", "application/json", []byte(`{"x":1}`), core.KindHTTPJSON},
		{"text", "text/plain", []byte("hi"), core.KindHTTPText},
		{"form", "application/x-www-form-urlencoded", []byte("a=b"), core.KindHTTPForm},
		{"binary", "application/octet-stream", []byte{0xFF, 0xFE}, core.KindHTTPBinary},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NewGenericHTTPNormalizer().Normalize(context.Background(), c.body, core.Meta{ContentType: c.ct})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got.Kind != c.kind {
				t.Fatalf("Kind: %v want %v", got.Kind, c.kind)
			}
			if got.NormalizeVersion != core.SchemaVersion {
				t.Fatalf("NormalizeVersion: %q", got.NormalizeVersion)
			}
		})
	}
}

// SSE + NDJSON byte-sniff robustness

// TestGenericHTTP_SSE_Detected exercises the canonical case that
// regressed baa07c15: a chatgpt.com SSE response stamped with
// `application/json` by the audit envelope. With the byte-sniff in
// place, the body routes to http-text + a verbatim dump that the UI
// can render — instead of the previous half-populated http-json with
// an empty BodyView.JSON.
func TestGenericHTTP_SSE_Detected(t *testing.T) {
	// Truncated but representative slice of the chatgpt.com SSE that
	// produced traffic_event baa07c15. The leading "event:" frame is
	// what the sniffer keys on.
	body := []byte(`event: delta_encoding
data: "v1"

event: delta
data: {"v":{"message":{"id":"abc","author":{"role":"user"},"content":{"content_type":"text","parts":["hello"]}}}}

event: delta
data: {"p": "/message/content/parts/0", "o": "append", "v": "A few that stand out"}

data: [DONE]
`)
	cases := []struct {
		name string
		ct   string
	}{
		{"mis-stamped-as-json", "application/json"},
		{"correct-event-stream", "text/event-stream"},
		{"unset-content-type", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewGenericHTTPNormalizer().Normalize(
				context.Background(),
				body,
				core.Meta{ContentType: tc.ct},
			)
			if err != nil {
				t.Fatalf("unexpected err for %s: %v", tc.name, err)
			}
			if got.Kind != core.KindHTTPText {
				t.Fatalf("Kind: %v want http-text", got.Kind)
			}
			if got.Protocol != "generic-http" {
				t.Fatalf("Protocol: %q", got.Protocol)
			}
			if got.HTTP == nil || got.HTTP.BodyView == nil {
				t.Fatalf("BodyView missing: %+v", got.HTTP)
			}
			if got.HTTP.BodyView.JSON != nil {
				t.Fatalf("BodyView.JSON populated on SSE — should be nil: %+v", got.HTTP.BodyView.JSON)
			}
			// The user-visible chat content must survive the projection.
			// Either the user's prompt or the assistant's partial reply
			// is enough to prove the dump round-tripped.
			if !strings.Contains(got.HTTP.BodyView.Text, "hello") {
				t.Fatalf("Text lost user prompt: %q", got.HTTP.BodyView.Text)
			}
			if !strings.Contains(got.HTTP.BodyView.Text, "A few that stand out") {
				t.Fatalf("Text lost assistant delta: %q", got.HTTP.BodyView.Text)
			}
		})
	}
}

func TestGenericHTTP_NDJSON_Detected(t *testing.T) {
	// Two independent JSON objects, one per line. Common shape from
	// bulk-export endpoints and some Gemini streaming wrappers.
	body := []byte(`{"id": 1, "name": "alice"}
{"id": 2, "name": "bob"}
{"id": 3, "name": "carol"}`)
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "application/x-ndjson"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPJSON {
		t.Fatalf("Kind: %v want http-json", got.Kind)
	}
	arr, ok := got.HTTP.BodyView.JSON.([]any)
	if !ok {
		t.Fatalf("BodyView.JSON not an array: %T", got.HTTP.BodyView.JSON)
	}
	if len(arr) != 3 {
		t.Fatalf("len: %d want 3 (%+v)", len(arr), arr)
	}
}

func TestGenericHTTP_NDJSON_OneBadLineFallsThroughToText(t *testing.T) {
	// First two lines parse, the third is broken: classification fails
	// and we route to plain text so the operator still sees the body.
	body := []byte(`{"id": 1}
{"id": 2}
{"id":`)
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: ""},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPText {
		t.Fatalf("Kind: %v want http-text", got.Kind)
	}
	if !strings.Contains(got.HTTP.BodyView.Text, `{"id":`) {
		t.Fatalf("Text fallback missing original body: %q", got.HTTP.BodyView.Text)
	}
}

func TestGenericHTTP_NDJSON_SingleLineRoutesAsJSON(t *testing.T) {
	// A single-line valid JSON document is JSON, not NDJSON. We must
	// preserve the single-object shape so the UI's tree view renders
	// correctly (instead of wrapping it in a one-element array).
	body := []byte(`{"only": "one"}`)
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "application/json"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPJSON {
		t.Fatalf("Kind: %v want http-json", got.Kind)
	}
	m, ok := got.HTTP.BodyView.JSON.(map[string]any)
	if !ok || m["only"] != "one" {
		t.Fatalf("Single-line JSON lost shape: %+v", got.HTTP.BodyView.JSON)
	}
}

func TestGenericHTTP_RealJSON_NotMisclassifiedAsNDJSON(t *testing.T) {
	// A pretty-printed JSON document with newlines must NOT be
	// classified as NDJSON — the first line `{` is incomplete on its
	// own and the NDJSON sniffer would fail to decode it as a full
	// object, so it correctly falls through to the JSON branch.
	body := []byte(`{
	"first": "alice",
	"nested": {"k": "v"}
}`)
	got, err := NewGenericHTTPNormalizer().Normalize(
		context.Background(),
		body,
		core.Meta{ContentType: "application/json"},
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Kind != core.KindHTTPJSON {
		t.Fatalf("Kind: %v want http-json", got.Kind)
	}
	m, ok := got.HTTP.BodyView.JSON.(map[string]any)
	if !ok || m["first"] != "alice" {
		t.Fatalf("Pretty JSON shape wrong: %+v", got.HTTP.BodyView.JSON)
	}
}

// Catch unintentional reliance on core.ErrUnsupported from this normalizer —
// generic-http claims everything and must not propagate core.ErrUnsupported.
func TestGenericHTTP_NeverReturnsErrUnsupported(t *testing.T) {
	cases := []struct {
		ct   string
		body []byte
	}{
		{"application/json", []byte(`{}`)},
		{"text/plain", []byte("x")},
		{"application/x-www-form-urlencoded", []byte("a=b")},
		{"image/png", []byte{0x89, 0x50}},
		{"", nil},
		{"", []byte("plain")},
	}
	for _, c := range cases {
		_, err := NewGenericHTTPNormalizer().Normalize(context.Background(), c.body, core.Meta{ContentType: c.ct})
		if err != nil && errors.Is(err, core.ErrUnsupported) {
			t.Fatalf("ct=%q: must not return core.ErrUnsupported", c.ct)
		}
	}
}
