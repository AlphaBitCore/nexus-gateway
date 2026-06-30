package canonicalbridge

// stream_encoders_openai_types.go — wire-shape typed structs for the OpenAI
// chat.completion.chunk stream encoder, split from stream_encoders.go to keep
// that file under the size ratchet.
//
// Field order is deliberately ALPHABETICAL to mirror go-json's map-key sorting,
// so the struct encoder emits byte-identical output to the prior map[string]any
// path (zero wire change). Field order is NOT an external contract — it is
// pinned only to keep the wire bytes stable for clients, response caches, and
// provider prefix-caches. go-json applies the same HTML-safe string escape to
// struct fields as it did to map values.

type oaiStreamEnvelope struct {
	Choices []oaiStreamChoice `json:"choices"`
	Created int64             `json:"created"`
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Object  string            `json:"object"`
	Usage   *oaiStreamUsage   `json:"usage,omitempty"`
}

type oaiStreamChoice struct {
	Delta oaiStreamDelta `json:"delta"`
	// FinishReason renders `null` on delta frames (nil pointer) and the value on
	// the terminal frame; NO omitempty so the explicit null is preserved.
	FinishReason *string `json:"finish_reason"`
	Index        int     `json:"index"`
}

// oaiStreamDelta covers every delta shape. Content is *string so a nil pointer
// is OMITTED (tool / reasoning / done frames) while a non-nil empty pointer
// renders the explicit `"content":""` the role-header frame requires.
type oaiStreamDelta struct {
	Content          *string       `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Role             string        `json:"role,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiToolCall struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function oaiToolFunc `json:"function"`
}

type oaiToolFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// oaiStreamUsage uses *int per token field so a non-nil zero still renders
// (matching the prior `if ptr != nil` map logic); detail sub-blocks are pointers
// with omitempty so they appear only when their source is set AND > 0.
type oaiStreamUsage struct {
	CompletionTokens        *int                        `json:"completion_tokens,omitempty"`
	CompletionTokensDetails *oaiCompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	PromptTokens            *int                        `json:"prompt_tokens,omitempty"`
	PromptTokensDetails     *oaiPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	TotalTokens             *int                        `json:"total_tokens,omitempty"`
}

type oaiPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type oaiCompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}
