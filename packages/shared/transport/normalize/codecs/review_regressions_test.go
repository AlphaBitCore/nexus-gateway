package codecs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// One test per defect found by the Gate-1 adversarial review rounds.
//
// Every fix in those rounds shipped with an execution count of zero, which is
// why each round rediscovered the previous round's mistakes by hand. These
// pin them: a regression fails here instead of surviving to another review.

// Round 3, HIGH. The depth cap used to json.Marshal the remaining subtree
// into a text block — 120 KB of base64 into the stream that feeds compliance
// scanning, with the media element gone. The headline defect, behind a gate.
func TestAnthropicDeepNesting_NeitherLeaksPayloadNorDropsSilently(t *testing.T) {
	// A base64 document buried deeper than the cap.
	inner := map[string]any{
		"type":   "document",
		"source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": strings.Repeat("QUJD", 4000)},
	}
	node := any(inner)
	for range 8 {
		node = map[string]any{
			"type":   "document",
			"source": map[string]any{"type": "content", "content": []any{node}},
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": []any{node}}},
	})

	p, err := NewAnthropicMessagesNormalizer().Normalize(
		context.Background(), body, core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	var sawMarker bool
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type != core.ContentText {
				continue
			}
			if strings.Contains(b.Text, "QUJDQUJD") {
				t.Fatalf("payload leaked into a scanned text block (%d chars)", len(b.Text))
			}
			if strings.Contains(b.Text, "not projected") {
				sawMarker = true
				// The marker must name what it saw, or a reader cannot tell
				// anything was there.
				if !strings.Contains(b.Text, "document") {
					t.Fatalf("marker does not name the content it elided: %q", b.Text)
				}
			}
		}
	}
	if !sawMarker {
		t.Fatal("content beyond the cap must leave a marker, not vanish")
	}
}

// Round 3, LOW. A non-array `source.content` used to be marshalled, yielding
// text blocks reading literally `null` or a quoted string — fabricated prose
// entering the scanned text.
func TestAnthropicNestedContent_NonArrayIsDescribedNotSerialised(t *testing.T) {
	for _, raw := range []string{
		`{"type":"document","source":{"type":"content"}}`,
		`{"type":"document","source":{"type":"content","content":"plain string"}}`,
	} {
		var part map[string]any
		if err := json.Unmarshal([]byte(raw), &part); err != nil {
			t.Fatal(err)
		}
		blocks := anthropicContentPart(part, "messages.0.content.0")
		if len(blocks) != 1 || blocks[0].Type != core.ContentText {
			t.Fatalf("want one text block, got %+v", blocks)
		}
		got := blocks[0].Text
		if got == "null" || strings.HasPrefix(got, `"`) {
			t.Fatalf("serialised value leaked as prose: %q", got)
		}
		if !strings.Contains(got, "nested content") {
			t.Fatalf("want a description, got %q", got)
		}
	}
}

// Round 2, HIGH. The no-silent-drop marker fired for `text` — the most
// recognised type of all — fabricating assistant prose that then reached
// compliance scanning.
func TestAnthropicStreamMarker_OnlyForGenuinelyUnknownBlocks(t *testing.T) {
	stream := func(blockType string) string {
		return "event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"` + blockType + `"}}` + "\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	for _, tc := range []struct {
		blockType  string
		wantMarker bool
	}{
		{"text", false},
		{"thinking", false},
		{"future_widget", true},
	} {
		p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
			[]byte(stream(tc.blockType)), core.Meta{Direction: core.DirectionResponse, Stream: true})
		if err != nil {
			t.Fatalf("%s: %v", tc.blockType, err)
		}
		var marked bool
		for _, m := range p.Messages {
			for _, b := range m.Content {
				if strings.Contains(b.Text, "unrecognised anthropic block") {
					marked = true
				}
			}
		}
		if marked != tc.wantMarker {
			t.Errorf("block %q: marker=%v, want %v", tc.blockType, marked, tc.wantMarker)
		}
	}
}

// Round 2. Files-API URIs are https, so a scheme check inverted custody. The
// replacement must not be spoofable by a host that merely embeds the string.
func TestGeminiFileDataCustody_HostNotSubstring(t *testing.T) {
	for uri, want := range map[string]string{
		"https://generativelanguage.googleapis.com/v1beta/files/rf9ysxbpu442": core.MediaProviderRef,
		"gs://bucket/obj.mp4": core.MediaProviderRef,
		"files/abc123":        core.MediaProviderRef,
		"https://evil.example.com/generativelanguage.googleapis.com/files/x": core.MediaExternal,
		"https://generativelanguage.googleapis.com.evil.com/files/x":         core.MediaExternal,
		"https://www.youtube.com/watch?v=abc":                                core.MediaExternal,
	} {
		if got := geminiFileDataMedia("image/png", uri).Source; got != want {
			t.Errorf("%s → %q, want %q", uri, got, want)
		}
	}
}

// Round 2. A data URI reaching MediaRef.URL is the headline defect. The guard
// lives in externalMedia because six callers pass client-controlled strings.
func TestExternalMedia_DataURINeverReachesURL(t *testing.T) {
	got := externalMedia("", "data:audio/wav;base64,QUJD", "")
	if got.URL != "" {
		t.Fatalf("payload in URL: %+v", got)
	}
	if got.Mime != "audio/wav" {
		t.Fatalf("declared mime lost: %+v", got)
	}
	// Modality must be derived from the recovered mime, not from the empty
	// one it was called with — otherwise the ref contradicts itself.
	if got.Modality != core.ModalityAudio {
		t.Fatalf("modality disagrees with mime: %+v", got)
	}
	if ok := externalMedia("", "https://cdn.example.com/a.png", core.ModalityImage); ok.URL == "" || ok.Source != core.MediaExternal {
		t.Fatalf("a legitimate remote URL must still be external: %+v", ok)
	}
}

// Round 2. The block type is the authority on modality: an image block is an
// image whatever media_type claims, or capability routing loses needsVision.
func TestAnthropicModality_BlockTypeIsAuthoritative(t *testing.T) {
	img := `{"type":"image","source":{"type":"base64","media_type":"application/octet-stream","data":"QUJD"}}`
	doc := `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}}`
	for raw, want := range map[string]string{img: core.ModalityImage, doc: core.ModalityFile} {
		var part map[string]any
		_ = json.Unmarshal([]byte(raw), &part)
		blocks := anthropicContentPart(part, "messages.0.content.0")
		if len(blocks) != 1 || blocks[0].MediaRef == nil {
			t.Fatalf("want one media block: %+v", blocks)
		}
		if got := blocks[0].MediaRef.Modality; got != want {
			t.Errorf("%s → modality %q, want %q", raw[:40], got, want)
		}
	}
}

// Round 3. A streamed responses image cannot carry a json: locator: the
// stored body is the event stream, so the locator would address a fold that
// exists only in memory, and the download would 404. Round 3 also settled
// that a record with no digest is not a "fingerprint".
func TestResponsesImageOut_StreamedIsHonestNonStreamedIsAddressable(t *testing.T) {
	obj := `{"object":"response","model":"gpt-5","status":"completed",` +
		`"output":[{"type":"image_generation_call","result":"QUJD"}],` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`

	nonStream, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
		[]byte(obj), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("non-stream: %v", err)
	}
	ref := firstMedia(t, nonStream)
	if ref.Source != core.MediaCaptured || ref.Locator != "json:output.0.result" {
		t.Fatalf("non-streamed image must be addressable: %+v", ref)
	}

	// The real wire shape: each data line is an EVENT wrapping the response.
	sse := "event: response.completed\ndata: " +
		`{"type":"response.completed","response":` + obj + `}` + "\n\n"
	streamed, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	sref := firstMedia(t, streamed)
	if sref.Locator != "" {
		t.Fatalf("a streamed body has no addressable location: %+v", sref)
	}
	if sref.Source == core.MediaFingerprint && sref.SHA256 == "" {
		t.Fatalf("a fingerprint with no digest is not a fingerprint: %+v", sref)
	}
	if sref.Cause == "" {
		t.Fatalf("an unreachable ref must say why: %+v", sref)
	}
}

// Round 3. computer_screenshot is in the endpoint's own supported set, and
// dropping it took the whole input item with it.
func TestResponsesComputerScreenshot_SurvivesInAllThreeShapes(t *testing.T) {
	for _, tc := range []struct{ part, wantSource string }{
		{`{"type":"computer_screenshot","image_url":"data:image/png;base64,QUJD"}`, core.MediaCaptured},
		{`{"type":"computer_screenshot","file_id":"img-1"}`, core.MediaProviderRef},
		{`{"type":"computer_screenshot"}`, core.MediaAbsent},
	} {
		body := `{"model":"gpt-5","input":[{"role":"user","content":[` + tc.part + `]}]}`
		p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
			[]byte(body), core.Meta{Direction: core.DirectionRequest})
		if err != nil {
			t.Fatalf("%s: %v", tc.part, err)
		}
		if len(p.Messages) == 0 {
			t.Fatalf("the input item vanished with the part: %s", tc.part)
		}
		ref := firstMedia(t, p)
		if ref.Source != tc.wantSource || ref.Modality != core.ModalityImage {
			t.Errorf("%s → %+v, want source %q", tc.part, ref, tc.wantSource)
		}
	}
}

// Round 3. Streamed chat audio spans frames, so no locator addresses it. It
// is not a fingerprint either — nothing is hashed.
func TestOpenAIChatStream_AudioIsAbsentWithACause(t *testing.T) {
	sse := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"audio":{"data":"QUJD","transcript":"hi"}}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Locator != "" || ref.Source != core.MediaAbsent || ref.Cause == "" {
		t.Fatalf("streamed audio must be absent-with-a-cause and unaddressable: %+v", ref)
	}
	// The transcript is text and must stay scannable.
	var sawTranscript bool
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && strings.Contains(b.Text, "hi") {
				sawTranscript = true
			}
		}
	}
	if !sawTranscript {
		t.Fatal("the spoken transcript must survive as text")
	}
}

// Round 2. The gemini marker belongs in its own block, not spliced into the
// concatenated text builder where it would corrupt the assistant's words.
func TestGeminiStreamMarker_IsItsOwnBlock(t *testing.T) {
	sse := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[` +
		`{"text":"hello"},{"executableCode":{"language":"PYTHON","code":"print(1)"}}]}}]}` + "\n\n"
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var textBlock, markerBlock string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type != core.ContentText {
				continue
			}
			if strings.Contains(b.Text, "unrecognised") {
				markerBlock = b.Text
			} else {
				textBlock = b.Text
			}
		}
	}
	if markerBlock == "" {
		t.Fatal("an unrecognised part must leave a marker")
	}
	if strings.Contains(textBlock, "unrecognised") {
		t.Fatalf("the marker contaminated the assistant text: %q", textBlock)
	}
	if textBlock != "hello" {
		t.Fatalf("assistant text should be exactly the model's words, got %q", textBlock)
	}
}

// Round 2. container_upload is a provider-held file reference, not prose.
func TestAnthropicContainerUpload_IsProviderHeld(t *testing.T) {
	var part map[string]any
	_ = json.Unmarshal([]byte(`{"type":"container_upload","file_id":"file-xyz"}`), &part)
	blocks := anthropicContentPart(part, "messages.0.content.0")
	if len(blocks) != 1 || blocks[0].MediaRef == nil {
		t.Fatalf("want one media block: %+v", blocks)
	}
	ref := blocks[0].MediaRef
	if ref.Source != core.MediaProviderRef || ref.ProviderRef != "file-xyz" {
		t.Fatalf("container_upload: %+v", ref)
	}
}

// Round 3, open item. Server-side tool blocks carry their payload over
// input_json_delta. Those frames were counted as recognised — raising the
// confidence score — and then dropped at stitch time because the block type
// did not match: content loss wearing the appearance of coverage.
func TestAnthropicStream_ServerToolPayloadIsNotDropped(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"internal project\"}"}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var found bool
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, "internal project") {
				found = true
				if !strings.Contains(b.Text, "server_tool_use") {
					t.Fatalf("payload kept but its origin unnamed: %q", b.Text)
				}
			}
			if strings.Contains(b.Text, "unrecognised") {
				t.Fatalf("a block whose payload we kept must not also be called unrecognised: %q", b.Text)
			}
		}
	}
	if !found {
		t.Fatal("the tool payload was counted as recognised and then discarded — content loss")
	}
}

// Round 4, HIGH x2. The payload-into-scanned-text defect was reachable
// through every default branch, not just the one each round happened to be
// shown. This drives all of them at once: whatever the door, no payload may
// come through it.
func TestNoPayloadReachesTextThroughAnyDefaultBranch(t *testing.T) {
	big := strings.Repeat("QUJD", 4000) // 16 KB of base64
	cases := []struct {
		name string
		norm func() (core.NormalizedPayload, error)
	}{
		{"anthropic unknown block with a base64 source", func() (core.NormalizedPayload, error) {
			body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
				`{"type":"future_block","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`
			return NewAnthropicMessagesNormalizer().Normalize(context.Background(),
				[]byte(body), core.Meta{Direction: core.DirectionRequest})
		}},
		{"anthropic mixed-type content array", func() (core.NormalizedPayload, error) {
			body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
				`"a bare string",{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`
			return NewAnthropicMessagesNormalizer().Normalize(context.Background(),
				[]byte(body), core.Meta{Direction: core.DirectionRequest})
		}},
		{"openai chat unknown part", func() (core.NormalizedPayload, error) {
			body := `{"model":"m","messages":[{"role":"user","content":[` +
				`{"type":"future_part","blob":"` + big + `"}]}]}`
			return NewOpenAIChatNormalizer().Normalize(context.Background(),
				[]byte(body), core.Meta{Direction: core.DirectionRequest})
		}},
		{"cohere unknown part", func() (core.NormalizedPayload, error) {
			body := `{"model":"m","messages":[{"role":"user","content":[` +
				`{"type":"future_part","blob":"` + big + `"}]}]}`
			return NewCohereChatNormalizer().Normalize(context.Background(),
				[]byte(body), core.Meta{Direction: core.DirectionRequest})
		}},
		{"cohere unparseable content", func() (core.NormalizedPayload, error) {
			body := `{"model":"m","messages":[{"role":"user","content":{"blob":"` + big + `"}}]}`
			return NewCohereChatNormalizer().Normalize(context.Background(),
				[]byte(body), core.Meta{Direction: core.DirectionRequest})
		}},
		{"gemini unrecognised part", func() (core.NormalizedPayload, error) {
			body := `{"contents":[{"role":"user","parts":[{"futureThing":{"blob":"` + big + `"}}]}]}`
			return NewGeminiGenerateNormalizer().Normalize(context.Background(),
				[]byte(body), core.Meta{Direction: core.DirectionRequest})
		}},
	}
	for _, tc := range cases {
		p, err := tc.norm()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, m := range p.Messages {
			for _, b := range m.Content {
				if b.Type != core.ContentText {
					continue
				}
				if strings.Contains(b.Text, "QUJDQUJDQUJD") {
					t.Errorf("%s: payload reached a scanned text block (%d chars)", tc.name, len(b.Text))
				}
				if len(b.Text) > 2000 {
					t.Errorf("%s: text block is %d chars — a payload is hiding in it", tc.name, len(b.Text))
				}
			}
		}
	}
}

// Round 4. The url-safe case in the alphabet test was length 6, so the %4
// guard rejected it before the alphabet check ran — the test passed for the
// wrong reason. These lengths are ≡ 0 mod 4, so only the alphabet can reject.
func TestValidBase64_UrlSafeAlphabetRejectedOnItsOwnMerits(t *testing.T) {
	for _, s := range []string{"ab-c", "ab_c", "ab-c_dXY"} {
		if len(s)%4 != 0 {
			t.Fatalf("fixture %q must be length ≡0 mod 4 or it proves nothing", s)
		}
		if locator.ValidBase64(s) {
			t.Errorf("locator.ValidBase64(%q) = true; StdEncoding rejects the url-safe alphabet", s)
		}
	}
}

// Round 4. The streamed-image test pinned the cause and the missing path
// but never the custody state the fix is named for.
func TestResponsesStreamedImage_IsExplicitlyAbsent(t *testing.T) {
	obj := `{"object":"response","model":"gpt-5","status":"completed",` +
		`"output":[{"type":"image_generation_call","result":"QUJD"}],` +
		`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	sse := "event: response.completed\ndata: " +
		`{"type":"response.completed","response":` + obj + `}` + "\n\n"
	p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Source != core.MediaAbsent {
		t.Fatalf("streamed image must be explicitly absent, got source=%q", ref.Source)
	}
}

// Round 4. Two fixes were wholly unpinned: nested images inside a
// tool_result, and a responses input_image carrying a data URI — the latter
// being the payload-in-URL defect on an ingress whose sibling branch was
// already pinned.
func TestPreviouslyUnpinnedMediaPaths(t *testing.T) {
	t.Run("anthropic tool_result nested image", func(t *testing.T) {
		body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
			`{"type":"tool_result","tool_use_id":"t1","content":[` +
			`{"type":"text","text":"here"},` +
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}]}`
		p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
			[]byte(body), core.Meta{Direction: core.DirectionRequest})
		if err != nil {
			t.Fatal(err)
		}
		ref := firstMedia(t, p)
		if ref.Source != core.MediaCaptured || !strings.Contains(ref.Locator, ".content.") {
			t.Fatalf("a screenshot returned by a tool must stay addressable: %+v", ref)
		}
	})
	t.Run("responses input_image data URI", func(t *testing.T) {
		body := `{"model":"gpt-5","input":[{"role":"user","content":[` +
			`{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]}]}`
		p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(),
			[]byte(body), core.Meta{Direction: core.DirectionRequest})
		if err != nil {
			t.Fatal(err)
		}
		ref := firstMedia(t, p)
		if ref.URL != "" {
			t.Fatalf("payload in URL — the headline defect on this ingress: %+v", ref)
		}
		if ref.Source != core.MediaCaptured || ref.Mime != "image/png" {
			t.Fatalf("inline image must be captured with its declared format: %+v", ref)
		}
	})
}

// Round 5, HIGH. A per-value length cap does not bound the output: a payload
// split across many short strings, hidden in JSON keys, or written as an
// array of integers walks straight through one. Measured at 32 KB — worse
// than the defect the cap was meant to close. The bound that holds is a
// budget on the rendered output, whatever shape the input takes.
func TestPayloadSafeRendering_BoundsTotalOutputNotPerValue(t *testing.T) {
	chunk := strings.Repeat("Q", 500)
	many := make([]any, 64)
	for i := range many {
		many[i] = chunk
	}
	ints := make([]any, 16000)
	for i := range ints {
		ints[i] = 123456789
	}
	keyed := map[string]any{}
	for i := range 64 {
		keyed[chunk+strconv.Itoa(i)] = "x"
	}

	for name, v := range map[string]any{
		"payload split across many short strings": many,
		"payload as an array of integers":         ints,
		"payload hidden in the keys":              keyed,
		"payload as one long string":              strings.Repeat("Q", 40000),
		"deeply nested":                           deepNest(40),
	} {
		got := payloadSafeJSON(v)
		// A literal, not payloadSafeBudget: comparing a bound against the
		// constant that defines it means widening the constant makes the
		// test pass. The number below is the contract.
		if len(got) > 1100 {
			t.Errorf("%s: rendered %d chars; a synthetic block must stay under 1100", name, len(got))
		}
	}
}

func deepNest(n int) any {
	var v any = "leaf"
	for range n {
		v = map[string]any{"k": v}
	}
	return v
}

// Round 5. The fix for server_tool_use kept its payload verbatim, creating a
// sixth leaking site — safe on the non-stream path, unsafe on the stream.
func TestAnthropicStream_ServerToolPayloadIsBounded(t *testing.T) {
	// The block TYPE is wire-controlled too, and payloadSafeText originally
	// bounded only the payload it was concatenated with — the guard against
	// unbounded text grew an unbounded path on its own line.
	t.Run("wire-controlled block type is bounded", func(t *testing.T) {
		huge := strings.Repeat("T", 65536)
		sse := "event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"` + huge + `"}}` + "\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
			[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range p.Messages {
			for _, b := range m.Content {
				if len(b.Text) > 1100 {
					t.Fatalf("wire-controlled block type reached a text block unbounded: %d chars", len(b.Text))
				}
			}
		}
	})

	big := strings.Repeat("Q", 18000)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s1"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + big + `"}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if len(b.Text) > 1100 {
				t.Fatalf("server tool payload reached a text block: %d chars", len(b.Text))
			}
		}
	}
}

// Round 5. Mutating the path's trailing field survived on four of five
// ingresses: only Anthropic's path was pinned. A locator that resolves to
// the wrong field means every inline-media download from those ingresses is
// dead while the card still offers the button — the disagreement between
// "offered" and "servable" this design exists to make impossible.
//
// Each expectation below is the exact gjson path into that provider's own
// wire body, taken from PROVIDER-MEDIA-SHAPES.md, not from the code.
func TestLocatorAddressesTheExactWireField(t *testing.T) {
	png := "iVBORw0KGgo="
	for _, tc := range []struct {
		name, body, wantLocator string
		norm                    func([]byte) (core.NormalizedPayload, error)
	}{
		{
			"openai chat image_url", // {image_url:{url}} — nested
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}]}`,
			"datauri:messages.0.content.0.image_url.url",
			func(b []byte) (core.NormalizedPayload, error) {
				return NewOpenAIChatNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
		{
			"cohere image_url", // same nesting as openai
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}]}`,
			"datauri:messages.0.content.0.image_url.url",
			func(b []byte) (core.NormalizedPayload, error) {
				return NewCohereChatNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
		{
			"responses input_image", // image_url is a BARE STRING here, not an object
			`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + png + `"}]}]}`,
			"datauri:input.0.content.0.image_url",
			func(b []byte) (core.NormalizedPayload, error) {
				return NewOpenAIResponsesNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
		{
			"gemini inlineData",
			`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + png + `"}}]}]}`,
			"json:contents.0.parts.0.inlineData.data",
			func(b []byte) (core.NormalizedPayload, error) {
				return NewGeminiGenerateNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
		{
			"anthropic image source",
			`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}]}]}`,
			"json:messages.0.content.0.source.data",
			func(b []byte) (core.NormalizedPayload, error) {
				return NewAnthropicMessagesNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
		{
			"openai images response artifact",
			`{"data":[{"b64_json":"` + png + `"}]}`,
			"json:data.0.b64_json",
			func(b []byte) (core.NormalizedPayload, error) {
				return NewOpenAIImagesNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionResponse})
			},
		},
	} {
		p, err := tc.norm([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		ref := firstMedia(t, p)
		if ref.Locator != tc.wantLocator {
			t.Errorf("%s: path %q, want %q", tc.name, ref.Locator, tc.wantLocator)
		}
	}
}

// Round 5. The gemini stream fold grouped blocks by kind, so text arriving
// on either side of a tool call fused into one utterance: "A" + "B" became
// "AB". That is not an ordering nicety — it changes what the record says the
// model said. A sibling fold in the same package keeps arrival order, so the
// grouping was a choice, not a constraint.
func TestGeminiStream_PreservesWireOrderAndDoesNotFuseSpeech(t *testing.T) {
	sse := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[` +
		`{"text":"A"},` +
		`{"functionCall":{"name":"lookup","args":{"q":"x"}}},` +
		`{"text":"B"}]}}]}` + "\n\n"
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case core.ContentText:
				shape = append(shape, "text:"+b.Text)
			case core.ContentToolUse:
				shape = append(shape, "tool")
			}
		}
	}
	want := []string{"text:A", "tool", "text:B"}
	if len(shape) != len(want) {
		t.Fatalf("got %v, want %v", shape, want)
	}
	for i := range want {
		if shape[i] != want[i] {
			t.Fatalf("got %v, want %v — the model's words were fused or reordered", shape, want)
		}
	}
}

// Consecutive deltas must still coalesce, or a streamed sentence arrives as
// one block per token.
func TestGeminiStream_CoalescesConsecutiveTextDeltas(t *testing.T) {
	sse := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"one "}]}}]}` + "\n\n" +
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"two"}]}}]}` + "\n\n"
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText {
				texts = append(texts, b.Text)
			}
		}
	}
	if len(texts) != 1 || texts[0] != "one two" {
		t.Fatalf("consecutive deltas must merge into one block, got %v", texts)
	}
}

// Round 6. The ordered accumulator skipped reasoning parts, so text arriving
// either side of a thinking pass stayed adjacent and fused — the A+B→AB
// defect the accumulator existed to remove, surviving on the one kind it
// did not cover.
func TestGeminiStream_ReasoningKeepsItsPlaceInTheOrder(t *testing.T) {
	sse := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"A"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"T","thought":true}]}}]}` + "\n\n" +
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"B"}]}}]}` + "\n\n"
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			shape = append(shape, string(b.Type)+":"+b.Text)
		}
	}
	want := []string{"text:A", "reasoning:T", "text:B"}
	if len(shape) != len(want) {
		t.Fatalf("got %v, want %v", shape, want)
	}
	for i := range want {
		if shape[i] != want[i] {
			t.Fatalf("got %v, want %v — speech either side of a thinking pass was fused", shape, want)
		}
	}
}

// Round 7. The unrecognised-part marker was appended raw, so it became a
// coalescing target and the next text delta fused into it: the model's word
// glued to synthetic text, and one projection entry where there should be
// two. Same class as the reasoning fix, on the kind that sweep skipped.
func TestGeminiStream_MarkerDoesNotSwallowTheNextDelta(t *testing.T) {
	sse := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Alpha. "}]}}]}` + "\n\n" +
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"somethingNew":{"x":1}}]}}]}` + "\n\n" +
		`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Beta."}]}}]}` + "\n\n"
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText {
				texts = append(texts, b.Text)
			}
		}
	}
	if len(texts) != 3 {
		t.Fatalf("want three separate blocks, got %d: %q", len(texts), texts)
	}
	if texts[2] != "Beta." {
		t.Fatalf("the model's word was glued to the marker: %q", texts[2])
	}
}

// Round 7. Every round-6 leak fix was correct and unfalsifiable: mutations
// reverting them all survived. These drive each guarded site with a
// wire-controlled value large enough that an unrouted path is unmistakable.
func TestSyntheticRenderingsAreBoundedAtEverySite(t *testing.T) {
	huge := strings.Repeat("T", 65536)

	t.Run("anthropic nested-content shape summary, array branch", func(t *testing.T) {
		// Exactly six wrappers, measured rather than guessed. At five or
		// fewer the node is projected normally and the summary never runs;
		// at seven or more the summary sees only the wrapper's own type
		// ("document") and reports `document x1`. Only at six does the
		// wire-controlled value reach the array branch — three earlier
		// versions of this fixture used 7, 2 and 5 and all passed while
		// proving nothing.
		node := any(map[string]any{"type": huge})
		for range 6 {
			node = map[string]any{"type": "document",
				"source": map[string]any{"type": "content", "content": []any{node}}}
		}
		body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 8,
			"messages": []any{map[string]any{"role": "user", "content": []any{node}}}})
		p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
		assertAllTextBounded(t, p, err)
	})

	t.Run("anthropic nested-content shape summary, object branch", func(t *testing.T) {
		node := any(map[string]any{"type": "document",
			"source": map[string]any{"type": "content", "content": map[string]any{huge: "x"}}})
		for range 2 {
			node = map[string]any{"type": "document",
				"source": map[string]any{"type": "content", "content": []any{node}}}
		}
		body, _ := json.Marshal(map[string]any{"model": "m", "max_tokens": 8,
			"messages": []any{map[string]any{"role": "user", "content": []any{node}}}})
		p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), body, core.Meta{Direction: core.DirectionRequest})
		assertAllTextBounded(t, p, err)
	})

	t.Run("gemini unrecognised part key list", func(t *testing.T) {
		body := `{"contents":[{"role":"user","parts":[{"` + huge + `":{"x":1}}]}]}`
		p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
		assertAllTextBounded(t, p, err)
	})

	t.Run("anthropic stream block type in the prefix position", func(t *testing.T) {
		sse := "event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"` + huge + `"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(), []byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
		assertAllTextBounded(t, p, err)
	})
}

func assertAllTextBounded(t *testing.T, p core.NormalizedPayload, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && len(b.Text) > 1100 {
				t.Fatalf("a wire-controlled value reached a text block unbounded: %d chars", len(b.Text))
			}
		}
	}
}

// Round 7. Replicate serves every model on its platform, and the codec piped
// `output` into a text block whatever it was: a data URI became a 20 KB text
// block with no MediaRef, and a generated-image URL became prose with no
// custody record. The headline defect and its inverted-custody sibling,
// alive on a codec the media unification never reached.
func TestReplicateOutput_ClassifiesPerElement(t *testing.T) {
	big := strings.Repeat("QUJD", 5000)
	for _, tc := range []struct {
		name, output string
		wantText     []string
		wantMedia    []string // expected Source per media block, in order
	}{
		{"a leading space must not smuggle a payload through as prose",
			`" data:image/png;base64,` + big + `"`, nil, []string{core.MediaCaptured}},
		{"a caption beside an artifact stays its own text block",
			`["Here you go: ","data:image/png;base64,` + big + `"]`,
			[]string{"Here you go: "}, []string{core.MediaCaptured}},
		{"two artifacts stay two references, never one fused URL",
			`["https://replicate.delivery/a/out-0.png","https://replicate.delivery/a/out-1.png"]`,
			nil, []string{core.MediaExternal, core.MediaExternal}},
		{"prose that merely begins with a URL is prose",
			`"https://example.com is the official site, founded in 1998."`,
			[]string{"https://example.com is the official site, founded in 1998."}, nil},
		{"streamed token fragments coalesce into one utterance",
			`["Hel","lo ","world"]`, []string{"Hello world"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewReplicateNormalizer().Normalize(context.Background(),
				[]byte(`{"status":"succeeded","output":`+tc.output+`}`),
				core.Meta{Direction: core.DirectionResponse})
			if err != nil {
				t.Fatal(err)
			}
			var texts, sources []string
			for _, m := range p.Messages {
				for _, b := range m.Content {
					switch b.Type {
					case core.ContentText:
						// No length assertion here. `output` is a declared
						// text field and this package preserves those
						// verbatim, so a bound would contradict
						// TestReplicateDeclaredTextSurvivesVerbatim over the
						// same field — two tests, one file, opposite rules.
						// The table below asserts exact contents instead,
						// which is stronger than any ceiling.
						texts = append(texts, b.Text)
					case core.ContentMedia:
						sources = append(sources, b.MediaRef.Source)
						if strings.Contains(b.MediaRef.URL, "out-0.pnghttps") {
							t.Fatalf("two outputs fused into one fabricated URL: %q", b.MediaRef.URL)
						}
					}
				}
			}
			if len(texts) != len(tc.wantText) {
				t.Fatalf("text blocks %q, want %q", texts, tc.wantText)
			}
			for i := range tc.wantText {
				if texts[i] != tc.wantText[i] {
					t.Errorf("text[%d] = %q, want %q", i, texts[i], tc.wantText[i])
				}
			}
			if len(sources) != len(tc.wantMedia) {
				t.Fatalf("media sources %v, want %v", sources, tc.wantMedia)
			}
			for i := range tc.wantMedia {
				if sources[i] != tc.wantMedia[i] {
					t.Errorf("media[%d] source = %q, want %q", i, sources[i], tc.wantMedia[i])
				}
			}
		})
	}
}

func TestReplicateOutput_MediaIsNotProse(t *testing.T) {
	t.Run("a data URI is media, not a text block", func(t *testing.T) {
		body := `{"status":"succeeded","output":"data:image/png;base64,` + strings.Repeat("QUJD", 5000) + `"}`
		p, err := NewReplicateNormalizer().Normalize(context.Background(),
			[]byte(body), core.Meta{Direction: core.DirectionResponse})
		if err != nil {
			t.Fatal(err)
		}
		// A clean data URI must classify as media, so no text block should
		// exist at all — asserted directly rather than by a size ceiling,
		// which was the observation-tool mistake that originally hid the
		// Anthropic PDF defect.
		for _, m := range p.Messages {
			for _, b := range m.Content {
				if b.Type == core.ContentText {
					t.Fatalf("a whole-value data URI produced a text block: %.60q", b.Text)
				}
			}
		}
		ref := firstMedia(t, p)
		if ref.Modality != core.ModalityImage || ref.URL != "" {
			t.Fatalf("a data URI must be media with no URL: %+v", ref)
		}
	})
	t.Run("a generated-media URL carries custody", func(t *testing.T) {
		body := `{"status":"succeeded","output":"https://replicate.delivery/pbxt/abc/out-0.png"}`
		p, err := NewReplicateNormalizer().Normalize(context.Background(),
			[]byte(body), core.Meta{Direction: core.DirectionResponse})
		if err != nil {
			t.Fatal(err)
		}
		ref := firstMedia(t, p)
		if ref.Source != core.MediaExternal || ref.URL == "" {
			t.Fatalf("provider-hosted output is an external reference: %+v", ref)
		}
	})
}

// Round 8, D7. The bounds sliced by byte, so truncation split a multi-byte
// character. Invalid UTF-8 is rejected outright by a Postgres text column,
// so a wire-controlled value with any non-ASCII in it would fail the audit
// write rather than merely render oddly.
func TestSyntheticRenderingsAreValidUTF8(t *testing.T) {
	wide := "x" + strings.Repeat("é", 300)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"` + wide + `"}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if !utf8.ValidString(b.Text) {
				t.Fatalf("truncation split a rune: %q", b.Text)
			}
		}
	}
	// Direct, so the helper is pinned independently of any one call site.
	for _, s := range []string{
		payloadSafeText("é"+strings.Repeat("漢", 200), "tail"),
		payloadSafeText("p: ", strings.Repeat("漢", 500)),
		payloadSafeJSON(map[string]any{"k": strings.Repeat("漢", 500)}),
	} {
		if !utf8.ValidString(s) {
			t.Fatalf("invalid UTF-8: %q", s)
		}
	}
}

// Round 8, D2 and D3. Two coalescing edges the suite could not see: the
// seal must RESET after the block that follows a marker (or coalescing is
// disabled for the rest of that candidate), and a merge must target the
// TRAILING block (or a delta lands in a media block's Text field).
func TestGeminiStream_CoalescingEdges(t *testing.T) {
	t.Run("the seal releases after the block that follows a marker", func(t *testing.T) {
		sse := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"somethingNew":{"x":1}}]}}]}` + "\n\n" +
			`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"one "}]}}]}` + "\n\n" +
			`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"two"}]}}]}` + "\n\n"
		p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
			[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, m := range p.Messages {
			for _, b := range m.Content {
				if b.Type == core.ContentText {
					texts = append(texts, b.Text)
				}
			}
		}
		// marker, then ONE block holding both deltas.
		if len(texts) != 2 || texts[1] != "one two" {
			t.Fatalf("coalescing stayed disabled after the marker: %q", texts)
		}
	})

	t.Run("a delta merges into the trailing block, never the first", func(t *testing.T) {
		sse := `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"QUJD"}}]}}]}` + "\n\n" +
			`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"one "}]}}]}` + "\n\n" +
			`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"two"}]}}]}` + "\n\n"
		p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(),
			[]byte(sse), core.Meta{Direction: core.DirectionResponse, Stream: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range p.Messages {
			for _, b := range m.Content {
				if b.Type == core.ContentMedia && b.Text != "" {
					t.Fatalf("a text delta was written into a media block: %q", b.Text)
				}
			}
		}
		ref := firstMedia(t, p)
		if ref.Source != core.MediaCaptured {
			t.Fatalf("the media element was corrupted: %+v", ref)
		}
	})
}

// Round 9. D7 was pinned only at payloadSafeText; the budgetWriter call site
// — where the budget runs out mid-render — was never reached, so reverting
// its rune-safe truncation failed nothing. Many SHORT multi-byte values are
// what drive it: each fits under the per-value cap, so nothing is elided and
// the budget is what finally cuts, mid-character.
func TestBudgetTruncationIsRuneSafe(t *testing.T) {
	// Width MEASURED, not guessed: a sweep showed 1–5 characters per value
	// puts the budget cut inside a character, while 10 happens to land on a
	// boundary and proves nothing. The first version of this test used 10.
	many := make([]any, 200)
	for i := range many {
		many[i] = strings.Repeat("漢", 3)
	}
	got := payloadSafeJSON(many)
	if utf8.ValidString(got) == false {
		t.Fatalf("budget truncation split a rune: tail=%q", got[len(got)-16:])
	}
	if len(got) > 1100 {
		t.Fatalf("budget not enforced: %d chars", len(got))
	}
	// Same through a real codec, so the property holds end to end. The
	// shape is MEASURED too: an earlier version routed multi-byte text
	// through geminiPartKeys, which renders KEY NAMES and discards the
	// values — 200 blocks, none multi-byte, longest 39 bytes, nothing
	// truncated. It asserted UTF-8 validity of ASCII 200 times and would
	// have passed identically with "xxx". A sweep over value width and key
	// count found 30 runes across 12 keys is what actually drives the
	// budget cut into a character on this path.
	var sb strings.Builder
	sb.WriteString(`{"model":"m","messages":[{"role":"user","content":[{"type":"mystery"`)
	for k := range 12 {
		fmt.Fprintf(&sb, `,"k%02d":"%s"`, k, strings.Repeat("漢", 30))
	}
	sb.WriteString(`}]}]}`)

	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(),
		[]byte(sb.String()), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatal(err)
	}
	var sawTruncatedMultibyte bool
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if !utf8.ValidString(b.Text) {
				t.Fatalf("codec emitted invalid UTF-8: %q", b.Text)
			}
			if strings.Contains(b.Text, "漢") && len(b.Text) > 900 {
				sawTruncatedMultibyte = true
			}
		}
	}
	// Without this the assertion above is vacuous: it would pass on any
	// codec that never emitted a multi-byte character at all.
	if !sawTruncatedMultibyte {
		t.Fatal("fixture no longer drives multi-byte text to the budget cut — re-measure before trusting this test")
	}
}

// Round 9. The caption-ordering claim in replicateOutputBlocks was unpinned:
// the existing test collected texts and media into separate slices and
// compared each independently, so captions landing after the artifacts —
// and fusing into one — failed nothing.
func TestReplicateOutput_CaptionsKeepTheirPlace(t *testing.T) {
	body := `{"status":"succeeded","output":[` +
		`"Here you go: ","https://replicate.delivery/a/out-0.png",` +
		`"and another: ","https://replicate.delivery/a/out-1.png"]}`
	p, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case core.ContentText:
				shape = append(shape, "text:"+b.Text)
			case core.ContentMedia:
				shape = append(shape, "media")
			}
		}
	}
	want := []string{"text:Here you go: ", "media", "text:and another: ", "media"}
	if len(shape) != len(want) {
		t.Fatalf("got %v, want %v", shape, want)
	}
	for i := range want {
		if shape[i] != want[i] {
			t.Fatalf("got %v, want %v — captions were reordered or fused", shape, want)
		}
	}
}

// Round 9. mimeFromURLPath added eight mappings and executed none of them,
// so a generated artifact's modality was unverified on every extension.
func TestReplicateURLMimeDrivesModality(t *testing.T) {
	for ext, want := range map[string]string{
		".png": core.ModalityImage, ".jpg": core.ModalityImage,
		".webp": core.ModalityImage, ".gif": core.ModalityImage,
		".mp4": core.ModalityVideo, ".webm": core.ModalityVideo,
		".wav": core.ModalityAudio, ".mp3": core.ModalityAudio,
		".bin": core.ModalityFile,
	} {
		body := `{"status":"succeeded","output":"https://replicate.delivery/a/out-0` + ext + `"}`
		p, err := NewReplicateNormalizer().Normalize(context.Background(),
			[]byte(body), core.Meta{Direction: core.DirectionResponse})
		if err != nil {
			t.Fatalf("%s: %v", ext, err)
		}
		if got := firstMedia(t, p).Modality; got != want {
			t.Errorf("%s → modality %q, want %q", ext, got, want)
		}
	}
	// A query string must not defeat the extension read.
	body := `{"status":"succeeded","output":"https://replicate.delivery/a/out-0.png?expires=1"}`
	p, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatal(err)
	}
	if got := firstMedia(t, p).Modality; got != core.ModalityImage {
		t.Errorf("query string defeated the extension read: %q", got)
	}
}

// Each half of payloadSafeText is bounded on its own line. Both are
// wire-derived, so a guard on one leaves the other unbounded — and with a
// whole-result bound present, deleting either half went undetected because
// the outer bound caught the pair. These assert absolute byte counts, not
// expressions in the constants, so widening a constant cannot make them pass.
func TestPayloadSafeTextBoundsEachHalfIndependently(t *testing.T) {
	huge := strings.Repeat("x", 65536)

	if got := payloadSafeText(huge, "short"); len(got) > 200 {
		t.Fatalf("an unbounded PREFIX reaches the output: %d bytes", len(got))
	}
	if got := payloadSafeText("short: ", huge); len(got) > 200 {
		t.Fatalf("an unbounded BODY reaches the output: %d bytes", len(got))
	}
	if got := payloadSafeText(huge, huge); len(got) > 200 {
		t.Fatalf("both halves unbounded: %d bytes", len(got))
	}
	// The short case must survive intact — a bound that eats real content
	// is not a bound, it is data loss.
	if got := payloadSafeText("tool: ", "ok"); got != "tool: ok" {
		t.Fatalf("short input must pass through verbatim, got %q", got)
	}
}

// The streamed locator is the only one the codec must COMPUTE rather than
// concatenate: it names the SSE frame the bytes landed in. It was pinned
// only as a helper unit test, so an off-by-one in the frame index the codec
// passes was invisible end to end — and a wrong frame index means the
// artifact endpoint reads the wrong frame and serves the wrong bytes.
func TestStreamedMediaLocatorAddressesItsOwnFrame(t *testing.T) {
	png := "iVBORw0KGgo="
	body := "data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"here"}]}}]}` + "\n\n" +
		"data: " + `{"candidates":[{"content":{"parts":[{"text":" it is"}]}}]}` + "\n\n" +
		"data: " + `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + png + `"}}]},"finishReason":"STOP"}]}` + "\n\n"

	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	const want = "sse:2:candidates.0.content.parts.0.inlineData.data"
	if ref.Locator != want {
		t.Fatalf("streamed path = %q, want %q — the frame index must be the frame the bytes arrived in", ref.Locator, want)
	}
	if ref.Source != core.MediaCaptured {
		t.Fatalf("inline streamed bytes are captured: %+v", ref)
	}
}

// A multi-artifact response addresses each artifact at its own index. All
// of them sharing index 0 still yields a locator per ref and still passes
// any "has a path" assertion, while Download on artifacts 2..n serves
// artifact 1.
func TestReplicateArrayArtifactsAddressTheirOwnIndex(t *testing.T) {
	// INLINE elements, deliberately. An https:// fixture routes through
	// externalMedia, which never receives the locator path — so the index
	// this test is named for was computed, discarded, and asserted nowhere.
	// The mutation `locator.JoinPath("output", i*0)` survived the whole suite until
	// the fixture changed shape.
	png := "iVBORw0KGgo="
	body := `{"output":["data:image/png;base64,` + png + `","data:image/jpeg;base64,` + png + `","data:image/webp;base64,` + png + `"]}`
	p, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var locs, mimes []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentMedia && b.MediaRef != nil {
				locs = append(locs, b.MediaRef.Locator)
				mimes = append(mimes, b.MediaRef.Mime)
			}
		}
	}
	wantLocs := []string{"datauri:output.0", "datauri:output.1", "datauri:output.2"}
	if len(locs) != len(wantLocs) {
		t.Fatalf("want %d artifacts, got %d", len(wantLocs), len(locs))
	}
	for i := range wantLocs {
		if locs[i] != wantLocs[i] {
			t.Fatalf("artifact %d path = %q, want %q — Download on this artifact would serve another one", i, locs[i], wantLocs[i])
		}
	}
	// Mime drives the served Content-Type and the download filename, so it
	// is pinned per artifact, not merely to the right modality class.
	wantMimes := []string{"image/png", "image/jpeg", "image/webp"}
	for i := range wantMimes {
		if mimes[i] != wantMimes[i] {
			t.Fatalf("artifact %d mime = %q, want %q", i, mimes[i], wantMimes[i])
		}
	}

	// The URL form keeps its own pin: mime from the extension, identity and
	// order preserved. It cannot pin the index, which is why it is no longer
	// the only fixture here.
	pu, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(`{"output":["https://r.example/a.png","https://r.example/b.jpg","https://r.example/c.webp"]}`),
		core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var urls []string
	for _, m := range pu.Messages {
		for _, b := range m.Content {
			if b.MediaRef != nil {
				urls = append(urls, b.MediaRef.URL+" "+b.MediaRef.Mime)
			}
		}
	}
	wantURLs := []string{"https://r.example/a.png image/png", "https://r.example/b.jpg image/jpeg", "https://r.example/c.webp image/webp"}
	if len(urls) != 3 {
		t.Fatalf("want 3 external artifacts, got %d", len(urls))
	}
	for i := range wantURLs {
		if urls[i] != wantURLs[i] {
			t.Fatalf("external artifact %d = %q, want %q", i, urls[i], wantURLs[i])
		}
	}
}

// An extension is case-insensitive on the wire. Losing the fold sends
// image/png down as an unknown file, which changes both the modality the UI
// renders and the Content-Type the endpoint serves.
func TestReplicateURLExtensionIsCaseInsensitive(t *testing.T) {
	for _, u := range []string{"https://r.example/OUT.PNG", "https://r.example/Out.Png"} {
		ref := externalMedia(mimeFromURLPath(u), u, "")
		if ref.Mime != "image/png" || ref.Modality != core.ModalityImage {
			t.Fatalf("%s → mime=%q modality=%q, want image/png + image", u, ref.Mime, ref.Modality)
		}
	}
}

// budgetWriter's job is to BOUND, and only its rune-safety was pinned —
// writing an over-budget value whole survived every mutation. The bound is
// asserted as an absolute byte count so widening the constant cannot make
// this pass.
func TestBudgetWriterRefusesToOvershoot(t *testing.T) {
	wide := make([]any, 400)
	for i := range wide {
		wide[i] = strings.Repeat("z", 90) // under the per-value cap, so only the total can stop it
	}
	got := payloadSafeJSON(wide)
	// 1055 measured with the bound in place; 1147 with an over-budget
	// value written whole. An absolute literal, so widening the budget
	// constant cannot make this pass.
	if len(got) > 1100 {
		t.Fatalf("total budget not enforced: %d bytes", len(got))
	}
}

// Replicate's object branch reads the first present key in a fixed order.
// Neither the classification nor the precedence was asserted, so an object
// answer could silently become nothing at all.
func TestReplicateObjectOutputPrecedence(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"text wins over answer", `{"output":{"answer":"B","text":"A"}}`, "A"},
		{"answer wins over completion", `{"output":{"completion":"C","answer":"B"}}`, "B"},
		{"completion wins over message", `{"output":{"message":"D","completion":"C"}}`, "C"},
		{"message is the last resort", `{"output":{"message":"D"}}`, "D"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(tc.body), core.Meta{Direction: core.DirectionResponse})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			var texts []string
			for _, m := range p.Messages {
				for _, b := range m.Content {
					if b.Type == core.ContentText {
						texts = append(texts, b.Text)
					}
				}
			}
			if len(texts) != 1 || texts[0] != tc.want {
				t.Fatalf("object output = %v, want exactly [%q]", texts, tc.want)
			}
		})
	}

	// A media reference under one of those keys is still media, not prose.
	p, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(`{"output":{"text":"https://r.example/a.mp4"}}`), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Modality != core.ModalityVideo || ref.URL != "https://r.example/a.mp4" {
		t.Fatalf("a URL under a text key is still an artifact: %+v", ref)
	}
}

// A streamed response may carry several candidates. They are emitted in
// arrival order, one message each — collapsing them loses whole answers,
// and reordering attributes text to the wrong candidate.
func TestGeminiStreamKeepsEveryCandidateInOrder(t *testing.T) {
	body := "data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"first"}]}},{"index":1,"content":{"role":"model","parts":[{"text":"second"}]}}]}` + "\n\n" +
		"data: " + `{"candidates":[{"index":1,"content":{"parts":[{"text":" more"}]},"finishReason":"STOP"},{"index":0,"content":{"parts":[{"text":" also"}]},"finishReason":"STOP"}]}` + "\n\n"

	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(p.Messages) != 2 {
		t.Fatalf("want one message per candidate, got %d", len(p.Messages))
	}
	want := []string{"first also", "second more"}
	for i, w := range want {
		var got string
		for _, b := range p.Messages[i].Content {
			got += b.Text
		}
		if got != w {
			t.Fatalf("candidate %d = %q, want %q — deltas must stitch to their OWN candidate, in arrival order", i, got, w)
		}
	}
}

// A streamed chunk may omit role entirely; every chunk after the first
// usually does. Without a default the message carries an empty role, which
// renders as neither user nor assistant.
func TestGeminiStreamDefaultsRoleToAssistant(t *testing.T) {
	body := "data: " + `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}` + "\n\n"
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("a roleless streamed candidate is the assistant, got %+v", p.Messages)
	}
}

// The streamed fileData arm was wholly unexercised. A Files-API URI is
// https exactly like a public link, so the custody label is the only thing
// distinguishing provider-held media from media Nexus never touched.
func TestGeminiStreamFileDataCustody(t *testing.T) {
	const uri = "https://generativelanguage.googleapis.com/v1beta/files/abc123"
	body := "data: " + `{"candidates":[{"content":{"role":"model","parts":[{"fileData":{"mimeType":"video/mp4","fileUri":"` + uri + `"}}]},"finishReason":"STOP"}]}` + "\n\n"
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Source != core.MediaProviderRef || ref.ProviderRef != uri {
		t.Fatalf("a streamed Files-API URI is provider-held: %+v", ref)
	}
	if ref.Modality != core.ModalityVideo || ref.URL != "" {
		t.Fatalf("provider-held media carries no fetchable URL: %+v", ref)
	}
}

// The stream normalizer also accepts a whole non-SSE body — a provider that
// answered a stream request without streaming. That arm defaults the role
// too, and defaulted it a second, different way until the shapes converged.
func TestGeminiStreamNonSSEBodyDefaultsRole(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body),
		core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != core.RoleAssistant {
		t.Fatalf("a roleless non-SSE candidate is the assistant, got %+v", p.Messages)
	}
}

// Round 11. payloadSafeRaw's unparseable arm is the same guard, the same
// magnitude and the same door as the leak that started this: a body whose
// content array does not decode falls back to verbatim wire bytes. Round 10
// audited payloadSafeText's bounds and never looked at the structurally
// identical bound twelve lines above it.
//
// Reaching the arm needs a sub-document the JSON decoder hands back
// unvalidated — a whole-body parse failure is rejected earlier — so the
// fixture is malformed INSIDE `content`, not at the top level.
func TestUnparseableBodyIsBoundedNotEchoed(t *testing.T) {
	secret := "4111 1111 1111 1111"
	filler := strings.Repeat("A", 40000)

	for _, tc := range []struct {
		name string
		body string
		norm func([]byte) (core.NormalizedPayload, error)
	}{
		{
			"anthropic content array",
			`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` + filler + secret + `}]}]}`,
			func(b []byte) (core.NormalizedPayload, error) {
				return NewAnthropicMessagesNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
		{
			"cohere content",
			`{"model":"m","messages":[{"role":"user","content":[` + filler + secret + `}]}]}`,
			func(b []byte) (core.NormalizedPayload, error) {
				return NewCohereChatNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.norm([]byte(tc.body))
			if err != nil {
				return // a codec that rejects the body outright never reaches the arm
			}
			for _, m := range p.Messages {
				for _, b := range m.Content {
					if b.Type != core.ContentText {
						continue
					}
					if len(b.Text) > 1100 {
						t.Fatalf("undecodable body echoed verbatim: %d bytes", len(b.Text))
					}
					if strings.Contains(b.Text, secret) {
						t.Fatalf("payload reached a scanned text block: %.80q", b.Text)
					}
				}
			}
		})
	}
}

// Round 11. Converging the two role defaults changed behaviour, not only
// shape: the non-SSE arm used to map first and override the RESULT, so a
// candidate declaring a role other than model was overridden too. Only the
// empty-role default was pinned, in both arms — the passthrough in neither,
// which is the half the convergence actually altered.
func TestGeminiStreamCandidateRoleIsCarriedNotOverridden(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       core.Role
	}{
		{"sse fold, declared role", "data: " + `{"candidates":[{"content":{"role":"user","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}` + "\n\n", core.RoleUser},
		{"sse fold, model role", "data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}` + "\n\n", core.RoleAssistant},
		{"non-SSE body, declared role", `{"candidates":[{"content":{"role":"user","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`, core.RoleUser},
		{"non-SSE body, model role", `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`, core.RoleAssistant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(tc.body),
				core.Meta{Direction: core.DirectionResponse, Stream: true})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if len(p.Messages) != 1 || p.Messages[0].Role != tc.want {
				t.Fatalf("role = %+v, want %q — a declared role is carried, not replaced", p.Messages, tc.want)
			}
		})
	}
}

// Round 11. The recursion depth cap was unpinned: deleting it left the
// budget as the only stop, so a pathologically nested value walked ~1024
// levels instead of 8 before the output filled. The budget makes it
// survivable, not free — this is the guard that keeps the walk shallow.
func TestPayloadSafeJSONStopsDescendingAtTheDepthCap(t *testing.T) {
	v := any("leaf")
	for range 40 {
		v = map[string]any{"n": v}
	}
	got := payloadSafeJSON(v)
	if strings.Contains(got, "leaf") {
		t.Fatalf("the walk reached a leaf 40 levels down: %q", got)
	}
	// Depth 8 of single-key wrappers, so the nesting visible in the output
	// is bounded well below the budget — an absolute count, not an
	// expression in the constant.
	if n := strings.Count(got, `"n"`); n > 10 {
		t.Fatalf("descended %d levels, cap is 8: %q", n, got)
	}
}

// Round 13. `output` is a DECLARED text field — the model's own answer —
// and this package's rule for those is that they survive verbatim
// (media.go, payloadSafeMaxValue). Two attempts to bound a base64 run
// inside it each deleted the model's words:
//
//   - Round 11 unwrapped line breaks to reclassify the value as media.
//     `…QUJD\nDone` fused, because D, o, n and e are base64 characters, and
//     produced a VALID-looking 6-byte capture of something that is not the
//     image. Two adjacent URIs fused into a third that never existed.
//   - Round 12 elided the run instead. The run must cross line breaks to
//     cover a wrapped payload, and once it does it eats the base64-alphabet
//     word after the break: `…\nDone` lost `Done`, `…\nThe image is ready`
//     lost `The`.
//
// Neither is a tuning problem. Nothing separates "payload the classifier
// missed" from "base64 the model chose to emit" — segment lengths and the
// alphabet accept `QUJD\nDone` exactly as they accept a genuine wrap — and
// no measured provider wire RFC-2045-wraps a JSON string value at all.
//
// So the classifier stays strict and prose stays untouched. What this pins
// is that neither bound comes back.
func TestReplicateDeclaredTextSurvivesVerbatim(t *testing.T) {
	payload := strings.Repeat("QUJD", 5000)
	var lines []string
	for i := 0; i < len(payload); i += 76 {
		end := i + 76
		if end > len(payload) {
			end = len(payload)
		}
		lines = append(lines, payload[i:end])
	}
	wrapped := "data:image/png;base64," + strings.Join(lines, "\r\n")

	for _, tc := range []struct {
		name, value string
		wantMedia   bool
	}{
		{"a clean data URI is media", "data:image/png;base64,iVBORw0KGgo=", true},
		{"a URL is media", "https://r.example/a.png", true},
		{"a padded data URI is still media", "  data:image/png;base64,iVBORw0KGgo=\n", true},
		{"a wrapped payload is prose, whole", wrapped, false},
		{"a base64-alphabet word after a large payload keeps its place", "data:image/png;base64," + payload + "\nDone", false},
		{"a sentence after a large payload keeps its first word", "data:image/png;base64," + payload + "\nThe image is ready.", false},
		{"CJK prose after a URI survives", "data:image/png;base64,QUJD\n\u8fd9\u662f\u56fe\u7247", false},
		{"an ideographic space does not hide the sentence", "data:image/png;base64,QUJD\u3000\u8fd9\u662f\u56fe\u7247", false},
		{"a line separator does not hide the sentence", "data:image/png;base64,QUJD\u2028more", false},
		{"two large data URIs do not fuse or lose a byte", "data:a/b;base64," + payload + "\ndata:c/d;base64," + payload, false},
		{"prose that mentions a URI is prose", "see data:image/png;base64,QUJD for it", false},
		{"plain prose is untouched", "the answer is 42", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.value)
			p, err := NewReplicateNormalizer().Normalize(context.Background(),
				[]byte(`{"output":`+string(raw)+`}`), core.Meta{Direction: core.DirectionResponse})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			var texts []string
			var media []*core.MediaRef
			for _, m := range p.Messages {
				for _, b := range m.Content {
					if b.Type == core.ContentText {
						texts = append(texts, b.Text)
					} else if b.MediaRef != nil {
						media = append(media, b.MediaRef)
					}
				}
			}
			if tc.wantMedia {
				if len(media) != 1 || len(texts) != 0 {
					t.Fatalf("want one media block, got %d media / %d text", len(media), len(texts))
				}
				if media[0].Truncated {
					t.Fatalf("a clean reference must not be truncated: %+v", media[0])
				}
				// The CUSTODY, not merely a ref: this branch passed with
				// every inline data URI degraded to `absent`, which is the
				// difference between "openable" and "we saw something".
				want := core.MediaCaptured
				if strings.HasPrefix(strings.TrimSpace(tc.value), "https://") {
					want = core.MediaExternal
				}
				if media[0].Source != want {
					t.Fatalf("source = %q, want %q: %+v", media[0].Source, want, media[0])
				}
				if want == core.MediaCaptured && (media[0].Locator == "" || media[0].SizeBytes <= 0) {
					t.Fatalf("captured bytes must be addressable and sized: %+v", media[0])
				}
				return
			}
			if len(media) != 0 {
				t.Fatalf("the model's words were swallowed as media: %+v", media[0])
			}
			// Byte-for-byte. Every earlier bound passed a "looks right" check
			// while deleting one word from the middle.
			if len(texts) != 1 || texts[0] != tc.value {
				t.Fatalf("declared text was altered:\n got %.90q (%d bytes)\nwant %.90q (%d bytes)",
					strings.Join(texts, "|"), len(strings.Join(texts, "")), tc.value, len(tc.value))
			}
		})
	}
}

// Round 12. A data URI may declare no mime at all. When it does, the FIELD
// it arrived in is the better evidence — `image_url` says image whatever
// the URI omits — and without the override modalityFromMime("") would call
// it a file, so the card renders a download instead of a preview.
func TestFieldModalityWinsWhenTheDataURIDeclaresNoMime(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		norm       func([]byte) (core.NormalizedPayload, error)
	}{
		{
			"openai chat image_url",
			`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:;base64,iVBORw0KGgo="}}]}]}`,
			func(b []byte) (core.NormalizedPayload, error) {
				return NewOpenAIChatNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
		{
			"responses input_image",
			`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:;base64,iVBORw0KGgo="}]}]}`,
			func(b []byte) (core.NormalizedPayload, error) {
				return NewOpenAIResponsesNormalizer().Normalize(context.Background(), b, core.Meta{Direction: core.DirectionRequest})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.norm([]byte(tc.body))
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			ref := firstMedia(t, p)
			if ref.Modality != core.ModalityImage {
				t.Fatalf("modality = %q, want image — the field declared it: %+v", ref.Modality, ref)
			}
			if ref.Source != core.MediaCaptured {
				t.Fatalf("the bytes are still captured: %+v", ref)
			}
		})
	}
}

// Round 14. The whole-value doctrine — a reference is the ENTIRE value, never
// a prefix or a substring — was carried only by cases containing spaces, so
// the whitespace guard decided them and the doctrine itself was never
// exercised. These have no whitespace at all, so nothing but the doctrine
// can classify them.
func TestReplicateReferenceMustBeTheWholeValue(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"a data URI embedded in a hyphenated token", "see-data:image/png;base64,QUJD"},
		{"a URL embedded in a hyphenated token", "link-https://r.example/a.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.value)
			p, err := NewReplicateNormalizer().Normalize(context.Background(),
				[]byte(`{"output":`+string(raw)+`}`), core.Meta{Direction: core.DirectionResponse})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			for _, m := range p.Messages {
				for _, b := range m.Content {
					if b.Type == core.ContentMedia {
						t.Fatalf("a substring match produced a reference, and the prose went into it: %+v", b.MediaRef)
					}
					if b.Text != tc.value {
						t.Fatalf("text = %q, want %q", b.Text, tc.value)
					}
				}
			}
		})
	}

	// The converse, so the doctrine is not read as "anything unusual is
	// prose": a value that IS a whole data URI stays a reference even when
	// its payload is corrupt. `!!` is inside the payload as far as the URI
	// grammar is concerned, so this degrades honestly rather than
	// reclassifying — the ref says truncated and offers nothing to open.
	p, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(`{"output":"data:image/png;base64,QUJD!!"}`), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if !ref.Truncated || ref.Cause != "undecodable-base64" || ref.SizeBytes != 0 {
		t.Fatalf("a corrupt payload must degrade honestly, not claim bytes: %+v", ref)
	}
}

// Round 14. A non-string element used to vanish from the array with no
// marker, while Confidence still reported a good parse — the no-silent-drop
// rule, broken in the one codec whose output field is polymorphic.
func TestReplicateArrayKeepsUnrecognisedElements(t *testing.T) {
	p, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(`{"output":["a",{"caption":"a picture of a cat"},"c"]}`),
		core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var texts []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) != 3 {
		t.Fatalf("want three blocks in wire order, got %d: %q", len(texts), texts)
	}
	if texts[0] != "a" || texts[2] != "c" {
		t.Fatalf("the recognised elements must keep their place: %q", texts)
	}
	if !strings.Contains(texts[1], "unrecognised replicate output element") {
		t.Fatalf("the dropped element must be named, not silently gone: %q", texts[1])
	}
	// Named, and bounded — it is a rendering of wire structure, not
	// declared text, so the verbatim rule does not apply to it.
	if len(texts[1]) > 1100 {
		t.Fatalf("the marker is a synthetic block and must stay bounded: %d", len(texts[1]))
	}
	if !strings.Contains(texts[1], "caption") {
		t.Fatalf("the marker must say what it saw: %q", texts[1])
	}
}

// Round 14. Every prefix test in the media builders runs against a
// client-controlled string, so an untrimmed comparison is not a guard at
// all: one leading space walked a whole data URI past both of them and
// parked 20 023 characters in MediaRef.URL — a reference field holding the
// payload, which is the defect this file exists for.
func TestPaddedDataURIDoesNotReachTheURLField(t *testing.T) {
	big := strings.Repeat("QUJD", 5000)

	// Through a real codec, since that is how the string arrives.
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":" data:image/png;base64,` + big + `"}}]}]}`
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.URL != "" {
		t.Fatalf("payload landed in the URL field: %d chars", len(ref.URL))
	}
	if ref.Source != core.MediaCaptured || ref.SizeBytes != 15000 {
		t.Fatalf("a padded data URI is still captured bytes: %+v", ref)
	}

	// And directly at the class site, which four of the six callers reach.
	if got := externalMedia("", " data:image/png;base64,"+big, ""); got.URL != "" {
		t.Fatalf("externalMedia stored the payload: %d chars", len(got.URL))
	}

	// The converse: a URL that merely CONTAINS "data:" is a URL. A
	// substring test here would blank a legitimate remote reference.
	const u = "https://r.example/data:not-a-uri/a.png"
	if got := externalMedia("", u, ""); got.URL != u || got.Source != core.MediaExternal {
		t.Fatalf("a remote URL containing \"data:\" must survive intact: %+v", got)
	}
}

// Round 14. The unrecognised-element marker renders wire structure, not
// declared text, so it is bounded — the distinction the whole
// declared-text-is-verbatim rule turns on. A 40 KB element must not echo.
func TestUnrecognisedArrayElementMarkerIsBounded(t *testing.T) {
	huge := strings.Repeat("A", 40000)
	body := `{"output":["a",{"blob":"` + huge + `"},"c"]}`
	p, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var marker string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, huge[:200]) {
				t.Fatalf("the element's payload reached a text block")
			}
			if strings.Contains(b.Text, "unrecognised") {
				marker = b.Text
			}
		}
	}
	// Assert the marker EXISTS before sizing it. Both assertions used to
	// sit inside the Contains check, so deleting the marker entirely left
	// the test green — it could not tell "bounded" from "absent".
	if marker == "" {
		t.Fatal("no marker emitted; a dropped element must be named")
	}
	if len(marker) > 1100 {
		t.Fatalf("the marker echoed the element: %d chars", len(marker))
	}
}

// Round 15. Three discriminators in a row were walked past one equivalence
// class at a time - a bare prefix test, then the same after TrimSpace, then
// that after aligning the whitespace set. Unicode format characters are not
// White_Space, and `DATA:` is a legal spelling of the same URI under RFC
// 3986 section 3.1, not an attack.
//
// So this pins two different things: the classifier reads every legal
// spelling, so the ref keeps its mime and modality; and a length bound
// catches whatever the classifier does not recognise - the only line here
// that does not depend on recognising the value.
func TestReferenceFieldNeverHoldsAPayload(t *testing.T) {
	big := strings.Repeat("QUJD", 5000)

	for _, tc := range []struct{ name, value string }{
		{"plain", "data:image/png;base64," + big},
		{"leading space", " data:image/png;base64," + big},
		{"leading NEL", "\u0085data:image/png;base64," + big},
		{"leading BOM", "\ufeffdata:image/png;base64," + big},
		{"leading zero-width space", "\u200bdata:image/png;base64," + big},
		{"leading left-to-right mark", "\u200edata:image/png;base64," + big},
		{"uppercase scheme", "DATA:image/png;base64," + big},
		{"mixed-case scheme", "Data:image/png;base64," + big},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := externalMedia("", tc.value, "")
			if got.URL != "" {
				t.Fatalf("payload landed in the reference field: %d chars", len(got.URL))
			}
			// Recognised, not merely blocked: a ref that forgot what it was
			// looking at renders as a file with no type.
			if got.Mime != "image/png" || got.Modality != core.ModalityImage {
				t.Fatalf("the ref lost what it saw: mime=%q modality=%q", got.Mime, got.Modality)
			}
		})
	}

	// The backstop, for a value no classifier recognises at all. The full
	// contract for it lives in TestOversizedURLKeepsItsEvidence; what this
	// test cares about is only that the payload does not survive.
	over := externalMedia("", "https://r.example/"+strings.Repeat("a", 5000)+".png", "")
	if len(over.URL) > urlEvidenceBytes || !over.Truncated || over.Cause != "oversized-url" {
		t.Fatalf("an oversized value must be cut to evidence and named: %+v", over)
	}
	// And a real URL is untouched - the bound must not eat legitimate refs.
	const u = "https://r.example/a.png"
	if ok := externalMedia("", u, ""); ok.URL != u || ok.Source != core.MediaExternal {
		t.Fatalf("a normal URL must survive: %+v", ok)
	}
	// A signed URL near the practical browser limit is still a URL.
	long := "https://r.example/a.png?sig=" + strings.Repeat("s", 1800)
	if ok := externalMedia("", long, ""); ok.URL != long {
		t.Fatalf("a signed URL under the bound must survive: %d chars stored", len(ok.URL))
	}
}

// Round 15. JSON null unmarshals into a string without error, so it reached
// the empty-string skip and vanished - fusing the text either side into one
// utterance that was never sent. ["a",null,"c"] produced exactly the same
// payload as ["a","c"], with Confidence still reporting a good parse.
func TestReplicateNullElementIsNamedNotDropped(t *testing.T) {
	p, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(`{"output":["a",null,"c"]}`), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var texts []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) != 3 {
		t.Fatalf("want three blocks in wire order, got %d: %q", len(texts), texts)
	}
	if texts[0] != "a" || texts[2] != "c" {
		t.Fatalf("the null must not fuse its neighbours: %q", texts)
	}
	if !strings.Contains(texts[1], "null") {
		t.Fatalf("the null element must be named: %q", texts[1])
	}

	// An empty string is different: it contributes nothing and fuses
	// nothing, and a streamed fragment array legitimately carries them.
	p2, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(`{"output":["a","","c"]}`), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var got string
	for _, m := range p2.Messages {
		for _, b := range m.Content {
			got += b.Text
		}
	}
	if got != "ac" {
		t.Fatalf("empty fragments are not elements: %q", got)
	}
}

// Round 15. The marker names what it saw. Wrapping it in payloadSafeText as
// well as payloadSafeRaw elided the content at 96 characters, so a marker
// for an eight-key object said "[elided 202-char value]" - it reported that
// something was unrecognised while destroying the evidence of what.
func TestUnrecognisedMarkerShowsTheShapeItSaw(t *testing.T) {
	// Sixteen keys, MEASURED: at eight the rendered body is 82 bytes, under
	// payloadSafeText's 96-char cap, so re-wrapping the marker in it elided
	// nothing and this test could not tell the difference. At sixteen the
	// body is past the cap and the difference is visible.
	var parts []string
	for i := 1; i <= 16; i++ {
		parts = append(parts, fmt.Sprintf(`"k%02d":"v"`, i))
	}
	body := `{"output":["a",{` + strings.Join(parts, ",") + `},"c"]}`
	p, err := NewReplicateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var marker string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, "unrecognised") {
				marker = b.Text
			}
		}
	}
	if marker == "" {
		t.Fatal("no marker emitted")
	}
	if strings.Contains(marker, "elided") {
		t.Fatalf("the marker elided the shape it exists to report: %q", marker)
	}
	for _, k := range []string{"k01", "k16"} {
		if !strings.Contains(marker, k) {
			t.Fatalf("the marker must show the whole shape, %s missing: %q", k, marker)
		}
	}
	// Bounded all the same - this is rendered wire structure, not declared
	// text, so the verbatim rule does not reach it.
	if len(marker) > 1100 {
		t.Fatalf("marker unbounded: %d chars", len(marker))
	}
}

// Round 15. The scheme fold in parseDataURI is reachable only on the
// CAPTURED path - externalMedia recovers a mime hint with its own parsing,
// so every earlier case exercised that copy and left this one unpinned. A
// case-sensitive test here would hand a legal `DATA:` URI back to a caller
// that had already decided it was one, and the bytes would go unaddressed.
func TestUppercaseSchemeStillCapturesItsBytes(t *testing.T) {
	for _, scheme := range []string{"data:", "DATA:", "Data:"} {
		got := inlineOrExternal(scheme+"image/png;base64,iVBORw0KGgo=", "messages.0.content.0.image_url.url", core.ModalityImage)
		if got.Source != core.MediaCaptured {
			t.Fatalf("%s -> source %q, want captured: %+v", scheme, got.Source, got)
		}
		if got.SizeBytes != 8 || got.Locator == "" {
			t.Fatalf("%s -> bytes must be sized and addressable: %+v", scheme, got)
		}
		if got.Mime != "image/png" {
			t.Fatalf("%s -> mime %q, want image/png", scheme, got.Mime)
		}
	}
}

// Round 16. The class fix landed in media.go, and this codec never reaches
// it: replicateMediaElement pre-filters with its own normalisation, so both
// equivalence classes stayed open here after being closed at the class site.
// Worse than the reference-field case it mirrors — the fall-through here is
// the verbatim declared-text channel, unbounded by design, so the payload
// went whole into a block the compliance hooks scan.
//
// Entered at the WIRE, not at the helper. The round-15 tests called
// externalMedia and inlineOrExternal directly, which is why they proved the
// helpers read every spelling without proving the class was closed.
func TestReplicateWirePathReadsEverySpelling(t *testing.T) {
	big := strings.Repeat("QUJD", 5000)
	for _, tc := range []struct{ name, value string }{
		{"lowercase scheme", "data:image/png;base64," + big},
		{"uppercase scheme", "DATA:image/png;base64," + big},
		{"mixed-case scheme", "Data:image/png;base64," + big},
		{"leading BOM", "\ufeffdata:image/png;base64," + big},
		{"leading zero-width space", "\u200bdata:image/png;base64," + big},
		{"leading soft hyphen", "\u00addata:image/png;base64," + big},
		{"leading left-to-right embedding", "\u202adata:image/png;base64," + big},
		{"leading NEL", "\u0085data:image/png;base64," + big},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.value)
			p, err := NewReplicateNormalizer().Normalize(context.Background(),
				[]byte(`{"output":`+string(raw)+`}`), core.Meta{Direction: core.DirectionResponse})
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			var media []*core.MediaRef
			for _, m := range p.Messages {
				for _, b := range m.Content {
					if b.Type == core.ContentText {
						t.Fatalf("payload reached the verbatim text channel: %d bytes", len(b.Text))
					}
					if b.MediaRef != nil {
						media = append(media, b.MediaRef)
					}
				}
			}
			if len(media) != 1 {
				t.Fatalf("want one media block, got %d", len(media))
			}
			if media[0].Source != core.MediaCaptured || media[0].SizeBytes != 15000 {
				t.Fatalf("bytes must be captured and sized: %+v", media[0])
			}
			if media[0].Mime != "image/png" {
				t.Fatalf("the ref lost what it saw: %+v", media[0])
			}
		})
	}

	// A remote URL spelled with an uppercase scheme is still a URL, not prose.
	raw, _ := json.Marshal("HTTPS://r.example/a.png")
	p, err := NewReplicateNormalizer().Normalize(context.Background(),
		[]byte(`{"output":`+string(raw)+`}`), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Source != core.MediaExternal || ref.Mime != "image/png" {
		t.Fatalf("an uppercase https scheme is still an external image: %+v", ref)
	}
}

// Round 16. An oversized value is truncated with its cause named, not
// blanked. Blanking made it indistinguishable from "the provider sent no
// media" and destroyed the only thing worth keeping — which host was
// referenced, which is the evidence a compliance reader needs.
func TestOversizedURLKeepsItsEvidence(t *testing.T) {
	const host = "https://evil.example/"
	got := externalMedia("", host+strings.Repeat("a", 5000)+".png", "")
	if got.Source != core.MediaExternal || !got.Truncated || got.Cause != "oversized-url" {
		t.Fatalf("an oversized value must be recorded and named: %+v", got)
	}
	if !strings.HasPrefix(got.URL, host) {
		t.Fatalf("the referenced host must survive: %.60q", got.URL)
	}
	if len(got.URL) != urlEvidenceBytes {
		t.Fatalf("evidence length = %d, want %d", len(got.URL), urlEvidenceBytes)
	}
	// Truncated, so no control is offered for it — the same contract
	// capturedMedia relies on for undecodable bytes.
	if got.SizeBytes != 0 {
		t.Fatalf("a truncated reference claims no bytes: %+v", got)
	}

	// The boundary itself, both sides, so the constant is not free to drift.
	pad := func(n int) string { return "https://r.example/" + strings.Repeat("a", n-18) }
	if at := externalMedia("", pad(maxURLBytes), ""); at.Truncated || len(at.URL) != maxURLBytes {
		t.Fatalf("a value of exactly maxURLBytes must be kept whole: trunc=%v len=%d", at.Truncated, len(at.URL))
	}
	if over := externalMedia("", pad(maxURLBytes+1), ""); !over.Truncated {
		t.Fatalf("one byte past the bound must truncate")
	}

	// A signed URL near the practical browser limit is under the bound.
	long := "https://r.example/a.png?sig=" + strings.Repeat("s", 1800)
	if ok := externalMedia("", long, ""); ok.Truncated || ok.URL != long {
		t.Fatalf("a signed URL must survive whole: trunc=%v len=%d", ok.Truncated, len(ok.URL))
	}
}

// Round 16. A url-typed source carrying no url used to become an external
// card referencing nothing. Both sibling builders already degrade to absent.
func TestEmptyURLDegradesToAbsent(t *testing.T) {
	got := externalMedia("image/png", "   ", core.ModalityImage)
	if got.Source != core.MediaAbsent {
		t.Fatalf("a reference to nothing is absent, not external: %+v", got)
	}
	if got.URL != "" {
		t.Fatalf("nothing to record: %+v", got)
	}

	// Through the wire shape that produces it.
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"url","media_type":"image/png"}}]}]}`
	p, err := NewAnthropicMessagesNormalizer().Normalize(context.Background(),
		[]byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Source != core.MediaAbsent {
		t.Fatalf("a url source with no url is absent: %+v", ref)
	}
}
