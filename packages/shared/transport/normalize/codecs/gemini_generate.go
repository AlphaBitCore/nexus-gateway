package codecs

import (
	"context"
	"fmt"
	"strings"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// GeminiGenerateNormalizer handles Google's native generateContent surface
// (`/v1beta/models/{model}:generateContent` and `:streamGenerateContent`)
// for both Google AI Studio (Gemini API) and Vertex AI deployments.
//
// Shape mapping into the canonical NormalizedPayload:
//
//   - request.contents[].parts[] → Message.Content[]; role "model" maps
//     to core.RoleAssistant, role "user" stays as core.RoleUser.
//   - request.systemInstruction.parts[] is flattened into a synthetic
//     system message at position [0] so downstream hooks see a uniform list.
//   - parts[].thought=true marks a text part as reasoning — preserved as
//     core.ContentReasoning (do not drop thinking).
//   - parts[].inlineData (base64 image / audio / video / pdf) becomes a
//     media block whose modality comes from the declared mimeType.
//   - parts[].functionCall / functionResponse become core.ContentToolUse /
//     core.ContentToolResult.
//   - response.candidates[].content.parts[] reuses the same decoder, and the
//     stream fold accumulates blocks in arrival order, coalescing only
//     CONSECUTIVE text deltas. So the same wire content yields the same
//     blocks in the same order whether or not it streamed — including the
//     case where the model speaks, calls a tool, and speaks again, which
//     per-kind grouping used to fuse into one utterance.
//   - usageMetadata.{promptTokenCount, candidatesTokenCount,
//     totalTokenCount, cachedContentTokenCount} → Usage.{Prompt,
//     Completion, Total, CacheReadTokens}.
type GeminiGenerateNormalizer struct{}

// NewGeminiGenerateNormalizer returns a stateless normalizer instance.
func NewGeminiGenerateNormalizer() *GeminiGenerateNormalizer {
	return &GeminiGenerateNormalizer{}
}

// ID is the metric / log label.
func (n *GeminiGenerateNormalizer) ID() string { return "gemini-generate" }

// Normalize routes by Meta.Direction.
func (n *GeminiGenerateNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroGemini(meta), fmt.Errorf("gemini-generate: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroGemini(meta), fmt.Errorf("gemini-generate: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	// Confidence semantics (one meaning per input shape): a stream fold
	// computes frame coverage (recognized / total data frames) and sets
	// Confidence itself; single-document bodies score weighted field
	// coverage using the gemini-generate FieldSpec — see confidence.go.
	// Gemini's response shape carries `candidates` + `usageMetadata` at
	// the root; the FieldSpec captures both required keys plus the
	// (numerous) Gemini-specific cosmetic fields like `modelVersion`,
	// `responseId`, `promptFeedback` so they don't trip the
	// unknown-field penalty.
	if err == nil {
		if p.Confidence == 0 {
			p.Confidence = core.ScoreTier1Confidence(raw, geminiGenerateFieldSpec(meta.Direction))
		}
		if p.DetectedSpec == "" {
			p.DetectedSpec = "gemini-generate"
		}
	}
	return p, err
}

// geminiGenerateFieldSpec returns the declared top-level wire keys for
// Google's generateContent / streamGenerateContent surface in direction d.
func geminiGenerateFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"contents"},
			Optional: []string{
				"model", "systemInstruction", "tools", "toolConfig",
				"generationConfig", "safetySettings", "cachedContent",
				"labels",
			},
		}
	}
	return core.FieldSpec{
		Required: []string{"candidates", "usageMetadata"},
		Optional: []string{
			"modelVersion", "responseId", "promptFeedback", "createTime",
		},
	}
}

type geminiRequest struct {
	Model             string          `json:"model,omitempty"`
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	// Google's JSON mapping accepts the proto field name too, and the
	// compliance adapter already reads both spellings. Without this a
	// snake_case caller's system prompt is scanned but never normalised, so
	// the audit row shows a conversation missing the instruction that shaped
	// it. Resolved in favour of the camelCase field when both are present.
	SystemInstructionSnake *geminiContent    `json:"system_instruction,omitempty"`
	Tools                  []geminiToolGroup `json:"tools,omitempty"`
	GenerationConfig       *geminiGenConfig  `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             *string                 `json:"text,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	// ThoughtSignature is attached to the exact Part-level functionCall by
	// Gemini and must remain paired with that canonical tool use.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	// Thought marks a text part as the model's reasoning (Gemini 2.x+
	// extended-thinking surface). When true we project to core.ContentReasoning.
	Thought bool `json:"thought,omitempty"`

	// unmatched holds the original bytes of a part that decoded to nothing
	// this struct models — executableCode, codeExecutionResult, a future
	// variant. It exists so an unrecognised part becomes a visible marker
	// instead of vanishing, which is how gemini media was silently lost.
	// Only an unmatched part pays for it (see UnmarshalJSON).
	unmatched json.RawMessage
}

type geminiInlineData struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// geminiFileData is Google's reference to media held in the Files API, and
// the mechanism its docs require above 100 MB. Without this field such a
// part matched no case and produced no audit trace at all.
type geminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri,omitempty"`
}

type geminiFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

type geminiToolGroup struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations,omitempty"`
}

type geminiFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	// ThinkingConfig is Gemini's BUDGET-plus-VISIBILITY spelling of the
	// reasoning request. `thinkingBudget` of -1 means DYNAMIC — let the model
	// decide — which neither other vendor has a value for.
	ThinkingConfig *geminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

type geminiThinkingConfig struct {
	ThinkingBudget  *int  `json:"thinkingBudget,omitempty"`
	IncludeThoughts *bool `json:"includeThoughts,omitempty"`
}

func (n *GeminiGenerateNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req geminiRequest
	if err := decodeLenient(raw, &req); err != nil {
		return zeroGemini(meta), fmt.Errorf("gemini-generate: request unmarshal: %w", err)
	}
	if len(req.Contents) == 0 {
		// generateContent without contents[] is not a valid Gemini request.
		return zeroGemini(meta), fmt.Errorf("gemini-generate: missing contents[]: %w", core.ErrUnsupported)
	}

	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-generate",
		Model:            firstNonEmpty(req.Model, meta.Model),
		Stream:           meta.Stream,
	}

	sysInstruction, sysPath := req.SystemInstruction, "systemInstruction.parts"
	if sysInstruction == nil && req.SystemInstructionSnake != nil {
		sysInstruction, sysPath = req.SystemInstructionSnake, "system_instruction.parts"
	}
	if sysInstruction != nil {
		blocks := geminiPartsToBlocks(sysInstruction.Parts, sysPath)
		if len(blocks) > 0 {
			out.Messages = append(out.Messages, core.Message{Role: core.RoleSystem, Content: blocks})
		}
	}

	for i, c := range req.Contents {
		blocks := geminiPartsToBlocks(c.Parts, locator.JoinPath("contents", i)+".parts")
		out.Messages = append(out.Messages, core.Message{Role: geminiRoleToCanonical(c.Role), Content: blocks})
	}

	if len(req.Tools) > 0 {
		var tools []core.ToolDef
		for _, group := range req.Tools {
			for _, fd := range group.FunctionDeclarations {
				td := core.ToolDef{Name: fd.Name, Description: fd.Description}
				if len(fd.Parameters) > 0 {
					var p map[string]any
					if err := json.Unmarshal(fd.Parameters, &p); err == nil {
						td.ParametersJSONSchema = p
					}
				}
				tools = append(tools, td)
			}
		}
		out.Tools = tools
	}

	if g := req.GenerationConfig; g != nil {
		params := &core.SamplingParam{
			Temperature: g.Temperature,
			TopP:        g.TopP,
			TopK:        g.TopK,
			MaxTokens:   g.MaxOutputTokens,
			Stop:        g.StopSequences,
		}
		// Recorded as the caller wrote it, -1 included: a budget of -1 is
		// Gemini saying "you decide", which is a real expression and not an
		// absent one. Translating it for a wire that has no such value is that
		// wire's codec's problem, not this decoder's.
		if tc := g.ThinkingConfig; tc != nil {
			r := &core.Reasoning{BudgetTokens: tc.ThinkingBudget, IncludeThoughts: tc.IncludeThoughts}
			if r.Asked() {
				params.Reasoning = r
			}
		}
		if params.Temperature != nil || params.TopP != nil || params.TopK != nil ||
			params.MaxTokens != nil || len(params.Stop) > 0 || params.Reasoning.Asked() {
			out.Params = params
		}
	}

	return out, nil
}

// geminiRoleToCanonical maps Gemini's role vocabulary (user / model /
// function) to the canonical Role enum. Empty role defaults to user
// (Gemini sometimes omits the role on the first turn).
func geminiRoleToCanonical(r string) core.Role {
	switch strings.ToLower(r) {
	case "model":
		return core.RoleAssistant
	case "user", "":
		return core.RoleUser
	case "function":
		return core.RoleTool
	case "system":
		return core.RoleSystem
	default:
		return core.Role(r)
	}
}

// Non-streaming response

type geminiResponse struct {
	ModelVersion  string            `json:"modelVersion,omitempty"`
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      *geminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index,omitempty"`
}

type geminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	// ThoughtsTokenCount is the Gemini 2.x extended-thinking surface
	// equivalent of OpenAI's reasoning_tokens. Counted into the total
	// but not surfaced as visible content.
	ThoughtsTokenCount int `json:"thoughtsTokenCount,omitempty"`
}

func (n *GeminiGenerateNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if meta.Stream || looksLikeGeminiEventStream(raw) {
		return n.normalizeStreamResponse(raw, meta)
	}
	var resp geminiResponse
	if err := decodeLenient(raw, &resp); err != nil {
		return zeroGemini(meta), fmt.Errorf("gemini-generate: response unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-generate",
		Model:            firstNonEmpty(resp.ModelVersion, meta.Model),
	}
	if len(resp.Candidates) == 0 {
		return out, fmt.Errorf("gemini-generate: no candidates in response: %w", core.ErrUnsupported)
	}
	// A body can carry a "candidates" array yet not be Gemini at all
	// (any JSON API returning a list of options unmarshals into the
	// struct with every Gemini field zero). When NO candidate carries
	// content AND usageMetadata is absent there is nothing Gemini-shaped
	// to project — decline with ErrUnsupported so the registry walk
	// continues, instead of claiming an empty ai-chat payload whose
	// required-fields-present score can sit exactly at the 0.70
	// threshold. A genuine empty Gemini response (safety block,
	// stream-final accounting chunk) always carries usageMetadata, so
	// this costs no recall.
	hasContent := false
	for _, c := range resp.Candidates {
		if c.Content != nil {
			hasContent = true
			break
		}
	}
	if !hasContent && resp.UsageMetadata == nil {
		return zeroGemini(meta), fmt.Errorf("gemini-generate: candidates carry no content and usageMetadata absent: %w", core.ErrUnsupported)
	}
	for ci, c := range resp.Candidates {
		if c.Content == nil {
			continue
		}
		// Gemini response candidates often omit content.role (the API
		// silently means "model" — i.e. the assistant's reply). The
		// generic geminiRoleToCanonical defaults empty role to core.RoleUser
		// because that's the right default for REQUEST contents (the
		// first turn often has no role). On the RESPONSE side an empty
		// role is unambiguously assistant — anything else would
		// mis-route into the wrong message slot for downstream consumers
		// (the OpenAI projector filters by core.RoleAssistant; an audit
		// reader rendering by role would mislabel the model's reply).
		role := geminiRoleToCanonical(c.Content.Role)
		if c.Content.Role == "" {
			role = core.RoleAssistant
		}
		msg := core.Message{
			Role:         role,
			Content:      geminiPartsToBlocks(c.Content.Parts, locator.JoinPath("candidates", ci)+".content.parts"),
			FinishReason: c.FinishReason,
		}
		out.Messages = append(out.Messages, msg)
	}
	out.FinishReason = resp.Candidates[0].FinishReason
	if resp.UsageMetadata != nil {
		out.Usage = geminiUsageToCanonical(resp.UsageMetadata)
	}
	return out, nil
}

// ExtractGeminiEventUsage parses a Gemini streaming chunk's raw bytes
// and returns the canonical Usage from its usageMetadata block. Used
// by ai-gateway's spec_gemini streaming session so per-
// chunk core.Usage extraction goes through the same alias chain as the
// non-streaming path. Returns nil when no usageMetadata is present.
func ExtractGeminiEventUsage(chunkJSON []byte) *core.Usage {
	var chunk geminiResponse
	if err := json.Unmarshal(chunkJSON, &chunk); err != nil {
		return nil
	}
	if chunk.UsageMetadata == nil {
		return nil
	}
	return geminiUsageToCanonical(chunk.UsageMetadata)
}

func geminiUsageToCanonical(u *geminiUsage) *core.Usage {
	out := &core.Usage{}
	setIntPtr(&out.PromptTokens, u.PromptTokenCount)
	// CompletionTokens follows the OpenAI canonical convention of
	// including reasoning tokens. Gemini reports them disjoint:
	// candidatesTokenCount is visible output, thoughtsTokenCount is
	// internal thinking. Sum here so callers don't need a per-provider
	// branch.
	completion := u.CandidatesTokenCount + u.ThoughtsTokenCount
	setIntPtr(&out.CompletionTokens, completion)
	setIntPtr(&out.TotalTokens, u.TotalTokenCount)
	setIntPtr(&out.CacheReadTokens, u.CachedContentTokenCount)
	setIntPtr(&out.ReasoningTokens, u.ThoughtsTokenCount)
	return out
}

func zeroGemini(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-generate",
		Model:            meta.Model,
	}
}

// looksLikeGeminiEventStream sniffs for the SSE `data:` prefix the
// streamGenerateContent endpoint emits. Helps when cp/agent strip the
// Content-Type before audit.
func looksLikeGeminiEventStream(raw []byte) bool {
	probe := raw
	if len(probe) > 64 {
		probe = probe[:64]
	}
	s := strings.TrimLeft(string(probe), " \r\n\t")
	return strings.HasPrefix(s, "data:")
}
