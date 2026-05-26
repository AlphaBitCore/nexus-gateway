package wireformat

import (
	"errors"
	"strings"
	"testing"
)

// These tests target the error-return branches of every validator in this
// package. They assert observable behavior: (a) the returned error wraps the
// documented sentinel via errors.Is, and (b) the error message names the
// missing/invalid field. The happy-path matrix lives in
// official_wire_contract_test.go.

func TestValidateOpenAIChatCompletionChunk_NegativeBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{
			name:       "empty json",
			body:       ``,
			wantSubstr: "empty json",
		},
		{
			name:       "wrong object literal",
			body:       `{"object":"chat.completion","id":"x","created":1,"model":"gpt","choices":[]}`,
			wantSubstr: `object=`,
		},
		{
			name:       "missing id",
			body:       `{"object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{}}]}`,
			wantSubstr: "missing id",
		},
		{
			name:       "empty id string",
			body:       `{"object":"chat.completion.chunk","id":"","created":1,"model":"gpt","choices":[{"index":0,"delta":{}}]}`,
			wantSubstr: "missing id",
		},
		{
			name:       "missing created",
			body:       `{"object":"chat.completion.chunk","id":"x","model":"gpt","choices":[{"index":0,"delta":{}}]}`,
			wantSubstr: "missing created",
		},
		{
			name:       "missing model",
			body:       `{"object":"chat.completion.chunk","id":"x","created":1,"choices":[{"index":0,"delta":{}}]}`,
			wantSubstr: "missing model",
		},
		{
			name:       "empty model string",
			body:       `{"object":"chat.completion.chunk","id":"x","created":1,"model":"","choices":[{"index":0,"delta":{}}]}`,
			wantSubstr: "missing model",
		},
		{
			name:       "missing choices",
			body:       `{"object":"chat.completion.chunk","id":"x","created":1,"model":"gpt"}`,
			wantSubstr: "missing choices",
		},
		{
			name:       "empty choices without usage",
			body:       `{"object":"chat.completion.chunk","id":"x","created":1,"model":"gpt","choices":[]}`,
			wantSubstr: "empty choices without usage",
		},
		{
			name:       "choices missing index",
			body:       `{"object":"chat.completion.chunk","id":"x","created":1,"model":"gpt","choices":[{"delta":{}}]}`,
			wantSubstr: "choices[0] missing index",
		},
		{
			name:       "choices missing delta",
			body:       `{"object":"chat.completion.chunk","id":"x","created":1,"model":"gpt","choices":[{"index":0}]}`,
			wantSubstr: "choices[0] missing delta",
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOpenAIChatCompletionChunk([]byte(tc.body))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSubstr)
			}
			if !errors.Is(err, ErrOpenAIChunkShape) {
				t.Fatalf("err=%v does not wrap ErrOpenAIChunkShape", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err=%q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestValidateOpenAIChatCompletionChunk_HappyEdges(t *testing.T) {
	t.Parallel()
	// Empty choices array WITH usage is the documented stream_options usage-chunk
	// terminator (OpenAI streaming events reference). Must succeed.
	usageOnly := `{"object":"chat.completion.chunk","id":"x","created":1,"model":"gpt","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	if err := ValidateOpenAIChatCompletionChunk([]byte(usageOnly)); err != nil {
		t.Fatalf("usage-only chunk should validate: %v", err)
	}
}

func TestIsOpenAIStreamDone_Matrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "exact", data: "[DONE]", want: true},
		{name: "trim space", data: "  [DONE]  ", want: true},
		{name: "trim newline", data: "[DONE]\n", want: true},
		{name: "trim crlf", data: "\r\n[DONE]\r\n", want: true},
		{name: "lowercase rejected", data: "[done]", want: false},
		{name: "empty", data: "", want: false},
		{name: "json", data: `{"foo":1}`, want: false},
		{name: "almost", data: "[DONE", want: false},
		{name: "with extras", data: "[DONE] x", want: false},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := IsOpenAIStreamDone([]byte(tc.data))
			if got != tc.want {
				t.Fatalf("IsOpenAIStreamDone(%q)=%v want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestValidateOpenAIChatCompletionRequest_NegativeBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{name: "empty json", body: ``, wantSubstr: "empty json"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hi"}]}`, wantSubstr: "missing model"},
		{name: "empty model", body: `{"model":"","messages":[{"role":"user","content":"hi"}]}`, wantSubstr: "missing model"},
		{name: "missing messages", body: `{"model":"gpt"}`, wantSubstr: "messages must be a non-empty array"},
		{name: "messages not array", body: `{"model":"gpt","messages":{"role":"user"}}`, wantSubstr: "messages must be a non-empty array"},
		{name: "empty messages array", body: `{"model":"gpt","messages":[]}`, wantSubstr: "messages must be a non-empty array"},
		{name: "tools not array", body: `{"model":"gpt","messages":[{"role":"user"}],"tools":{"type":"function"}}`, wantSubstr: "tools must be an array"},
		{name: "response_format not object", body: `{"model":"gpt","messages":[{"role":"user"}],"response_format":"json_object"}`, wantSubstr: "response_format must be an object"},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOpenAIChatCompletionRequest([]byte(tc.body))
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.wantSubstr)
			}
			if !errors.Is(err, ErrOpenAIRequestShape) {
				t.Fatalf("err=%v does not wrap ErrOpenAIRequestShape", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err=%q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestValidateOpenAIChatCompletionRequest_OptionalOmitted(t *testing.T) {
	t.Parallel()
	// tools and response_format absent → must validate cleanly (covers the
	// optional-branch fall-throughs of `.Exists()` guards).
	req := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	if err := ValidateOpenAIChatCompletionRequest([]byte(req)); err != nil {
		t.Fatalf("minimal valid request rejected: %v", err)
	}
}

func TestValidateAnthropicMessagesRequest_NegativeBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{name: "empty json", body: ``, wantSubstr: "empty json"},
		{name: "missing model", body: `{"max_tokens":1,"messages":[{"role":"user"}]}`, wantSubstr: "missing model"},
		{name: "empty model", body: `{"model":"","max_tokens":1,"messages":[{"role":"user"}]}`, wantSubstr: "missing model"},
		{name: "missing max_tokens", body: `{"model":"claude","messages":[{"role":"user"}]}`, wantSubstr: "missing max_tokens"},
		{name: "missing messages", body: `{"model":"claude","max_tokens":1}`, wantSubstr: "messages must be a non-empty array"},
		{name: "messages not array", body: `{"model":"claude","max_tokens":1,"messages":{"role":"user"}}`, wantSubstr: "messages must be a non-empty array"},
		{name: "empty messages array", body: `{"model":"claude","max_tokens":1,"messages":[]}`, wantSubstr: "messages must be a non-empty array"},
		{name: "metadata not object", body: `{"model":"claude","max_tokens":1,"messages":[{"role":"user"}],"metadata":"abc"}`, wantSubstr: "metadata must be an object"},
		{name: "stop_sequences not array", body: `{"model":"claude","max_tokens":1,"messages":[{"role":"user"}],"stop_sequences":"</end>"}`, wantSubstr: "stop_sequences must be an array"},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAnthropicMessagesRequest([]byte(tc.body))
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.wantSubstr)
			}
			if !errors.Is(err, ErrAnthropicRequestShape) {
				t.Fatalf("err=%v does not wrap ErrAnthropicRequestShape", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err=%q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestValidateAnthropicMessagesRequest_OptionalOmitted(t *testing.T) {
	t.Parallel()
	// metadata and stop_sequences absent → must validate.
	req := `{"model":"claude-3-5-sonnet","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`
	if err := ValidateAnthropicMessagesRequest([]byte(req)); err != nil {
		t.Fatalf("minimal request rejected: %v", err)
	}
}

func TestValidateAnthropicStreamingJSON_NegativeBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{name: "empty json", body: ``, wantSubstr: "empty json"},
		{name: "missing type", body: `{}`, wantSubstr: "missing type"},

		// message_start branches
		{name: "message_start missing message", body: `{"type":"message_start"}`, wantSubstr: "message_start missing message"},
		{name: "message_start missing id", body: `{"type":"message_start","message":{"type":"message","role":"assistant","usage":{}}}`, wantSubstr: "message_start.message.id required"},
		{name: "message_start empty id", body: `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","usage":{}}}`, wantSubstr: "message_start.message.id required"},
		{name: "message_start wrong type", body: `{"type":"message_start","message":{"id":"m","type":"foo","role":"assistant","usage":{}}}`, wantSubstr: "message_start.message.type"},
		{name: "message_start missing nested type", body: `{"type":"message_start","message":{"id":"m","role":"assistant","usage":{}}}`, wantSubstr: "message_start.message.type"},
		{name: "message_start wrong role", body: `{"type":"message_start","message":{"id":"m","type":"message","role":"user","usage":{}}}`, wantSubstr: "message_start.message.role"},
		{name: "message_start missing role", body: `{"type":"message_start","message":{"id":"m","type":"message","usage":{}}}`, wantSubstr: "message_start.message.role"},
		{name: "message_start missing usage", body: `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant"}}`, wantSubstr: "message_start.message.usage required"},

		// content_block_start branches
		{name: "content_block_start missing index", body: `{"type":"content_block_start","content_block":{"type":"text"}}`, wantSubstr: "content_block_start missing index"},
		{name: "content_block_start missing block", body: `{"type":"content_block_start","index":0}`, wantSubstr: "content_block_start missing content_block"},
		{name: "content_block_start empty type", body: `{"type":"content_block_start","index":0,"content_block":{"text":""}}`, wantSubstr: "content_block.type required"},

		// content_block_delta branches
		{name: "content_block_delta missing index", body: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, wantSubstr: "content_block_delta missing index"},
		{name: "content_block_delta missing delta", body: `{"type":"content_block_delta","index":0}`, wantSubstr: "content_block_delta missing delta"},
		{name: "content_block_delta empty delta type", body: `{"type":"content_block_delta","index":0,"delta":{"text":"hi"}}`, wantSubstr: "content_block_delta.delta.type required"},

		// content_block_stop
		{name: "content_block_stop missing index", body: `{"type":"content_block_stop"}`, wantSubstr: "content_block_stop missing index"},

		// message_delta
		{name: "message_delta missing delta", body: `{"type":"message_delta"}`, wantSubstr: "message_delta missing delta"},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAnthropicStreamingJSON([]byte(tc.body))
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.wantSubstr)
			}
			if !errors.Is(err, ErrAnthropicSSEShape) {
				t.Fatalf("err=%v does not wrap ErrAnthropicSSEShape", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err=%q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestValidateAnthropicStreamingJSON_HappyEdges(t *testing.T) {
	t.Parallel()
	// Each branch's terminal success path. message_stop / ping carry no extra
	// required fields. An unknown event type must be accepted (forward-compat).
	tests := []string{
		`{"type":"message_stop"}`,
		`{"type":"ping"}`,
		`{"type":"some_future_event_kind","arbitrary":42}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	}
	for _, body := range tests {

		t.Run(body, func(t *testing.T) {
			t.Parallel()
			if err := ValidateAnthropicStreamingJSON([]byte(body)); err != nil {
				t.Fatalf("body %q rejected: %v", body, err)
			}
		})
	}
}

func TestValidateGeminiGenerateContentResponseChunk_NegativeBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{name: "empty json", body: ``, wantSubstr: "empty json"},
		{name: "candidate missing content", body: `{"candidates":[{"index":0,"finishReason":"STOP"}]}`, wantSubstr: "candidates[0] missing content"},
		{name: "candidate missing parts", body: `{"candidates":[{"index":0,"content":{"role":"model"}}]}`, wantSubstr: "candidates[0] missing content.parts"},
		{name: "candidate empty parts", body: `{"candidates":[{"index":0,"content":{"role":"model","parts":[]}}]}`, wantSubstr: "candidates[0] missing content.parts"},
		{name: "second candidate broken", body: `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]}},{"index":1,"content":{"role":"model"}}]}`, wantSubstr: "candidates[1] missing content.parts"},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateGeminiGenerateContentResponseChunk([]byte(tc.body))
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.wantSubstr)
			}
			if !errors.Is(err, ErrGeminiChunkShape) {
				t.Fatalf("err=%v does not wrap ErrGeminiChunkShape", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err=%q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestValidateGeminiGenerateContentResponseChunk_HappyEdges(t *testing.T) {
	t.Parallel()
	// usageMetadata-only trailer chunk: no candidates key → must accept.
	usageOnly := `{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}`
	if err := ValidateGeminiGenerateContentResponseChunk([]byte(usageOnly)); err != nil {
		t.Fatalf("usage-only chunk rejected: %v", err)
	}
	// empty candidates array (zero-length, key present): also accepted by the
	// documented model (no candidates to validate).
	emptyCandidates := `{"candidates":[]}`
	if err := ValidateGeminiGenerateContentResponseChunk([]byte(emptyCandidates)); err != nil {
		t.Fatalf("empty candidates rejected: %v", err)
	}
}

func TestValidateGeminiGenerateContentRequest_NegativeBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{name: "empty json", body: ``, wantSubstr: "empty json"},
		{name: "missing contents", body: `{}`, wantSubstr: "contents must be a non-empty array"},
		{name: "contents not array", body: `{"contents":{"role":"user"}}`, wantSubstr: "contents must be a non-empty array"},
		{name: "empty contents array", body: `{"contents":[]}`, wantSubstr: "contents must be a non-empty array"},
		{name: "generationConfig not object", body: `{"contents":[{"role":"user"}],"generationConfig":"hi"}`, wantSubstr: "generationConfig must be an object"},
		{name: "safetySettings not array", body: `{"contents":[{"role":"user"}],"safetySettings":{"category":"x"}}`, wantSubstr: "safetySettings must be an array"},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateGeminiGenerateContentRequest([]byte(tc.body))
			if err == nil {
				t.Fatalf("want error %q, got nil", tc.wantSubstr)
			}
			if !errors.Is(err, ErrGeminiRequestShape) {
				t.Fatalf("err=%v does not wrap ErrGeminiRequestShape", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err=%q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestValidateGeminiGenerateContentRequest_OptionalOmitted(t *testing.T) {
	t.Parallel()
	req := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	if err := ValidateGeminiGenerateContentRequest([]byte(req)); err != nil {
		t.Fatalf("minimal request rejected: %v", err)
	}
}
