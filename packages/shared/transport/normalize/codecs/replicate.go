package codecs

import (
	"context"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/goccy/go-json"
	"strings"
)

// ReplicateNormalizer handles Replicate's prediction-result response
// shape returned by POST /v1/predictions (and the GET poll path).
//
// Response shape (relevant subset; full schema at
// https://replicate.com/docs/reference/http#predictions):
//
//	{
//	  "id": "...",
//	  "version": "...",
//	  "status": "succeeded" | "failed" | "canceled" | ...,
//	  "output": "..." | ["..."] | {"text": "..."} | {"answer": "..."}
//	             | {"completion": "..."} | {"message": "..."},
//	  "error": "..." (when status != succeeded),
//	  "created_at": "RFC3339",
//	  "metrics": {
//	    "input_token_count":  N,
//	    "output_token_count": N
//	  }
//	}
//
// Usage extraction (OpenAI-aligned):
//   - PromptTokens     ← metrics.input_token_count
//   - CompletionTokens ← metrics.output_token_count
//   - TotalTokens      ← PromptTokens + CompletionTokens
//
// Replicate has no cache or reasoning telemetry; the corresponding
// Usage fields stay nil.
type ReplicateNormalizer struct{}

// NewReplicateNormalizer returns a stateless normalizer instance.
func NewReplicateNormalizer() *ReplicateNormalizer { return &ReplicateNormalizer{} }

// ID is the metric / log label.
func (n *ReplicateNormalizer) ID() string { return "replicate-prediction" }

// Normalize routes by Meta.Direction.
func (n *ReplicateNormalizer) Normalize(_ context.Context, raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	if len(raw) == 0 {
		return zeroReplicate(meta), fmt.Errorf("replicate: empty body: %w", core.ErrUnsupported)
	}
	var p core.NormalizedPayload
	var err error
	switch meta.Direction {
	case core.DirectionRequest:
		p, err = n.normalizeRequest(raw, meta)
	case core.DirectionResponse:
		p, err = n.normalizeResponse(raw, meta)
	default:
		return zeroReplicate(meta), fmt.Errorf("replicate: direction %q not supported: %w", meta.Direction, core.ErrUnsupported)
	}
	if err == nil {
		p.Confidence = core.ScoreTier1Confidence(raw, replicateFieldSpec(meta.Direction))
		if p.DetectedSpec == "" {
			p.DetectedSpec = "replicate-prediction"
		}
	}
	return p, err
}

// replicateFieldSpec returns the declared top-level wire keys for the
// Replicate /v1/predictions surface in direction d.
func replicateFieldSpec(d core.Direction) core.FieldSpec {
	if d == core.DirectionRequest {
		return core.FieldSpec{
			Required: []string{"input"},
			Optional: []string{
				"version", "model", "stream", "webhook", "webhook_events_filter",
			},
		}
	}
	return core.FieldSpec{
		Required: []string{"id", "status", "output"},
		Optional: []string{
			"version", "model", "created_at", "started_at", "completed_at",
			"metrics", "error", "logs", "urls", "input",
		},
	}
}

// Replicate request body: { "version": "...", "stream": bool, "input": {...} }
// We treat input.prompt / input.messages as the user content for
// canonical purposes.
type replicateRequest struct {
	Version string          `json:"version"`
	Stream  bool            `json:"stream,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
}

type replicateInput struct {
	Prompt   string             `json:"prompt,omitempty"`
	Messages []replicateMessage `json:"messages,omitempty"`
}

type replicateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (n *ReplicateNormalizer) normalizeRequest(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var req replicateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return zeroReplicate(meta), fmt.Errorf("replicate: request unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "replicate-prediction",
		Model:            firstNonEmpty(req.Version, meta.Model),
		Stream:           req.Stream,
	}
	if len(req.Input) > 0 && string(req.Input) != "null" {
		var inp replicateInput
		if err := json.Unmarshal(req.Input, &inp); err == nil {
			switch {
			case len(inp.Messages) > 0:
				for _, m := range inp.Messages {
					out.Messages = append(out.Messages, core.Message{
						Role:    roleFromString(m.Role),
						Content: []core.ContentBlock{{Type: core.ContentText, Text: m.Content}},
					})
				}
			case inp.Prompt != "":
				out.Messages = append(out.Messages, core.Message{
					Role:    core.RoleUser,
					Content: []core.ContentBlock{{Type: core.ContentText, Text: inp.Prompt}},
				})
			}
		}
	}
	if len(out.Messages) == 0 {
		// We claim a low-confidence parse — usable for routing-shape
		// detection but missing message content.
		out.Messages = nil
		return out, fmt.Errorf("replicate: no recoverable input content: %w", core.ErrUnsupported)
	}
	return out, nil
}

type replicateResponse struct {
	ID        string            `json:"id"`
	Version   string            `json:"version"`
	Status    string            `json:"status"`
	Output    json.RawMessage   `json:"output,omitempty"`
	Error     string            `json:"error,omitempty"`
	CreatedAt string            `json:"created_at,omitempty"`
	Metrics   *replicateMetrics `json:"metrics,omitempty"`
}

type replicateMetrics struct {
	InputTokenCount  int `json:"input_token_count,omitempty"`
	OutputTokenCount int `json:"output_token_count,omitempty"`
}

func (n *ReplicateNormalizer) normalizeResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	var resp replicateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zeroReplicate(meta), fmt.Errorf("replicate: response unmarshal: %w", err)
	}
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "replicate-prediction",
		Model:            firstNonEmpty(resp.Version, meta.Model),
	}
	// Extract Usage first so usage-only bodies still produce tokens.
	if resp.Metrics != nil && (resp.Metrics.InputTokenCount != 0 || resp.Metrics.OutputTokenCount != 0) {
		out.Usage = replicateUsageToCanonical(resp.Metrics)
	}
	content := replicateExtractOutputText(resp.Output)
	finishReason := "stop"
	switch resp.Status {
	case "failed", "canceled":
		finishReason = "error"
	}
	if resp.Error != "" {
		if content == "" {
			content = resp.Error
		}
		finishReason = "error"
	}
	out.FinishReason = finishReason
	if content == "" {
		return out, fmt.Errorf("replicate: no output content: %w", core.ErrUnsupported)
	}
	out.Messages = []core.Message{{
		Role:         core.RoleAssistant,
		Content:      []core.ContentBlock{{Type: core.ContentText, Text: content}},
		FinishReason: finishReason,
	}}
	return out, nil
}

// replicateExtractOutputText pulls assistant text from Replicate's
// polymorphic `output` field (string / array of strings / object with
// known string keys).
func replicateExtractOutputText(out json.RawMessage) string {
	if len(out) == 0 || string(out) == "null" {
		return ""
	}
	// String.
	var s string
	if err := json.Unmarshal(out, &s); err == nil {
		return s
	}
	// Array of strings.
	var arr []json.RawMessage
	if err := json.Unmarshal(out, &arr); err == nil {
		var b strings.Builder
		for _, item := range arr {
			var s string
			if err := json.Unmarshal(item, &s); err == nil {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	// Object with known keys.
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err == nil {
		for _, key := range []string{"text", "answer", "completion", "message"} {
			if v, ok := obj[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

func replicateUsageToCanonical(m *replicateMetrics) *core.Usage {
	out := &core.Usage{}
	setIntPtr(&out.PromptTokens, m.InputTokenCount)
	setIntPtr(&out.CompletionTokens, m.OutputTokenCount)
	if m.InputTokenCount != 0 || m.OutputTokenCount != 0 {
		tot := m.InputTokenCount + m.OutputTokenCount
		out.TotalTokens = &tot
	}
	return out
}

func zeroReplicate(meta core.Meta) core.NormalizedPayload {
	return core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "replicate-prediction",
		Model:            meta.Model,
	}
}
