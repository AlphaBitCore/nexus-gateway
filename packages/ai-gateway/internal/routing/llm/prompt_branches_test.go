package llm

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// TestBuildRequestBody_SkipsEmptyTextMessages pins the "drop user
// messages whose text projection is empty" branch. A user message that
// holds only an image_ref (no ContentText blocks) projects to "" via
// textOf — the builder must skip it rather than emit a blank
// {"role":"user","content":""} entry. Otherwise the router LLM sees a
// confusing empty turn and may refuse to pick.
func TestBuildRequestBody_SkipsEmptyTextMessages(t *testing.T) {
	userMsgs := []normalize.Message{
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentImageRef, ImageRef: &normalize.BinaryRef{Size: 1, ContentType: "image/png", SHA256: "abc"}},
		}}, // text-empty — must be skipped
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: "real text"},
		}},
	}

	body := BuildRequestBody("rm", Request{SystemPrompt: "pick", UserMessages: userMsgs})

	// system + exactly one user message ("real text"); the image-only
	// message must NOT appear as a blank-content turn.
	if len(body.Messages) != 2 {
		t.Fatalf("expected 1 system + 1 user (image-only skipped), got %d: %+v", len(body.Messages), body.Messages)
	}
	if body.Messages[1].Content != "real text" {
		t.Errorf("Messages[1].Content = %q, want %q", body.Messages[1].Content, "real text")
	}
}

// TestBuildRequestBody_EmptySystemPromptFallsBackToDefault pins the
// "empty SystemPrompt -> DefaultSystemPrompt" fallback. The smart
// strategy normally always supplies a non-empty prompt (catalog
// substituted in), but if a caller hands an empty string the builder
// must NOT emit "" as the system message — it must substitute the
// built-in DefaultSystemPrompt so the router LLM still has selection
// rules to follow.
func TestBuildRequestBody_EmptySystemPromptFallsBackToDefault(t *testing.T) {
	body := BuildRequestBody("rm", Request{
		SystemPrompt: "",
		UserMessages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "x"},
			}},
		},
	})

	if len(body.Messages) < 1 || body.Messages[0].Role != "system" {
		t.Fatalf("expected system message at index 0, got %+v", body.Messages)
	}
	if body.Messages[0].Content != DefaultSystemPrompt {
		t.Errorf("Messages[0].Content did not fall back to DefaultSystemPrompt; got %q", body.Messages[0].Content)
	}
	if !strings.Contains(body.Messages[0].Content, "Selection Rules") {
		t.Errorf("DefaultSystemPrompt fallback should contain its built-in Selection Rules section")
	}
}

// TestParseResponse_EnvelopeInvalidJSON pins the first observable failure
// mode in ParseResponse: the chat-completions envelope must itself be
// valid JSON. The audit routing_trace surfaces the wrapped json.Unmarshal
// error verbatim so an operator can spot a truncated/garbled upstream
// body without reaching for logs.
func TestParseResponse_EnvelopeInvalidJSON(t *testing.T) {
	_, err := ParseResponse(`{not-json`)
	if err == nil {
		t.Fatal("expected envelope-parse error for malformed JSON")
	}
	if !strings.HasPrefix(err.Error(), "failed to parse response envelope:") {
		t.Errorf("err = %q; want prefix %q", err.Error(), "failed to parse response envelope:")
	}
}

// TestParseResponse_NoChoices pins the no-choices branch. A well-formed
// chat-completions envelope with an empty choices array (some providers
// return this on content-filter triggers) yields the literal trace
// string "no choices in response" so the smart strategy can fall back
// instead of NPE-ing on choices[0].
func TestParseResponse_NoChoices(t *testing.T) {
	_, err := ParseResponse(`{"choices":[]}`)
	if err == nil || err.Error() != "no choices in response" {
		t.Errorf("err = %v; want %q", err, "no choices in response")
	}
}

// TestParseResponse_EmptyContent covers the case where choices[0] exists
// but message.content is whitespace-only. TrimSpace produces "" and the
// router has nothing to extract, so the operator-facing trace must be
// "empty content in response" (not a confusing parse-error message).
func TestParseResponse_EmptyContent(t *testing.T) {
	envelope := `{"choices":[{"message":{"content":"   \n  "}}]}`
	_, err := ParseResponse(envelope)
	if err == nil || err.Error() != "empty content in response" {
		t.Errorf("err = %v; want %q", err, "empty content in response")
	}
}

// TestParseResponse_CodeBlockExtraction_WithJSONLanguage covers the
// markdown-fenced JSON extraction path: the router LLM wraps its picked
// model in ```json ... ``` (a common formatting habit). The direct
// json.Unmarshal of the wrapped string fails, and the codeBlockRe path
// must successfully extract the inner JSON and produce a Decision.
func TestParseResponse_CodeBlockExtraction_WithJSONLanguage(t *testing.T) {
	envelope := `{"choices":[{"message":{"content":"Here is my pick:\n` +
		"```" + `json\n{\"modelId\":\"m-pick\",\"reason\":\"matches capability tags\"}\n` +
		"```" + `\n"}}]}`

	d, err := ParseResponse(envelope)
	if err != nil {
		t.Fatalf("expected code-block extraction success; err = %v", err)
	}
	if d.ModelID != "m-pick" || d.Reason != "matches capability tags" {
		t.Errorf("got %#v, want ModelID=m-pick reason=matches capability tags", d)
	}
}

// TestParseResponse_CodeBlockExtraction_NoLanguage covers the variant
// where the LLM wraps with bare ``` (no language tag). codeBlockRe must
// still match — the language group is optional — and the inner JSON
// must decode to a Decision.
func TestParseResponse_CodeBlockExtraction_NoLanguage(t *testing.T) {
	envelope := `{"choices":[{"message":{"content":"` +
		"```" + `\n{\"modelId\":\"m-anon\",\"reason\":\"good\"}\n` +
		"```" + `"}}]}`

	d, err := ParseResponse(envelope)
	if err != nil {
		t.Fatalf("expected bare-fence extraction success; err = %v", err)
	}
	if d.ModelID != "m-anon" || d.Reason != "good" {
		t.Errorf("got %#v, want ModelID=m-anon reason=good", d)
	}
}

// TestParseResponse_RegexFallback pins the last-resort regex path: the
// router LLM emits prose that is NOT valid JSON and NOT inside a code
// block, but contains the modelId/reason pattern as a JSON fragment.
// modelIDRe extracts the two fields and returns a Decision rather than
// failing — this rescues otherwise-good picks from chatty models.
func TestParseResponse_RegexFallback(t *testing.T) {
	// Content is plain prose holding a JSON snippet inside. The direct
	// json.Unmarshal will fail; codeBlockRe will not match (no fences);
	// modelIDRe must catch the pattern.
	envelope := `{"choices":[{"message":{"content":"After thinking, my answer is {\"modelId\":\"m-prose\",\"reason\":\"cheap and capable\"} — go."}}]}`

	d, err := ParseResponse(envelope)
	if err != nil {
		t.Fatalf("expected regex fallback to succeed; err = %v", err)
	}
	if d.ModelID != "m-prose" || d.Reason != "cheap and capable" {
		t.Errorf("got %#v, want ModelID=m-prose reason=cheap and capable", d)
	}
	// Regex fallback intentionally does NOT populate ProviderID — the
	// regex captures only modelId and reason. Pin that contract.
	if d.ProviderID != "" {
		t.Errorf("regex fallback should leave ProviderID empty; got %q", d.ProviderID)
	}
}

// TestParseResponse_AllExtractionsFail pins the terminal failure: a
// non-empty content string that matches none of (direct JSON / code
// block / regex). The audit routing_trace gets the literal string
// "could not extract modelId from router response" so smart-strategy
// fallback can branch on it.
func TestParseResponse_AllExtractionsFail(t *testing.T) {
	envelope := `{"choices":[{"message":{"content":"I cannot decide — please retry."}}]}`
	_, err := ParseResponse(envelope)
	if err == nil || err.Error() != "could not extract modelId from router response" {
		t.Errorf("err = %v; want %q", err, "could not extract modelId from router response")
	}
}

// TestParseResponse_CodeBlock_InnerJSONInvalid_FallsThroughToRegex
// exercises the branch where codeBlockRe matches the fenced segment but
// the inner string is malformed JSON. The fenced parse attempt returns
// !ok; control must fall through to the regex path, which then also
// fails to match, producing the terminal extraction error. This pins
// the "don't crash on a half-formed code block" contract.
func TestParseResponse_CodeBlock_InnerJSONInvalid_FallsThroughToRegex(t *testing.T) {
	envelope := `{"choices":[{"message":{"content":"` +
		"```" + `json\n{this is not json}\n` +
		"```" + `"}}]}`
	_, err := ParseResponse(envelope)
	if err == nil || err.Error() != "could not extract modelId from router response" {
		t.Errorf("err = %v; want %q", err, "could not extract modelId from router response")
	}
}

// TestTryParseRouterJSON_InvalidJSON pins the first observable rejection
// path: the helper returns (zero, false) on malformed JSON so the caller
// can fall through to the code-block / regex paths instead of mistaking
// a decode error for a missing-modelId error.
func TestTryParseRouterJSON_InvalidJSON(t *testing.T) {
	d, ok := tryParseRouterJSON("{not-valid")
	if ok {
		t.Fatal("expected ok=false on malformed JSON")
	}
	if d != (Decision{}) {
		t.Errorf("expected zero Decision on rejection; got %#v", d)
	}
}

// TestTryParseRouterJSON_MissingModelID pins the modelId-required rule.
// A syntactically valid JSON object without a modelId field is rejected
// (ok=false). Without this, the smart strategy would accept a router
// reply that picks "no model" and crash downstream when looking the
// empty code up in the catalog.
func TestTryParseRouterJSON_MissingModelID(t *testing.T) {
	d, ok := tryParseRouterJSON(`{"reason":"I have an opinion but no pick"}`)
	if ok {
		t.Fatal("expected ok=false when modelId is missing")
	}
	if d != (Decision{}) {
		t.Errorf("expected zero Decision; got %#v", d)
	}
}

// TestTryParseRouterJSON_EmptyReason_FillsPlaceholder pins the "no
// reason provided" default. The audit routing_trace stamps Decision.Reason
// verbatim; an empty Reason would render as blank in the admin UI, so
// the helper substitutes a visible placeholder. Pinning this protects
// the operator-facing trace text.
func TestTryParseRouterJSON_EmptyReason_FillsPlaceholder(t *testing.T) {
	d, ok := tryParseRouterJSON(`{"modelId":"m-x","reason":""}`)
	if !ok {
		t.Fatal("expected ok=true with modelId present")
	}
	if d.Reason != "no reason provided" {
		t.Errorf("Reason = %q; want %q", d.Reason, "no reason provided")
	}
	if d.ModelID != "m-x" {
		t.Errorf("ModelID = %q; want m-x", d.ModelID)
	}
}

// TestTryParseRouterJSON_AllFieldsPresent pins the happy-path projection
// from routerJSONResponse to Decision: all three fields (modelId,
// providerId, reason) survive the struct conversion intact. Belt-and-
// suspenders next to TestParseResponse_ProviderID — that one drives
// through ParseResponse; this one isolates the helper.
func TestTryParseRouterJSON_AllFieldsPresent(t *testing.T) {
	d, ok := tryParseRouterJSON(`{"modelId":"m-a","providerId":"p-b","reason":"r-c"}`)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if d.ModelID != "m-a" || d.ProviderID != "p-b" || d.Reason != "r-c" {
		t.Errorf("Decision projection wrong: %#v", d)
	}
}

// TestBuildRequestBodyWithLogger_InputOverflow_LogsWarnAndFallsThrough
// exercises the OverflowSingleMessageTooBig path in buildRequestBodyWithLogger.
// A single user message whose token count exceeds routerContextLimit-routerReserveOutput
// triggers a logged warn but the request body is still built (fail-open):
// the smart strategy's upstream fallback handles the bad pick gracefully.
func TestBuildRequestBodyWithLogger_InputOverflow_LogsWarnAndFallsThrough(t *testing.T) {
	// Generate a message large enough to exceed the budget.
	// routerContextLimit=8192, routerReserveOutput=256 → budget=7936 tokens.
	// EstimateTokens uses 0.25 tok/ASCII char → ~31 744 chars needed.
	bigText := strings.Repeat("x", 32000) // ≈ 8 000 tokens

	userMsgs := []normalize.Message{
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: bigText},
		}},
	}

	// Use a discard logger — we are pinning the "no panic" and "returns
	// a valid body" contract; log output is not observable in unit tests.
	discardLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := buildRequestBodyWithLogger("rm", Request{
		SystemPrompt: "pick",
		UserMessages: userMsgs,
	}, discardLogger)

	// The function must not panic and must return at least the system
	// message. The oversized user message is still forwarded (fail-open
	// semantics — the provider will reject it if truly over-limit, and
	// the smart strategy fallback recovers).
	if len(body.Messages) < 1 {
		t.Fatal("expected at least the system message in the body")
	}
	if body.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want system", body.Messages[0].Role)
	}
}

// TestBuildRequestBodyWithLogger_InputOverflow_DiscardLogger ensures the
// discard-logger variant used in overflow tests does not panic on io.Discard.
func TestBuildRequestBodyWithLogger_SingleUserMessage_KeptDirectly(t *testing.T) {
	// A single user message within budget: should pass through unchanged.
	userMsgs := []normalize.Message{
		{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
			{Type: normalize.ContentText, Text: "what is the capital of France?"},
		}},
	}

	body := buildRequestBodyWithLogger("rm", Request{
		SystemPrompt: "pick",
		UserMessages: userMsgs,
	}, slog.Default())

	if len(body.Messages) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(body.Messages))
	}
	if body.Messages[1].Content != "what is the capital of France?" {
		t.Errorf("user message content = %q; want original", body.Messages[1].Content)
	}
}
