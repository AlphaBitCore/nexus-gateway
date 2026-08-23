package dispatch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// capturingTransport records the bytes handed to the transport — the last thing
// that happens before the socket. Asserting here rather than on a leg's return
// value is the whole point: a guarantee tested on one leg is a guarantee for that
// leg, and the defect this file exists for was a leg that skipped the leg that
// was tested.
type capturingTransport struct{ body []byte }

func (c *capturingTransport) BuildURL(CallTarget, typology.WireShape, bool) (string, error) {
	return "https://upstream.invalid/v1/chat/completions", nil
}
func (c *capturingTransport) ApplyAuth(*http.Request, CallTarget) error { return nil }
func (c *capturingTransport) Probe(context.Context, CallTarget) (*ProbeResult, error) {
	return &ProbeResult{OK: true}, nil
}
func (c *capturingTransport) Do(_ context.Context, r *http.Request, _ CallTarget) (*http.Response, error) {
	if r.Body != nil {
		c.body, _ = io.ReadAll(r.Body)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"id":"x","choices":[]}`))),
	}, nil
}

// passthroughCodec is the shape that caused the incident: a codec that returns
// the canonical body essentially as it received it. Every OpenAI-family adapter
// is one of these, and Cohere's chat codec was one until it grew a declared emit
// set. If the egress guarantee holds for this codec it holds for the careful ones.
type passthroughCodec struct{}

func (passthroughCodec) EncodeRequest(_ typology.WireShape, body []byte, _ CallTarget) (EncodeResult, error) {
	return EncodeResult{Body: body, ContentType: "application/json"}, nil
}
func (passthroughCodec) DecodeResponse(_ typology.WireShape, body []byte, _ string, _ DecodeContext) (DecodeResult, error) {
	return DecodeResult{CanonicalBody: body}, nil
}
func (passthroughCodec) RewriteNative(_ typology.WireShape, body []byte, _ CallTarget, _ bool) (EncodeResult, error) {
	return EncodeResult{Body: body, ContentType: "application/json"}, nil
}

// The remaining spec members exist only to satisfy AdapterSpec.Valid(); the
// egress assertion never reaches them.
type nopStreamDecoder struct{}

func (nopStreamDecoder) Open(io.ReadCloser, typology.WireShape) (StreamSession, error) {
	return nil, nil
}

type nopErrorNormalizer struct{}

func (nopErrorNormalizer) Normalize(int, http.Header, []byte) *ProviderError { return nil }

// carrierBody is a canonical body carrying both internal carriers plus a caller
// tool schema whose property names are the caller's own.
const carrierBody = `{"model":"m",` +
	`"nexus":{"ext":{"cohere":{"input_type":"search_document"}}},` +
	`"messages":[{"role":"assistant","content":"hi","reasoning_content":"why",` +
	`"nexus_thinking":[{"thinking":"why","signature":"sig-1"}],` +
	`"tool_calls":[{"id":"call-1","type":"function","function":{"name":"f","arguments":"{}","thought_signature":"sig-gem"}}]}],` +
	`"tools":[{"type":"function","function":{"name":"f","parameters":` +
	`{"type":"object","properties":{"nexus_id":{"type":"string"}},"required":["nexus_id"]}}}]}`

func captureEgress(t *testing.T, bodyFormat Format) []byte {
	t.Helper()
	tr := &capturingTransport{}
	a := NewSpecAdapter(AdapterSpec{
		Format:          FormatOpenAI,
		Transport:       tr,
		SchemaCodec:     passthroughCodec{},
		StreamDecoder:   nopStreamDecoder{},
		ErrorNormalizer: nopErrorNormalizer{},
		RequestShapes:   []typology.WireShape{typology.WireShapeOpenAIChat},
	}, slog.Default())

	_, err := a.Execute(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		Body:       []byte(carrierBody),
		BodyFormat: bodyFormat,
		Target:     CallTarget{Format: FormatOpenAI, ProviderModelID: "provider-model-id"},
	})
	if err != nil {
		t.Fatalf("Execute returned %v", err)
	}
	if len(tr.body) == 0 {
		t.Fatal("transport received no body; the capture proved nothing")
	}
	return tr.body
}

// TestEgress_NoInternalCarrierReachesTheTransport asserts at the frame where the
// guarantee actually lives.
//
// The history is the argument. The strip first lived in the codecs, and Cohere's
// forwarded the namespace to api.cohere.com — HTTP 422. It then moved to
// PrepareBody and was called one dispatcher guarantee; the cross-format failover
// leg builds its body in canonicalbridge and hands it to ExecuteWithBody, never
// touching PrepareBody, and kept sending the namespace upstream. Each fix was
// tested where it was installed, which is why each one looked complete.
//
// Both legs are exercised: BodyFormat equal to the adapter's format takes the
// native leg, a different one takes the cross-format leg.
func TestEgress_NoInternalCarrierReachesTheTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		fmt  Format
	}{
		{"native leg", FormatOpenAI},
		{"cross-format leg", FormatAnthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureEgress(t, tc.fmt)
			for _, carrier := range []string{`"nexus"`, `"nexus_thinking"`, `"thought_signature"`} {
				if bytes.Contains(out, []byte(carrier)) {
					t.Errorf("%s reached the transport: %s", carrier, out)
				}
			}
		})
	}
}

func TestEgress_ExecuteWithBody_NoThoughtSignatureReachesTransport(t *testing.T) {
	tr := &capturingTransport{}
	a := NewSpecAdapter(AdapterSpec{
		Format:          FormatOpenAI,
		Transport:       tr,
		SchemaCodec:     passthroughCodec{},
		StreamDecoder:   nopStreamDecoder{},
		ErrorNormalizer: nopErrorNormalizer{},
		RequestShapes:   []typology.WireShape{typology.WireShapeOpenAIChat},
	}, slog.Default())
	_, err := a.ExecuteWithBody(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatAnthropic,
		Target:     CallTarget{Format: FormatOpenAI, ProviderModelID: "provider-model-id"},
	}, []byte(carrierBody), nil, "")
	if err != nil {
		t.Fatalf("ExecuteWithBody returned %v", err)
	}
	if bytes.Contains(tr.body, []byte(`"thought_signature"`)) {
		t.Fatalf("thought_signature reached ExecuteWithBody transport: %s", tr.body)
	}
}

func TestEgress_ExecuteWithBody_NoEscapedThoughtSignatureReachesTransport(t *testing.T) {
	tr := &capturingTransport{}
	a := NewSpecAdapter(AdapterSpec{
		Format:          FormatOpenAI,
		Transport:       tr,
		SchemaCodec:     passthroughCodec{},
		StreamDecoder:   nopStreamDecoder{},
		ErrorNormalizer: nopErrorNormalizer{},
		RequestShapes:   []typology.WireShape{typology.WireShapeOpenAIChat},
	}, slog.Default())
	body := []byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"call-1","type":"function","function":{"name":"f","arguments":"{}",` +
		`"thought_\u0073ignature":"sig-gem"}}]}]}`)
	_, err := a.ExecuteWithBody(context.Background(), Request{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: FormatAnthropic,
		Target:     CallTarget{Format: FormatOpenAI, ProviderModelID: "provider-model-id"},
	}, body, nil, "")
	if err != nil {
		t.Fatalf("ExecuteWithBody returned %v", err)
	}
	if gjson.GetBytes(tr.body, "messages.0.tool_calls.0.function.thought_signature").Exists() {
		t.Fatalf("escaped thought_signature reached ExecuteWithBody transport: %s", tr.body)
	}
}

// TestEgress_CallerSchemaSurvives is the half that stops a fix from being bought
// by mutating the caller's data. An earlier strip matched the reserved prefix at
// any depth and deleted a caller's tool-schema property while leaving `required`
// pointing at it — an invalid schema, a model answering about a contract it never
// received, HTTP 200, every byte assertion green.
func TestEgress_CallerSchemaSurvives(t *testing.T) {
	for _, f := range []Format{FormatOpenAI, FormatAnthropic} {
		out := captureEgress(t, f)
		if !gjson.GetBytes(out, "tools.0.function.parameters.properties.nexus_id").Exists() {
			t.Errorf("BodyFormat=%s: the caller's tool-schema property was deleted: %s", f, out)
		}
	}
}

// TestEgress_CanonicalContentSurvives pins that the strip removes our envelope
// and nothing else — `reasoning_content` is a canonical message field several
// targets consume, not an internal carrier.
func TestEgress_CanonicalContentSurvives(t *testing.T) {
	out := captureEgress(t, FormatAnthropic)
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "why" {
		t.Errorf("reasoning_content = %q, want %q — canonical content must not be stripped", got, "why")
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Errorf("message content = %q, want %q", got, "hi")
	}
}
