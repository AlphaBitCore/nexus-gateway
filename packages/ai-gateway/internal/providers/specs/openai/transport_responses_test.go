// Tests for the OpenAI adapter transport, error normalizer, and Responses
// codec — exercising branches in:
//   - errors.go: parseRetryAfter HTTP-date forms; Normalize fallbacks
//     (5xx default, 408 timeout, 404 → invalid_request, code-without-type
//     promotion, empty body Message default, default-arm 200-class).
//   - transport.go: BuildURL empty BaseURL + unknown endpoint;
//     PathForEndpoint legacy completions + responses + unknown;
//     Probe happy path + empty BaseURL + non-2xx + bad URL.
//   - codec_responses.go: instructions-only (no input) decode;
//     buildMessagesFromInput function_call_output role; flat-shape
//     function tool partition; non-tool/non-flat passthrough on encode
//     side; refusal / input_image / input_audio / input_file /
//     unknown content-part normalization; encode-side content-array
//     path; tool_choice + response_format on encode.
//   - codec_responses_response.go: refusal content part on decode;
//     incomplete (content_filter) finish_reason; failed status →
//     finish_reason stop; mapFinishReasonToResponsesStatus unknown +
//     max_tokens; mapFinishReasonToResponsesIncompleteReason
//     unknown branch; buildCanonicalUsage returns nil when usage absent;
//     buildResponsesUsage returns nil when usage absent; firstNonEmpty
//     empty-args fallback; EncodeResponsesResponse with tool_calls
//     branch + nil-id fallback.
//   - stream.go: NewStreamDecoder nil-log fallback; Open nil body;
//     Next ctx.Err() short-circuit; empty data frame skipped;
//     tool_call delta with index.
//   - stream_responses.go: refusal.delta one-shot;
//     [DONE] terminal defensive; ctx.Err() short-circuit; explicit done
//     re-entry returns EOF; output_text.delta with empty delta skipped;
//     reasoning delta with empty skipped.
//   - spec.go: NewSpec with nil logger.
//   - responses_builtin_tools.go: IsResponsesBuiltinTool positive +
//     negative + empty.
//
// All assertions check observable behaviour (exact wire bytes, code
// classification, error chain). No err==nil padding.
package openai_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// errors.go — Normalize + parseRetryAfter branches

// TestErrorNormalize_AllStatusBranches covers every documented arm of
// the status switch in errorNormalizer.Normalize so a regression in
// the canonical Code mapping surfaces here, not only end-to-end.
func TestErrorNormalize_AllStatusBranches(t *testing.T) {
	norm := openai.ErrorNormalizerInstance()
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantType string
		wantMsg  string
	}{
		{
			name:     "400_invalid_request",
			status:   400,
			body:     `{"error":{"type":"invalid_request_error","message":"bad model"}}`,
			wantCode: provcore.CodeInvalidRequest,
			wantType: "invalid_request_error",
			wantMsg:  "bad model",
		},
		{
			name:     "403_auth_failed",
			status:   403,
			body:     `{"error":{"type":"forbidden","message":"forbidden"}}`,
			wantCode: provcore.CodeAuthFailed,
		},
		{
			name:     "408_timeout",
			status:   408,
			body:     `{"error":{"type":"timeout","message":"timeout"}}`,
			wantCode: provcore.CodeTimeout,
		},
		{
			name:     "504_gateway_timeout",
			status:   504,
			body:     `{"error":{"type":"timeout","message":"upstream"}}`,
			wantCode: provcore.CodeTimeout,
		},
		{
			name:     "404_invalid_request",
			status:   404,
			body:     `{"error":{"type":"not_found","message":"model not found"}}`,
			wantCode: provcore.CodeInvalidRequest,
		},
		{
			name:     "500_upstream_default",
			status:   500,
			body:     `{"error":{"type":"server_error","message":"boom"}}`,
			wantCode: provcore.CodeUpstreamError,
		},
		{
			name:     "418_unknown_falls_through",
			status:   418,
			body:     `{"error":{"type":"teapot"}}`,
			wantCode: provcore.CodeUpstreamError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := norm.Normalize(tc.status, http.Header{}, []byte(tc.body))
			if pe == nil {
				t.Fatalf("nil ProviderError")
			}
			if pe.Code != tc.wantCode {
				t.Errorf("Code=%q want %q", pe.Code, tc.wantCode)
			}
			if pe.Status != tc.status {
				t.Errorf("Status=%d want %d", pe.Status, tc.status)
			}
			if tc.wantType != "" && pe.Type != tc.wantType {
				t.Errorf("Type=%q want %q", pe.Type, tc.wantType)
			}
			if tc.wantMsg != "" && pe.Message != tc.wantMsg {
				t.Errorf("Message=%q want %q", pe.Message, tc.wantMsg)
			}
		})
	}
}

// TestErrorNormalize_CodePromotedToType pins the "code-without-type"
// promotion: when error.type is empty but error.code is present,
// ProviderError.Type carries the code value. Some OpenAI-compat
// upstreams (older DeepSeek) emit only `code`.
func TestErrorNormalize_CodePromotedToType(t *testing.T) {
	norm := openai.ErrorNormalizerInstance()
	body := []byte(`{"error":{"code":"deepseek_overload","message":"try later"}}`)
	pe := norm.Normalize(429, http.Header{}, body)
	if pe.Type != "deepseek_overload" {
		t.Errorf("Type=%q want deepseek_overload (code promoted)", pe.Type)
	}
}

// TestErrorNormalize_NoMessageFallsBackToStatusText covers the
// "if pe.Message == \"\" { pe.Message = http.StatusText(status) }"
// branch — body has no error envelope at all.
func TestErrorNormalize_NoMessageFallsBackToStatusText(t *testing.T) {
	norm := openai.ErrorNormalizerInstance()
	pe := norm.Normalize(503, http.Header{}, []byte(`{}`))
	if pe.Message != http.StatusText(http.StatusServiceUnavailable) {
		t.Errorf("Message=%q want %q (StatusText fallback)", pe.Message, http.StatusText(http.StatusServiceUnavailable))
	}
	if pe.Code != provcore.CodeUpstreamError {
		t.Errorf("503 must classify as upstream_error, got %q", pe.Code)
	}
}

// TestParseRetryAfter_DateForms covers the http.ParseTime arm of
// parseRetryAfter (reached through Normalize on 429). Two forms:
// future date → positive duration; past date → clamped to zero.
func TestParseRetryAfter_DateForms(t *testing.T) {
	norm := openai.ErrorNormalizerInstance()

	t.Run("future_http_date", func(t *testing.T) {
		future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
		h := http.Header{}
		h.Set("Retry-After", future)
		pe := norm.Normalize(429, h, []byte(`{"error":{"type":"rate_limit"}}`))
		if pe.RetryAfter == nil {
			t.Fatalf("expected RetryAfter set for future HTTP-date %q", future)
		}
		if *pe.RetryAfter <= 0 || *pe.RetryAfter > 60*time.Second {
			t.Errorf("RetryAfter %v out of range [0,60s]", *pe.RetryAfter)
		}
	})

	t.Run("past_http_date_clamped_to_zero", func(t *testing.T) {
		past := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
		h := http.Header{}
		h.Set("Retry-After", past)
		pe := norm.Normalize(429, h, []byte(`{"error":{"type":"rate_limit"}}`))
		if pe.RetryAfter == nil {
			t.Fatalf("expected RetryAfter set even for past HTTP-date")
		}
		if *pe.RetryAfter != 0 {
			t.Errorf("past date must clamp to 0, got %v", *pe.RetryAfter)
		}
	})

	t.Run("garbage_returns_nil_retry_after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "not-a-number-or-date")
		pe := norm.Normalize(429, h, []byte(`{"error":{"type":"rate_limit"}}`))
		if pe.RetryAfter != nil {
			t.Errorf("garbage Retry-After must yield nil, got %v", *pe.RetryAfter)
		}
	})

	t.Run("negative_seconds_returns_nil", func(t *testing.T) {
		// parseRetryAfter's integer arm requires secs >= 0; negative
		// values fail both arms and return nil.
		h := http.Header{}
		h.Set("Retry-After", "-5")
		pe := norm.Normalize(429, h, []byte(`{"error":{"type":"rate_limit"}}`))
		if pe.RetryAfter != nil {
			t.Errorf("negative seconds must yield nil RetryAfter, got %v", *pe.RetryAfter)
		}
	})
}

// transport.go — BuildURL / PathForEndpoint / Probe

// TestTransport_BuildURL_EmptyBaseURL pins the explicit
// "BaseURL is empty" guard so a CallTarget without BaseURL fails fast
// rather than producing a malformed URL.
func TestTransport_BuildURL_EmptyBaseURL(t *testing.T) {
	transport := openai.NewTransport(slog.Default())
	if _, err := transport.BuildURL(provcore.CallTarget{}, typology.WireShapeOpenAIChat, false); err == nil {
		t.Fatal("expected error on empty BaseURL")
	}
}

// TestTransport_BuildURL_UnsupportedEndpoint pins that unknown endpoints
// return an error rather than building a URL with an empty path.
func TestTransport_BuildURL_UnsupportedEndpoint(t *testing.T) {
	transport := openai.NewTransport(slog.Default())
	tgt := provcore.CallTarget{BaseURL: "https://api.example.com/"}
	if _, err := transport.BuildURL(tgt, typology.WireShape("nope"), false); err == nil {
		t.Fatal("expected error on unsupported endpoint")
	}
}

// TestPathForEndpoint_AllArms covers every endpoint case + the unknown
// fallthrough so the path table can't silently lose an entry.
func TestPathForEndpoint_AllArms(t *testing.T) {
	cases := []struct {
		ep      typology.WireShape
		wantOK  bool
		wantStr string
	}{
		{typology.WireShapeOpenAIChat, true, "/v1/chat/completions"},
		{typology.WireShapeOpenAIEmbeddings, true, "/v1/embeddings"},
		{typology.WireShapeNone, true, "/v1/models"},
		{typology.WireShapeOpenAICompletionsLegacy, true, "/v1/completions"},
		{typology.WireShapeOpenAIResponses, true, "/v1/responses"},
		{typology.WireShape("does-not-exist"), false, ""},
	}
	for _, tc := range cases {
		got, ok := openai.PathForEndpoint(tc.ep)
		if ok != tc.wantOK {
			t.Errorf("PathForEndpoint(%q) ok=%v want %v", tc.ep, ok, tc.wantOK)
		}
		if got != tc.wantStr {
			t.Errorf("PathForEndpoint(%q) path=%q want %q", tc.ep, got, tc.wantStr)
		}
	}
}

// TestTransport_Probe_HappyPath covers the 2xx success branch of Probe.
func TestTransport_Probe_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe hit %q, want /v1/models", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer probe-key" {
			t.Errorf("Authorization header missing: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	transport := openai.NewTransport(slog.Default())
	res, err := transport.Probe(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "probe-key",
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res == nil || !res.OK {
		t.Fatalf("probe result not OK: %+v", res)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs must be >= 0, got %d", res.LatencyMs)
	}
}

// TestTransport_Probe_Non2xx covers the non-2xx response branch:
// upstream answers HTTP 401 → OK false, Detail "HTTP 401".
func TestTransport_Probe_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	transport := openai.NewTransport(slog.Default())
	res, err := transport.Probe(context.Background(), provcore.CallTarget{
		BaseURL: srv.URL,
		APIKey:  "wrong-key",
	})
	if err != nil {
		t.Fatalf("Probe (non-2xx must return no error, just OK=false): %v", err)
	}
	if res.OK {
		t.Errorf("probe must report OK=false on 401")
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("Detail %q must include status 401", res.Detail)
	}
}

// TestTransport_Probe_EmptyBaseURL pins the explicit empty-BaseURL branch.
func TestTransport_Probe_EmptyBaseURL(t *testing.T) {
	transport := openai.NewTransport(slog.Default())
	res, err := transport.Probe(context.Background(), provcore.CallTarget{})
	if err != nil {
		t.Fatalf("Probe with empty BaseURL must return result+nil err, got err=%v", err)
	}
	if res.OK {
		t.Errorf("empty BaseURL must report OK=false")
	}
	if !strings.Contains(strings.ToLower(res.Detail), "baseurl") {
		t.Errorf("Detail must mention BaseURL, got %q", res.Detail)
	}
}

// TestTransport_Probe_DialError exercises the err != nil branch of
// the HTTP client Do() call by pointing at a closed port.
func TestTransport_Probe_DialError(t *testing.T) {
	// httptest server we close immediately gives us a guaranteed-closed
	// port without OS-specific tricks.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	transport := openai.NewTransport(slog.Default())
	res, err := transport.Probe(context.Background(), provcore.CallTarget{BaseURL: url})
	if err != nil {
		t.Fatalf("Probe must always return nil err, got %v", err)
	}
	if res.OK {
		t.Errorf("connection refused must report OK=false")
	}
	if res.Err == nil {
		t.Errorf("Err field must carry the underlying dial error")
	}
}

// codec_responses.go — request decode/encode rare branches

// TestDecodeResponsesRequest_InstructionsOnlyNoInput covers the
// "instructions present, no input" branch (else-arm of the input
// length check) which synthesizes a system-only messages array.
func TestDecodeResponsesRequest_InstructionsOnlyNoInput(t *testing.T) {
	in := []byte(`{"model":"gpt-5","instructions":"Greet the user."}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 system message, got %d; body=%s", len(msgs), string(out))
	}
	if msgs[0].Get("role").String() != "system" {
		t.Errorf("role=%q want system", msgs[0].Get("role").String())
	}
	if msgs[0].Get("content").String() != "Greet the user." {
		t.Errorf("content=%q want 'Greet the user.'", msgs[0].Get("content").String())
	}
}

// TestDecodeResponsesRequest_FunctionCallOutput covers the
// function_call_output → tool-role message branch in
// buildMessagesFromInput.
func TestDecodeResponsesRequest_FunctionCallOutput(t *testing.T) {
	in := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"function_call_output","call_id":"call_abc","output":"{\"temp\":72}"}
		]
	}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "tool" {
		t.Errorf("role=%q want tool", got)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_call_id").String(); got != "call_abc" {
		t.Errorf("tool_call_id=%q want call_abc", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != `{"temp":72}` {
		t.Errorf("content=%q want raw output", got)
	}
}

// TestDecodeResponsesRequest_StringContentInInputItem covers the
// Responses-API "input item with content as string" branch in
// buildMessagesFromInput (c.Type == gjson.String).
func TestDecodeResponsesRequest_StringContentInInputItem(t *testing.T) {
	in := []byte(`{"model":"m","input":[{"role":"user","content":"plain text"}]}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "plain text" {
		t.Errorf("content=%q want 'plain text'", got)
	}
}

// TestDecodeResponsesRequest_NoRoleDefaultsToUser covers the
// "role == \"\" → user" defensive branch.
func TestDecodeResponsesRequest_NoRoleDefaultsToUser(t *testing.T) {
	in := []byte(`{"model":"m","input":[{"content":[{"type":"input_text","text":"hi"}]}]}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Errorf("role=%q want user (default for missing role)", got)
	}
}

// TestDecodeResponsesRequest_NonArrayNonStringInput covers the
// "input exists but is neither string nor array" defensive branch:
// buildMessagesFromInput returns nil.
func TestDecodeResponsesRequest_NonArrayNonStringInput(t *testing.T) {
	in := []byte(`{"model":"m","input":42}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gjson.GetBytes(out, "messages").Exists() {
		t.Errorf("messages should be absent for non-string non-array input; body=%s", string(out))
	}
}

// TestNormalizeInputContentPart_AllKinds covers each branch of
// normalizeInputContentPart through Decode: input_image (top-level URL),
// input_image (nested URL), input_audio, input_file, refusal,
// output_text (echoed assistant), and unknown.
func TestNormalizeInputContentPart_AllKinds(t *testing.T) {
	// Each request body contains exactly one content part to make
	// assertions one-to-one.
	cases := []struct {
		name     string
		part     string
		wantType string
		wantText string // when type==text, expected text
		check    func(t *testing.T, out []byte)
	}{
		{
			name:     "input_image_top_level_url",
			part:     `{"type":"input_image","image_url":"https://x/y.png"}`,
			wantType: "image_url",
			check: func(t *testing.T, out []byte) {
				if got := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String(); got != "https://x/y.png" {
					t.Errorf("image_url.url=%q", got)
				}
			},
		},
		{
			name:     "input_image_nested_url",
			part:     `{"type":"input_image","image_url":{"url":"https://x/z.png"}}`,
			wantType: "image_url",
			check: func(t *testing.T, out []byte) {
				if got := gjson.GetBytes(out, "messages.0.content.0.image_url.url").String(); got != "https://x/z.png" {
					t.Errorf("image_url.url=%q", got)
				}
			},
		},
		{
			name:     "input_audio",
			part:     `{"type":"input_audio","input_audio":{"data":"base64...","format":"mp3"}}`,
			wantType: "input_audio",
			check: func(t *testing.T, out []byte) {
				if got := gjson.GetBytes(out, "messages.0.content.0.input_audio.format").String(); got != "mp3" {
					t.Errorf("input_audio.format=%q", got)
				}
			},
		},
		{
			name:     "input_file_preserved_as_text",
			part:     `{"type":"input_file","filename":"report.pdf"}`,
			wantType: "text",
			check: func(t *testing.T, out []byte) {
				txt := gjson.GetBytes(out, "messages.0.content.0.text").String()
				if !strings.Contains(txt, "report.pdf") {
					t.Errorf("input_file text marker missing filename; got %q", txt)
				}
			},
		},
		{
			name:     "refusal_surfaces_as_text",
			part:     `{"type":"refusal","refusal":"I will not."}`,
			wantType: "text",
			wantText: "I will not.",
		},
		{
			name:     "output_text_echo_treated_as_text",
			part:     `{"type":"output_text","text":"prior assistant turn"}`,
			wantType: "text",
			wantText: "prior assistant turn",
		},
		{
			name:     "unknown_part_object_preserved_verbatim",
			part:     `{"type":"some_brand_new_part","payload":{"k":"v"}}`,
			wantType: "some_brand_new_part",
			check: func(t *testing.T, out []byte) {
				if got := gjson.GetBytes(out, "messages.0.content.0.payload.k").String(); got != "v" {
					t.Errorf("unknown part payload not preserved; body=%s", string(out))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"model":"m","input":[{"role":"user","content":[` + tc.part + `]}]}`)
			out, err := openai.DecodeResponsesRequest(in)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != tc.wantType {
				t.Errorf("type=%q want %q; body=%s", got, tc.wantType, string(out))
			}
			if tc.wantText != "" {
				if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != tc.wantText {
					t.Errorf("text=%q want %q", got, tc.wantText)
				}
			}
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

// TestNormalizeFunctionTool_NestedShapeB pins the (B) shape passthrough:
// {type:"function", function:{name,...}} survives unchanged.
func TestNormalizeFunctionTool_NestedShapeB(t *testing.T) {
	in := []byte(`{
		"model":"m","input":"hi",
		"tools":[{"type":"function","function":{"name":"calc","parameters":{"type":"object"}}}]
	}`)
	out, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "calc" {
		t.Errorf("nested (B) shape name lost: %s", string(out))
	}
}

// TestEncodeResponsesRequest_ContentArrayWithImage exercises the
// canonical content-array path of responsesInputItemFromMessage +
// responsesContentPartFromCanonical (image_url branch).
func TestEncodeResponsesRequest_ContentArrayWithImage(t *testing.T) {
	canon := []byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"caption this"},
				{"type":"image_url","image_url":{"url":"https://x/a.png"}}
			]}
		]
	}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "input_text" {
		t.Errorf("text part type=%q want input_text", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.1.type").String(); got != "input_image" {
		t.Errorf("image part type=%q want input_image", got)
	}
	if got := gjson.GetBytes(out, "input.0.content.1.image_url").String(); got != "https://x/a.png" {
		t.Errorf("image_url not preserved: %s", string(out))
	}
}

// TestEncodeResponsesRequest_ImageURLTopLevelString covers the
// "image_url.url" empty → fallback to image_url string branch in
// responsesContentPartFromCanonical.
func TestEncodeResponsesRequest_ImageURLTopLevelString(t *testing.T) {
	canon := []byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":[{"type":"image_url","image_url":"https://x/top.png"}]}
		]
	}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.image_url").String(); got != "https://x/top.png" {
		t.Errorf("top-level image_url string fallback failed; body=%s", string(out))
	}
}

// TestEncodeResponsesRequest_UnknownContentPartPassthrough covers the
// default arm of responsesContentPartFromCanonical (unknown type).
func TestEncodeResponsesRequest_UnknownContentPartPassthrough(t *testing.T) {
	canon := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":[{"type":"weird","payload":42}]}]
	}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "input.0.content.0.type").String(); got != "weird" {
		t.Errorf("unknown part type not preserved; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "input.0.content.0.payload").Int(); got != 42 {
		t.Errorf("unknown part payload not preserved")
	}
}

// TestEncodeResponsesRequest_ToolMessage covers the tool-role branch of
// responsesInputItemFromMessage (emits function_call_output item).
func TestEncodeResponsesRequest_ToolMessage(t *testing.T) {
	canon := []byte(`{
		"model":"m",
		"messages":[{"role":"tool","tool_call_id":"call_xyz","content":"42"}]
	}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "input.0.type").String(); got != "function_call_output" {
		t.Errorf("tool role must encode as function_call_output, got %q", got)
	}
	if got := gjson.GetBytes(out, "input.0.call_id").String(); got != "call_xyz" {
		t.Errorf("call_id lost: %s", string(out))
	}
	if got := gjson.GetBytes(out, "input.0.output").String(); got != "42" {
		t.Errorf("output lost: %s", string(out))
	}
}

// TestEncodeResponsesRequest_MessageWithoutContent covers the
// "no content" return path of responsesInputItemFromMessage.
func TestEncodeResponsesRequest_MessageWithoutContent(t *testing.T) {
	canon := []byte(`{"model":"m","messages":[{"role":"assistant"}]}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "assistant" {
		t.Errorf("role lost on bare message: %s", string(out))
	}
	if gjson.GetBytes(out, "input.0.content").Exists() {
		t.Errorf("bare message must not synthesize content; body=%s", string(out))
	}
}

// TestEncodeResponsesRequest_NonFunctionToolPassthrough covers the
// "type != function" / no .function field branch of
// responsesToolFromCanonical — returns the raw map unchanged.
func TestEncodeResponsesRequest_NonFunctionToolPassthrough(t *testing.T) {
	canon := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search"}]
	}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "web_search" {
		t.Errorf("non-function tool not passed through; body=%s", string(out))
	}
}

// TestEncodeResponsesRequest_ToolChoiceAndResponseFormat covers the
// tool_choice + response_format → text.format passthrough on encode.
func TestEncodeResponsesRequest_ToolChoiceAndResponseFormat(t *testing.T) {
	canon := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"function","function":{"name":"f"}},
		"response_format":{"type":"json_schema","json_schema":{"name":"s"}}
	}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "function" {
		t.Errorf("tool_choice lost: %s", string(out))
	}
	if got := gjson.GetBytes(out, "text.format.type").String(); got != "json_schema" {
		t.Errorf("response_format → text.format conversion lost: %s", string(out))
	}
}

// TestEncodeResponsesRequest_BuiltinToolsFromExt covers the
// "builtin_tools from nexus.ext" branch of EncodeResponsesRequest.
func TestEncodeResponsesRequest_BuiltinToolsFromExt(t *testing.T) {
	// Round-trip a body that has web_search built-in. DecodeResponsesRequest
	// is the only producer of the nexus.ext.openai.responses.builtin_tools
	// path; assemble through it to mirror the real flow.
	in := []byte(`{
		"model":"m","input":"search",
		"tools":[{"type":"web_search"},{"type":"function","name":"f","parameters":{}}]
	}`)
	canon, err := openai.DecodeResponsesRequest(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Encoded tools must include the web_search built-in (appended from ext).
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (function + web_search built-in) after round-trip; got %d, body=%s", len(tools), string(out))
	}
	foundWS := false
	for _, t2 := range tools {
		if t2.Get("type").String() == "web_search" {
			foundWS = true
		}
	}
	if !foundWS {
		t.Errorf("web_search built-in not restored from ext; body=%s", string(out))
	}
}

// TestEncodeResponsesRequest_HoistFirstSystemMessageNoExt covers the
// "no ext.instructions, hoist content from leading system message" path.
func TestEncodeResponsesRequest_HoistFirstSystemMessageNoExt(t *testing.T) {
	canon := []byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"Be polite."},
			{"role":"user","content":"hi"}
		]
	}`)
	out, err := openai.EncodeResponsesRequest(canon)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "instructions").String(); got != "Be polite." {
		t.Errorf("instructions hoisted from system message lost; body=%s", string(out))
	}
	// First input item must be the user message — the system was consumed.
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "user" {
		t.Errorf("input[0].role=%q want user; body=%s", got, string(out))
	}
}

// codec_responses_response.go — response decode/encode rare branches

// TestDecodeResponsesResponse_RefusalContentPart pins the
// content[type=refusal] branch which routes onto choices[0].message.refusal.
func TestDecodeResponsesResponse_RefusalContentPart(t *testing.T) {
	in := []byte(`{
		"id":"resp_r","status":"completed","model":"gpt-5",
		"output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot."}]}]
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.message.refusal").String(); got != "I cannot." {
		t.Errorf("refusal lost: %s", string(out))
	}
}

// TestDecodeResponsesResponse_IncompleteContentFilter pins
// status:incomplete + content_filter → finish_reason:content_filter.
func TestDecodeResponsesResponse_IncompleteContentFilter(t *testing.T) {
	in := []byte(`{
		"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"},
		"model":"gpt-5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "content_filter" {
		t.Errorf("finish_reason=%q want content_filter", got)
	}
}

// TestDecodeResponsesResponse_IncompleteUnknownReason pins
// status:incomplete + unrecognised reason → default-arm finish_reason:length.
func TestDecodeResponsesResponse_IncompleteUnknownReason(t *testing.T) {
	in := []byte(`{
		"id":"r","status":"incomplete","incomplete_details":{"reason":"surprise"},
		"model":"gpt-5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}]
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "length" {
		t.Errorf("finish_reason=%q want length (default for unknown incomplete reason)", got)
	}
}

// TestDecodeResponsesResponse_FailedStatus pins status:failed → stop.
func TestDecodeResponsesResponse_FailedStatus(t *testing.T) {
	in := []byte(`{
		"id":"r","status":"failed","model":"gpt-5",
		"output":[],"error":{"message":"boom"}
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("failed status must map to stop, got %q", got)
	}
}

// TestDecodeResponsesResponse_UnknownStatusDefaultArm pins the default
// arm of mapResponsesStatusToFinishReason.
func TestDecodeResponsesResponse_UnknownStatusDefaultArm(t *testing.T) {
	in := []byte(`{
		"id":"r","status":"queued","model":"gpt-5",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := gjson.GetBytes(out, "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("queued (defensive) must map to stop, got %q", got)
	}
}

// TestDecodeResponsesResponse_NoIDSynthesized pins the "no id present"
// fallback in DecodeResponsesResponse (id := chatcmpl-<unixnano>).
func TestDecodeResponsesResponse_NoIDSynthesized(t *testing.T) {
	in := []byte(`{
		"status":"completed","model":"m",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}]}]
	}`)
	out, _, err := openai.DecodeResponsesResponse(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(gjson.GetBytes(out, "id").String(), "chatcmpl-") {
		t.Errorf("synthesized id must start with chatcmpl-, got %q", gjson.GetBytes(out, "id").String())
	}
	// created must also be synthesized (non-zero unix timestamp).
	if gjson.GetBytes(out, "created").Int() == 0 {
		t.Errorf("created must be synthesized to non-zero unix timestamp")
	}
}

// TestEncodeResponsesResponse_ToolCallsBranch pins the function-call
// output item emission from canonical tool_calls.
func TestEncodeResponsesResponse_ToolCallsBranch(t *testing.T) {
	canon := []byte(`{
		"id":"chatcmpl_1","model":"m",
		"choices":[{
			"index":0,
			"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}
			]},
			"finish_reason":"tool_calls"
		}]
	}`)
	out, err := openai.EncodeResponsesResponse(canon, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	outputs := gjson.GetBytes(out, "output").Array()
	if len(outputs) != 1 {
		t.Fatalf("expected exactly 1 function_call output item, got %d; body=%s", len(outputs), string(out))
	}
	if got := outputs[0].Get("type").String(); got != "function_call" {
		t.Errorf("output[0].type=%q want function_call", got)
	}
	if got := outputs[0].Get("call_id").String(); got != "call_abc" {
		t.Errorf("call_id=%q want call_abc", got)
	}
	if got := outputs[0].Get("name").String(); got != "get_weather" {
		t.Errorf("name=%q want get_weather", got)
	}
	if got := outputs[0].Get("arguments").String(); got != `{"city":"Tokyo"}` {
		t.Errorf("arguments=%q", got)
	}
}

// TestEncodeResponsesResponse_NoRequestIDNoExtIDSynth pins the
// fmt.Sprintf("resp_%d", time.Now().UnixNano()) fallback when neither
// ext.id nor requestID is provided.
func TestEncodeResponsesResponse_NoRequestIDNoExtIDSynth(t *testing.T) {
	canon := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	out, err := openai.EncodeResponsesResponse(canon, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	id := gjson.GetBytes(out, "id").String()
	if !strings.HasPrefix(id, "resp_") {
		t.Errorf("synthesized id must start with resp_, got %q", id)
	}
	// created_at must be synthesized.
	if gjson.GetBytes(out, "created_at").Int() == 0 {
		t.Errorf("created_at must default to time.Now().Unix()")
	}
}

// TestMapFinishReasonToResponsesStatus_AllArms pins every branch of
// the canonical→Responses status mapping.
func TestMapFinishReasonToResponsesStatus_AllArms(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"length", "incomplete"},
		{"max_tokens", "incomplete"},
		{"content_filter", "incomplete"},
		{"stop", "completed"},
		{"tool_calls", "completed"},
		{"", "completed"},
		{"surprise_value", "completed"},
	}
	for _, tc := range cases {
		// Drive via EncodeResponsesResponse which calls the helper.
		canon := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + tc.finish + `"}]}`)
		out, err := openai.EncodeResponsesResponse(canon, "rid", "")
		if err != nil {
			t.Fatalf("encode %q: %v", tc.finish, err)
		}
		if got := gjson.GetBytes(out, "status").String(); got != tc.want {
			t.Errorf("finish=%q → status=%q want %q", tc.finish, got, tc.want)
		}
	}
}

// TestMapFinishReasonToResponsesIncompleteReason_MaxTokens pins the
// max_tokens → max_output_tokens arm (separate from length).
func TestMapFinishReasonToResponsesIncompleteReason_MaxTokens(t *testing.T) {
	canon := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"max_tokens"}]}`)
	out, err := openai.EncodeResponsesResponse(canon, "rid", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := gjson.GetBytes(out, "incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Errorf("max_tokens must map to max_output_tokens, got %q", got)
	}
}

// TestEncodeResponsesResponse_UsageAbsent pins the "no usage block"
// branch of buildResponsesUsage which returns nil → usage omitted.
func TestEncodeResponsesResponse_UsageAbsent(t *testing.T) {
	canon := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`)
	out, err := openai.EncodeResponsesResponse(canon, "rid", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if gjson.GetBytes(out, "usage").Exists() {
		t.Errorf("usage must be absent when canonical has no usage; body=%s", string(out))
	}
}

// TestEncodeResponsesResponse_EmptyCanonicalError pins the explicit
// empty / invalid JSON error contract.
func TestEncodeResponsesResponse_EmptyCanonicalError(t *testing.T) {
	if _, err := openai.EncodeResponsesResponse(nil, "rid", ""); err == nil {
		t.Error("expected error on nil canonical")
	}
	if _, err := openai.EncodeResponsesResponse([]byte("not-json"), "rid", ""); err == nil {
		t.Error("expected error on invalid JSON canonical")
	}
}

// stream.go — chat-completions SSE rare branches

// TestStreamDecoder_NilLogDefaultsToSlog pins NewStreamDecoder(nil)
// (else branch where log is replaced by slog.Default()).
func TestStreamDecoder_NilLogDefaultsToSlog(t *testing.T) {
	dec := openai.NewStreamDecoder(nil)
	if dec == nil {
		t.Fatal("nil decoder returned")
	}
	// Smoke: must Open a real session without panicking.
	sess, err := dec.Open(io.NopCloser(strings.NewReader("data: [DONE]\n\n")), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck
}

// TestStreamDecoder_OpenNilBody pins the nil-body guard.
func TestStreamDecoder_OpenNilBody(t *testing.T) {
	dec := openai.NewStreamDecoder(slog.Default())
	if _, err := dec.Open(nil, typology.WireShapeOpenAIChat); err == nil {
		t.Error("expected error on nil body")
	}
}

// TestStreamSession_NextCtxCancel pins the ctx.Err() short-circuit on
// the chat-completions session.
func TestStreamSession_NextCtxCancel(t *testing.T) {
	dec := openai.NewStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader("data: {}\n\n")), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Next(ctx); err == nil {
		t.Error("expected cancelled context to surface error")
	}
}

// TestStreamSession_EmptyDataFrameSkipped pins the
// "len(ev.Data) == 0 → recurse" branch.
func TestStreamSession_EmptyDataFrameSkipped(t *testing.T) {
	// First event has empty data; second is the real delta; then [DONE].
	raw := strings.Join([]string{
		`event: ping`,
		`data:`,
		``,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	dec := openai.NewStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	var text string
	done := false
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		text += ch.Delta
		if ch.Done {
			done = true
		}
	}
	if text != "hi" {
		t.Errorf("delta=%q want hi (empty data frames should not abort)", text)
	}
	if !done {
		t.Errorf("expected Done")
	}
}

// TestStreamSession_ToolCallDeltas pins the
// "delta.tool_calls" + "delta.reasoning_content" branches.
func TestStreamSession_ToolCallDeltas(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking..."}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_w","arguments":"{\"city\":"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Tokyo\"}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	dec := openai.NewStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(raw)), typology.WireShapeOpenAIChat)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	gotReasoningChannel := false
	toolCallChunks := 0
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		// reasoning_content routes to the dedicated ReasoningDelta channel
		// (kept separate from the answer Delta) per the codec contract.
		if strings.Contains(ch.ReasoningDelta, "thinking...") {
			gotReasoningChannel = true
		}
		if strings.Contains(ch.Delta, "thinking...") {
			t.Errorf("reasoning_content must NOT leak into Delta; got %q", ch.Delta)
		}
		if len(ch.ToolCallDeltas) > 0 {
			toolCallChunks++
			if ch.ToolCallDeltas[0].Index != 0 {
				t.Errorf("ToolCallDelta.Index=%d want 0", ch.ToolCallDeltas[0].Index)
			}
		}
	}
	if !gotReasoningChannel {
		t.Errorf("reasoning_content must populate ReasoningDelta")
	}
	if toolCallChunks != 2 {
		t.Errorf("toolCallChunks=%d want 2", toolCallChunks)
	}
}

// stream_responses.go — responses SSE rare branches

// TestResponsesStream_RefusalDelta pins refusal.delta → Chunk.Delta path.
func TestResponsesStream_RefusalDelta(t *testing.T) {
	transcript := "event: response.created\n" +
		"data: {\"type\":\"response.created\"}\n\n" +
		"event: response.refusal.delta\n" +
		"data: {\"type\":\"response.refusal.delta\",\"delta\":\"I refuse.\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(transcript)), typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	var sawRefusal bool
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ch.Delta == "I refuse." {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Errorf("refusal.delta did not surface as Chunk.Delta")
	}
}

// TestResponsesStream_DoneAfterTerminalReturnsEOF pins that calling
// Next again after a Done chunk returns io.EOF (state machine).
func TestResponsesStream_DoneAfterTerminalReturnsEOF(t *testing.T) {
	transcript := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, err := dec.Open(io.NopCloser(strings.NewReader(transcript)), typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck

	c1, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if !c1.Done {
		t.Errorf("first chunk must be Done")
	}
	if _, err := sess.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("second Next after Done must return EOF, got %v", err)
	}
}

// TestResponsesStream_CtxCancel pins ctx.Err() short-circuit.
func TestResponsesStream_CtxCancel(t *testing.T) {
	transcript := "event: response.output_text.delta\ndata: {\"delta\":\"x\"}\n\n"
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(io.NopCloser(strings.NewReader(transcript)), typology.WireShapeOpenAIResponses)
	defer sess.Close() //nolint:errcheck
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Next(ctx); err == nil {
		t.Error("cancelled ctx must error")
	}
}

// TestResponsesStream_OutputTextDeltaEmptySkipped pins the
// "delta == \"\" → continue" branch of output_text.delta processing.
func TestResponsesStream_OutputTextDeltaEmptySkipped(t *testing.T) {
	transcript := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"real\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(io.NopCloser(strings.NewReader(transcript)), typology.WireShapeOpenAIResponses)
	defer sess.Close() //nolint:errcheck

	var deltas []string
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ch.Delta != "" {
			deltas = append(deltas, ch.Delta)
		}
	}
	if len(deltas) != 1 || deltas[0] != "real" {
		t.Errorf("empty deltas not skipped; got %v", deltas)
	}
}

// TestResponsesStream_ReasoningDeltaEmptySkipped pins the
// "reasoning delta == \"\" → continue" branch.
func TestResponsesStream_ReasoningDeltaEmptySkipped(t *testing.T) {
	transcript := "event: response.reasoning_text.delta\n" +
		"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"\"}\n\n" +
		"event: response.reasoning_text.delta\n" +
		"data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"think\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(io.NopCloser(strings.NewReader(transcript)), typology.WireShapeOpenAIResponses)
	defer sess.Close() //nolint:errcheck

	var reasonings []string
	for {
		ch, err := sess.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ch.ReasoningDelta != "" {
			reasonings = append(reasonings, ch.ReasoningDelta)
		}
	}
	if len(reasonings) != 1 || reasonings[0] != "think" {
		t.Errorf("empty reasoning deltas not skipped; got %v", reasonings)
	}
}

// TestResponsesStream_NilLogDefaultsToSlog pins the
// "log == nil → slog.Default()" branch in NewResponsesStreamDecoder.
func TestResponsesStream_NilLogDefaultsToSlog(t *testing.T) {
	dec := openai.NewResponsesStreamDecoder(nil)
	if dec == nil {
		t.Fatal("nil decoder")
	}
	// Smoke: Open must not panic.
	sess, err := dec.Open(io.NopCloser(strings.NewReader("")), typology.WireShapeOpenAIResponses)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close() //nolint:errcheck
}

// TestResponsesStream_DONESentinel pins the defensive [DONE] terminal
// (some SDKs emit it even on Responses-API).
func TestResponsesStream_DONESentinel(t *testing.T) {
	transcript := "data: [DONE]\n\n"
	dec := openai.NewResponsesStreamDecoder(slog.Default())
	sess, _ := dec.Open(io.NopCloser(strings.NewReader(transcript)), typology.WireShapeOpenAIResponses)
	defer sess.Close() //nolint:errcheck
	c, err := sess.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !c.Done {
		t.Errorf("[DONE] sentinel must produce Done=true")
	}
}

// spec.go — NewSpec rare branch

// TestNewSpec_NilLogDefaultsToSlog pins the log==nil branch.
func TestNewSpec_NilLogDefaultsToSlog(t *testing.T) {
	spec := openai.NewSpec(nil)
	if spec.Format != provcore.FormatOpenAI {
		t.Errorf("Format=%v want FormatOpenAI", spec.Format)
	}
	if spec.Transport == nil || spec.SchemaCodec == nil ||
		spec.StreamDecoder == nil || spec.ErrorNormalizer == nil {
		t.Errorf("NewSpec(nil) returned incomplete spec: %+v", spec)
	}
}

// responses_builtin_tools.go — predicate coverage

// TestIsResponsesBuiltinTool covers each documented entry, the empty
// string, and an unrelated function-tool type (caller-defined tools
// must NOT be reported as built-ins).
func TestIsResponsesBuiltinTool(t *testing.T) {
	for _, name := range []string{
		"web_search", "web_search_preview", "file_search", "computer_use_preview",
		"image_generation", "mcp", "code_interpreter", "custom", "apply_patch",
		"tool_search", "function_shell",
	} {
		if !openai.IsResponsesBuiltinTool(name) {
			t.Errorf("IsResponsesBuiltinTool(%q) = false; want true", name)
		}
	}
	for _, name := range []string{"function", "", "unknown_type_2026"} {
		if openai.IsResponsesBuiltinTool(name) {
			t.Errorf("IsResponsesBuiltinTool(%q) = true; want false", name)
		}
	}
}
