package canonicalbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// openAIStreamEncoder converts canonical chunks to OpenAI chat.completion.chunk
// SSE frames. Used when ingress is OpenAI-like but the upstream provider speaks
// a different wire format (e.g. openai → anthropic, deepseek → gemini).
// LivePipeline appends data:[DONE] for OpenAI-shape ingress via EmitOpenAIDone,
// so this encoder emits nothing for chunk.Done after the finish_reason frame.
//
// Every emitted SSE frame carries the full OpenAI chunk envelope (id, object,
// created, model) so downstream clients can parse the stream with standard
// OpenAI SDKs.
type openAIStreamEncoder struct {
	id         string
	created    int64
	model      string
	headerSent bool
	// scratch is reused across Write calls; each Write truncates it to zero
	// length before appending its frames. The slice returned by Write aliases
	// scratch and may be overwritten by the next Write — callers MUST NOT retain
	// it (the io.Writer "must not retain p" contract; every call site writes it
	// out synchronously before the next Write).
	scratch []byte
	// contentSuffix is the precomputed tail of a content-delta frame — everything
	// after the (variable) content string. See stream_encoders_fastpath.go
	// (emitContentDelta) for how the per-token hot path uses it to avoid
	// marshalling the whole envelope struct graph.
	contentSuffix []byte
	finishTracker
	// roleSent tracks which choices have had their opening role delta emitted.
	// An n>1 turn opens each candidate separately, so this is per-choice rather
	// than a single bool.
	roleSent map[int]bool
	// finishSent records that a finish_reason frame already went out on the
	// chunk that observed it, so the terminal frame does not emit a second one.
	finishSent bool
}

// result returns the frames accumulated by this Write, or nil when it produced
// none (the caller treats nil as "skip this chunk").
func (e *openAIStreamEncoder) result() ([]byte, error) {
	if len(e.scratch) == 0 {
		return nil, nil
	}
	return e.scratch, nil
}

// NewChatCompletionsStreamEncoder returns an encoder that converts canonical
// provider.Chunk values into OpenAI chat-completions SSE frames. Exported for
// the auto-upgrade path (handler) which feeds upstream Responses-SSE-derived
// chunks back to a chat-completions client regardless of the (ingress, target)
// pair the bridge's NewStreamTranscoder would otherwise pick.
func NewChatCompletionsStreamEncoder(model string) StreamTranscoder {
	return newOpenAIStreamEncoder(model)
}

// NewResponsesStreamEncoder returns an encoder that converts canonical
// provider.Chunk values into OpenAI /v1/responses SSE event grammar.
// Exported for the cross-ingress cache-HIT path: when a stream-HIT
// entry's origin shape differs from the current ingress (e.g. cached
// chat.completion SSE replayed for a /v1/responses caller), the
// standard [Bridge.NewStreamTranscoder] passthrough rule short-circuits
// before the right encoder is picked; the handler bypasses that gate
// by calling this directly.
func NewResponsesStreamEncoder(model string) StreamTranscoder {
	return newResponsesStreamEncoder(model)
}

// IngressStreamEncoder returns the encoder that re-encodes canonical
// provider.Chunk values into the CALLER's ingress-native SSE wire shape. It is
// the single source of truth for "given canonical chunks, which wire does this
// ingress speak" — consumed both by [Bridge.NewStreamTranscoder] (the
// cross-format live path) and by the buffer / Model-A re-emit path
// (proxy.fallbackStreamEncoder). Keeping one switch eliminates the drift class
// where a second, partial copy defaulted every non-OpenAI ingress to the
// chat-completions encoder — which leaked chat.completion.chunk frames to a
// Gemini / Anthropic / Responses client whenever the enforcing buffer path ran.
//
// It NEVER returns nil: the buffer/re-emit caller must always have a concrete
// encoder. OpenAI-family ingresses (and any unrecognised ingress) get the
// chat-completions encoder, since canonical IS the chat-completions shape.
func IngressStreamEncoder(ingress provcore.Format, model string) StreamTranscoder {
	switch ingress {
	case provcore.FormatOpenAIResponses:
		return newResponsesStreamEncoder(model)
	case provcore.FormatAnthropic:
		return newAnthropicStreamEncoder()
	case provcore.FormatGemini, provcore.FormatVertex:
		return &geminiStreamEncoder{}
	case provcore.FormatCohere:
		// openBlock starts at -1: the zero value would claim block 0 is already
		// open, so the first delta would emit no content-start at all.
		return &cohereStreamEncoder{openBlock: -1}
	case provcore.FormatReplicate:
		return &replicateStreamEncoder{}
	default:
		return newOpenAIStreamEncoder(model)
	}
}

func newOpenAIStreamEncoder(model string) *openAIStreamEncoder {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return &openAIStreamEncoder{
		id:      "chatcmpl-" + hex.EncodeToString(b),
		created: time.Now().Unix(),
		model:   model,
	}
}

// emit marshals one envelope (single choice + optional usage) and appends the
// SSE frame to the reused scratch buffer. The payload is ALWAYS produced by
// json.Marshal (preserving go-json's HTML-safe escape); only the `data: ` /
// `\n\n` framing is hand-assembled — no user/upstream data is hand-serialised.
func (e *openAIStreamEncoder) emit(choice oaiStreamChoice, usage *oaiStreamUsage) {
	data, _ := json.Marshal(oaiStreamEnvelope{
		Choices: []oaiStreamChoice{choice},
		Created: e.created,
		ID:      e.id,
		Model:   e.model,
		Object:  "chat.completion.chunk",
		Usage:   usage,
	})
	e.scratch = append(e.scratch, "data: "...)
	e.scratch = append(e.scratch, data...)
	e.scratch = append(e.scratch, '\n', '\n')
}

func (e *openAIStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	e.scratch = e.scratch[:0]

	idx := chunk.ChoiceIndex
	e.observe(chunk)

	// Emit a role-assignment chunk before any content, once per CHOICE. An n>1
	// turn opens each candidate with its own role delta, so a single header
	// would leave every candidate past the first without one. Choice 0 keeps
	// using headerSent, which callers construct the encoder with already set.
	sentRole := e.headerSent
	if idx != 0 {
		sentRole = e.roleSent[idx]
	}
	if !sentRole {
		if idx == 0 {
			e.headerSent = true
		} else {
			if e.roleSent == nil {
				e.roleSent = map[int]bool{}
			}
			e.roleSent[idx] = true
		}
		empty := ""
		e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{Content: &empty, Role: "assistant"}}, nil)
	}

	// Check content before Done: providers like Gemini 2.5 combine text,
	// finishReason, and usageMetadata into a single SSE frame, so chunk.Delta
	// can be non-empty even when chunk.Done is also true.
	if chunk.Delta != "" {
		// Per-token hot path: byte-identical to
		// emit(oaiStreamChoice{Delta:{Content:&d}}, nil) but skips the envelope
		// struct reflection (the dominant streaming encode cost). Its precomputed
		// suffix bakes in `"index":0`, so a candidate past the first takes the
		// struct path — correctness over the optimisation on the rare shape.
		if idx == 0 {
			e.emitContentDelta(chunk.Delta)
		} else {
			d := chunk.Delta
			e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{Content: &d}}, nil)
		}
	}
	if len(chunk.ToolCallDeltas) > 0 {
		e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{ToolCalls: buildOAIToolCalls(chunk.ToolCallDeltas)}}, nil)
	}
	if len(chunk.NexusThinking) > 0 {
		e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{NexusThinking: chunk.NexusThinking}}, nil)
	}
	if chunk.ReasoningDelta != "" {
		e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{ReasoningContent: chunk.ReasoningDelta}}, nil)
	}
	if chunk.RefusalDelta != "" {
		e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{Refusal: chunk.RefusalDelta}}, nil)
	}
	// A finish_reason is emitted on the chunk that OBSERVED it, tagged with that
	// chunk's choice. Deferring every finish to the terminal chunk collapses an
	// n>1 turn to one finish frame for the whole stream, and it rewrote a
	// tool-calling turn's "tool_calls" into "stop" — the terminal chunk carries
	// no finish of its own, because the wire puts it on the preceding
	// delta-empty frame.
	if chunk.FinishReason != "" && !chunk.Done {
		fr := finishReasonOrStop(chunk.FinishReason)
		e.finishSent = true
		e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{}, FinishReason: &fr}, nil)
	}
	if chunk.Done {
		// The terminal frame carries usage, and carries a finish_reason only
		// when none was seen earlier — the cross-format case, where a decoder
		// reports the stop condition on the terminal chunk itself.
		usage := buildOAIStreamUsage(chunk.Usage)
		if e.finishSent && usage == nil {
			return e.result()
		}
		if e.finishSent {
			e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{}}, usage)
			return e.result()
		}
		fr := finishReasonOrStop(e.resolve(chunk))
		e.emit(oaiStreamChoice{Index: idx, Delta: oaiStreamDelta{}, FinishReason: &fr}, usage)
	}

	if len(e.scratch) == 0 {
		return nil, nil
	}
	return e.scratch, nil
}

// buildOAIStreamUsage maps a canonical usage into the wire usage block: a token
// field is set whenever its source pointer is non-nil (so a non-nil 0 still
// renders), and a detail sub-block appears only when its source is non-nil AND
// > 0. Returns nil when no usage was reported (the usage key is omitted).
func buildOAIStreamUsage(u *provcore.Usage) *oaiStreamUsage {
	if u == nil {
		return nil
	}
	out := &oaiStreamUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.CacheReadTokens != nil && *u.CacheReadTokens > 0 {
		out.PromptTokensDetails = &oaiPromptTokensDetails{CachedTokens: *u.CacheReadTokens}
	}
	if u.ReasoningTokens != nil && *u.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &oaiCompletionTokensDetails{ReasoningTokens: *u.ReasoningTokens}
	}
	return out
}

// geminiStreamEncoder converts canonical chunks to Gemini streamGenerateContent
// SSE format. Vertex ingress uses the same wire shape, so this encoder serves
// both FormatGemini and FormatVertex.
//
// Each text delta becomes a candidate part; tool calls become functionCall parts.
// The Done chunk carries finishReason="STOP" and usageMetadata.
type geminiStreamEncoder struct{ finishTracker }

func (e *geminiStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	e.observe(chunk)
	var parts []any
	if chunk.Delta != "" {
		parts = append(parts, map[string]any{"text": chunk.Delta})
	}
	for _, tc := range chunk.ToolCallDeltas {
		// Gemini sends function calls as complete (not streamed arguments),
		// so only emit when we have an ID or name (i.e. start of call).
		if tc.Name != "" {
			var args map[string]any
			if tc.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Arguments), &args)
			}
			if args == nil {
				args = map[string]any{}
			}
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"id":   tc.ID,
					"name": tc.Name,
					"args": args,
				},
			})
			if tc.ThoughtSignature != "" {
				parts[len(parts)-1].(map[string]any)["thoughtSignature"] = tc.ThoughtSignature
			}
		}
	}
	if chunk.ReasoningDelta != "" {
		// Tag reasoning as a Gemini thought part (`thought:true`) to match the
		// non-stream egress (spec_gemini/ingress/hub_ingress.go) AND to keep the
		// stream symmetric with the gemini stream DECODER, which routes
		// thought:true parts back to ReasoningDelta. Without the tag, a
		// cross-format reasoning delta (DeepSeek/OpenAI reasoning_content,
		// Anthropic thinking) leaks into the visible answer text instead of the
		// reasoning channel, and a gemini→…→gemini round-trip loses the thought
		// classification.
		parts = append(parts, map[string]any{"text": chunk.ReasoningDelta, "thought": true})
	}
	if chunk.RefusalDelta != "" {
		// Gemini has no refusal channel. Rendering the decline as ordinary model
		// text is a deliberate choice over dropping it: a Gemini-shaped client
		// would otherwise receive an EMPTY turn where the model actually
		// refused, and an empty answer is harder to act on than a visible
		// decline. The classification is what is lost, not the text.
		parts = append(parts, map[string]any{"text": chunk.RefusalDelta})
	}
	if len(parts) == 0 && !chunk.Done {
		return nil, nil
	}
	candidate := map[string]any{
		"content": map[string]any{"parts": parts, "role": "model"},
		"index":   0,
	}
	if chunk.Done {
		candidate["finishReason"] = canonicalFinishToGemini(e.resolve(chunk))
	}
	resp := map[string]any{"candidates": []any{candidate}}
	if chunk.Done {
		if u := buildGeminiUsage(chunk.Usage); u != nil {
			resp["usageMetadata"] = u
		}
	}
	return geminiSSEFrame(resp), nil
}

// cohereStreamEncoder converts canonical chunks to Cohere Chat v2 streaming
// event format (message-start → content-start → content-delta → content-end
// → message-end).
type cohereStreamEncoder struct {
	headerSent bool
	// openBlock is the index of the content block currently open, or -1. Cohere
	// closes each block before opening the next and numbers them in the order
	// they open, so one cursor models the whole thing — two independent "is it
	// open" flags could not express "close the other one first" and produced
	// overlapping blocks with hardcoded, inverted indices.
	openBlock int
	// openKind is what that block declared itself to be ("text" / "thinking"),
	// so a delta of the other kind knows it has to start a new block.
	openKind string
	// nextBlock is the index the next block will take.
	nextBlock int
	finishTracker
}

func (e *cohereStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	e.observe(chunk)
	var buf bytes.Buffer

	if !e.headerSent {
		e.headerSent = true
		writeCohereEvent(&buf, map[string]any{
			"type": "message-start",
			"id":   "transcoded",
			"delta": map[string]any{
				"message": map[string]any{"role": "assistant"},
			},
		})
	}

	// Cohere has TWO reasoning-ish channels that both decode to the one
	// canonical ReasoningDelta: tool-plan-delta (the plan for calling tools) and
	// a `thinking` content block (chain of thought). NativeEvent is what tells
	// them apart on a same-format turn — without it a tool plan round-trips as
	// the model's private thinking, which keeps every byte and still mislabels
	// it. Cross-format reasoning carries some other provider's event name and
	// takes the thinking block, which is the closer of the two.
	if chunk.ReasoningDelta != "" && chunk.NativeEvent == "tool-plan-delta" {
		writeCohereEvent(&buf, map[string]any{
			"type":  "tool-plan-delta",
			"delta": map[string]any{"message": map[string]any{"tool_plan": chunk.ReasoningDelta}},
		})
	} else if chunk.ReasoningDelta != "" {
		idx := e.openCohereBlock(&buf, "thinking")
		writeCohereEvent(&buf, map[string]any{
			"type":  "content-delta",
			"index": idx,
			"delta": map[string]any{
				"message": map[string]any{
					"content": map[string]any{"thinking": chunk.ReasoningDelta},
				},
			},
		})
	}

	// Cohere has no refusal channel, so a decline joins the text run. Dropping it
	// would hand a Cohere-shaped client an EMPTY turn where the model refused;
	// the classification is what is lost, not the text.
	for _, text := range [...]string{chunk.Delta, chunk.RefusalDelta} {
		if text == "" {
			continue
		}
		idx := e.openCohereBlock(&buf, "text")
		writeCohereEvent(&buf, map[string]any{
			"type":  "content-delta",
			"index": idx,
			"delta": map[string]any{
				"message": map[string]any{
					"content": map[string]any{"text": text},
				},
			},
		})
	}

	for _, tc := range chunk.ToolCallDeltas {
		if tc.Name != "" {
			writeCohereEvent(&buf, map[string]any{
				"type":  "tool-call-start",
				"index": tc.Index,
				"delta": map[string]any{
					"message": map[string]any{
						// tool_calls is an OBJECT on this wire, not an array —
						// the captured stream sends
						// {"tool_calls":{"id":…,"function":{…}}}. Emitting an
						// array put the call where a Cohere client does not
						// look for it.
						"tool_calls": map[string]any{
							"id":   tc.ID,
							"type": "function",
							"function": map[string]any{
								"name":      tc.Name,
								"arguments": tc.Arguments,
							},
						},
					},
				},
			})
		} else if tc.Arguments != "" {
			writeCohereEvent(&buf, map[string]any{
				"type":  "tool-call-delta",
				"index": tc.Index,
				"delta": map[string]any{
					"message": map[string]any{
						"tool_calls": map[string]any{
							"function": map[string]any{"arguments": tc.Arguments},
						},
					},
				},
			})
		}
	}

	if chunk.Done {
		e.closeCohereBlock(&buf)
		msgEnd := map[string]any{
			"type":  "message-end",
			"delta": map[string]any{"finish_reason": canonicalFinishToCohere(e.resolve(chunk))},
		}
		if chunk.Usage != nil {
			tokens := map[string]any{}
			if chunk.Usage.PromptTokens != nil {
				tokens["input_tokens"] = *chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens != nil {
				tokens["output_tokens"] = *chunk.Usage.CompletionTokens
			}
			msgEnd["usage"] = map[string]any{"tokens": tokens}
		}
		writeCohereEvent(&buf, msgEnd)
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// replicateStreamEncoder converts canonical chunks to Replicate SSE output
// events (event: output + event: done). Replicate output data is plain text,
// not JSON.
type replicateStreamEncoder struct{}

func (e *replicateStreamEncoder) Write(_ context.Context, chunk provcore.Chunk) ([]byte, error) {
	if chunk.Done {
		return []byte("event: done\ndata: {}\n\n"), nil
	}
	if chunk.Delta != "" {
		// Replicate output event data is the raw token text — no JSON wrapping.
		return fmt.Appendf(nil, "event: output\ndata: %s\n\n", chunk.Delta), nil
	}
	// Replicate's stream is a bare token feed with no refusal channel, so a
	// decline goes out as output text rather than as an empty run.
	if chunk.RefusalDelta != "" {
		return fmt.Appendf(nil, "event: output\ndata: %s\n\n", chunk.RefusalDelta), nil
	}
	return nil, nil
}

// --- shared helpers ---

func writeCohereEvent(buf *bytes.Buffer, payload map[string]any) {
	data, _ := json.Marshal(payload)
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
}

func geminiSSEFrame(payload map[string]any) []byte {
	data, _ := json.Marshal(payload)
	return fmt.Appendf(nil, "data: %s\n\n", data)
}

func buildGeminiUsage(u *provcore.Usage) map[string]any {
	if u == nil {
		return nil
	}
	out := map[string]any{}
	if u.PromptTokens != nil {
		out["promptTokenCount"] = *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		out["candidatesTokenCount"] = *u.CompletionTokens
	}
	if u.TotalTokens != nil {
		out["totalTokenCount"] = *u.TotalTokens
	}
	// Cache + reasoning fields — mirror the non-stream egress translation
	// in spec_gemini/hub_ingress.go OpenAIChatCompletionToGenerateContentResponse.
	// These must be present in the streaming /v1beta egress path too;
	// omitting them causes cross-format callers (e.g. /v1beta SSE → claude
	// target) to see no cachedContentTokenCount in the final usage SSE frame
	// and incorrectly classify the response as a cache miss.
	if u.CacheReadTokens != nil && *u.CacheReadTokens > 0 {
		out["cachedContentTokenCount"] = *u.CacheReadTokens
	}
	if u.ReasoningTokens != nil && *u.ReasoningTokens > 0 {
		out["thoughtsTokenCount"] = *u.ReasoningTokens
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// finishReasonOrStop returns the canonical OpenAI finish_reason, defaulting to
// "stop" when the upstream stream never reported one. An empty FinishReason on
// the terminal chunk is the case for the live cross-format path (whose Done
// chunk is not threaded with a captured finish_reason); only buffer mode
// threads the real value, so this default preserves prior live behavior.
func finishReasonOrStop(fr string) string {
	if fr == "" {
		return "stop"
	}
	return fr
}

// canonicalFinishToAnthropicStop maps a canonical OpenAI finish_reason to an
// Anthropic stop_reason for the synthesized terminal message_delta. Empty →
// "end_turn" (the historical default), keeping the live transcode unchanged.
// Inverse of anthropic/codec.MapStopReason.
//
// content_filter → "refusal", per the documented Anthropic vocabulary
// (end_turn / max_tokens / stop_sequence / tool_use / pause_turn / refusal /
// model_context_window_exceeded), where refusal is defined as "when streaming
// classifiers intervene to handle potential policy violations". Mapping it
// to "stop_sequence" is not a lossy approximation but a
// different and false claim: stop_sequence means the caller's OWN custom
// stop string was generated, so a filtered answer would be reported as the
// caller's own stop rule firing.
func canonicalFinishToAnthropicStop(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "", "stop":
		return "end_turn"
	default:
		return fr
	}
}

// canonicalFinishToGemini maps a canonical OpenAI finish_reason to a Gemini
// finishReason for the synthesized terminal candidate. Empty → "STOP".
// Inverse of gemini/codec.MapFinishReason.
func canonicalFinishToGemini(fr string) string {
	switch fr {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "", "stop", "tool_calls", "function_call":
		return "STOP"
	default:
		return "OTHER"
	}
}

// canonicalFinishToCohere maps a canonical OpenAI finish_reason to a Cohere
// finish_reason for the synthesized terminal message-end. Empty → "COMPLETE".
func canonicalFinishToCohere(fr string) string {
	switch fr {
	case "length":
		return "MAX_TOKENS"
	case "tool_calls", "function_call":
		return "TOOL_CALL"
	default:
		return "COMPLETE"
	}
}

// buildOAIToolCalls converts canonical tool-call deltas into the OpenAI
// streaming tool_calls shape. Type=function is set only when an id starts a new
// call; continuation deltas carry just index + arguments.
func buildOAIToolCalls(deltas []provcore.ToolCallDelta) []oaiToolCall {
	calls := make([]oaiToolCall, 0, len(deltas))
	for _, d := range deltas {
		tc := oaiToolCall{Index: d.Index, Function: oaiToolFunc{Name: d.Name, Arguments: d.Arguments, ThoughtSignature: d.ThoughtSignature}}
		if d.ID != "" {
			tc.ID = d.ID
			tc.Type = "function"
		}
		calls = append(calls, tc)
	}
	return calls
}

// openCohereBlock returns the index of a block of `kind`, opening one — and
// closing whatever was open first — when the current block is of another kind.
//
// Cohere's own stream does exactly this: content-start, deltas, content-end,
// then the next block at the next index. Emitting two overlapping blocks with
// hardcoded indices, as this used to, leaves a client that finalizes on
// content-end waiting for an event that never comes, and attributes the
// thinking to whichever index it thought was the answer.
func (e *cohereStreamEncoder) openCohereBlock(buf *bytes.Buffer, kind string) int {
	if e.openKind == kind && e.openBlock >= 0 {
		return e.openBlock
	}
	e.closeCohereBlock(buf)
	idx := e.nextBlock
	e.nextBlock++
	e.openBlock, e.openKind = idx, kind
	content := map[string]any{"type": kind}
	if kind == "thinking" {
		// Upstream declares the empty string on the start event; a client that
		// concatenates from the start event rather than from the first delta
		// would otherwise read a missing key.
		content["thinking"] = ""
	}
	writeCohereEvent(buf, map[string]any{
		"type":  "content-start",
		"index": idx,
		"delta": map[string]any{"message": map[string]any{"content": content}},
	})
	return idx
}

// closeCohereBlock emits content-end for the open block, if any.
func (e *cohereStreamEncoder) closeCohereBlock(buf *bytes.Buffer) {
	if e.openBlock < 0 {
		return
	}
	writeCohereEvent(buf, map[string]any{"type": "content-end", "index": e.openBlock})
	e.openBlock, e.openKind = -1, ""
}
