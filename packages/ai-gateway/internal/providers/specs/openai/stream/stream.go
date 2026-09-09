package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specutil"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// StreamDecoder parses OpenAI-format SSE streams (shape also used by
// DeepSeek, Moonshot, SiliconFlow, Together, Groq, OpenRouter, Fireworks,
// Ollama, GLM's OpenAI-compat endpoint, and Azure OpenAI deployments).
type StreamDecoder struct {
	log *slog.Logger
}

// NewStreamDecoder returns a StreamDecoder.
func NewStreamDecoder(log *slog.Logger) *StreamDecoder {
	if log == nil {
		log = slog.Default()
	}
	return &StreamDecoder{log: log}
}

// Open wraps body in the right session for the endpoint:
//   - EndpointResponsesAPI → responsesEgressSession (content-detecting: forwards
//     genuine Responses frames verbatim, decodes a chat.completion upstream into
//     canonical so the proxy re-shapes it to Responses events — never leaks the
//     wrong wire shape to a /v1/responses client)
//   - everything else      → openaiStreamSession (chat-completions SSE)
//
// The dispatch is necessary because /v1/responses sends a completely different
// SSE event shape (event: response.output_text.delta + nested `delta` field)
// vs /v1/chat/completions (data: {choices:[{delta}]}). Feeding Responses bytes
// into openaiStreamSession would silently parse no content.
func (d *StreamDecoder) Open(body io.ReadCloser, endpoint typology.WireShape) (provcore.StreamSession, error) {
	if body == nil {
		return nil, fmt.Errorf("openai: nil stream body")
	}
	if endpoint == typology.WireShapeOpenAIResponses {
		return &responsesEgressSession{
			scanner: specutil.NewSSEScanner(body),
			log:     d.log,
		}, nil
	}
	return &openaiStreamSession{
		scanner: specutil.NewSSEScanner(body),
		log:     d.log,
	}, nil
}

type openaiStreamSession struct {
	scanner *specutil.SSEScanner
	log     *slog.Logger
	done    bool
}

func (s *openaiStreamSession) Next(ctx context.Context) (provcore.Chunk, error) {
	if s.done {
		return provcore.Chunk{}, io.EOF
	}

	// Loop until a frame with a non-empty `data:` payload arrives. Empty
	// frames (SSE keep-alives / comments) are skipped via `continue`
	// rather than a tail-recursive `return s.Next(ctx)`: Go has no tail-call
	// optimisation, so a hostile or broken upstream flooding empty `data:`
	// frames would otherwise grow the goroutine stack without bound.
	var ev specutil.SSEEvent
	for {
		if err := ctx.Err(); err != nil {
			return provcore.Chunk{}, err
		}
		var err error
		ev, err = s.scanner.Next()
		if err != nil {
			return provcore.Chunk{}, err
		}
		if bytes.Equal(bytes.TrimSpace(ev.Data), []byte("[DONE]")) {
			s.done = true
			// Re-emit the canonical "data: [DONE]\n\n" SSE frame so the
			// handler can forward it verbatim to OpenAI-compat clients.
			return provcore.Chunk{
				Done:        true,
				RawBytes:    []byte("data: [DONE]\n\n"),
				NativeEvent: ev.Event,
			}, nil
		}
		if len(ev.Data) == 0 {
			continue
		}
		break
	}

	if pe := streamFrameError(ev.Data); pe != nil {
		s.done = true
		return provcore.Chunk{}, pe
	}

	return chatChunkFromFrame(ev), nil
}

// streamFrameError reports an OpenAI-compatible upstream that signalled a
// mid-stream failure by sending an error envelope as a data frame rather
// than by closing the connection. Returns nil for an ordinary chunk.
//
// Without this arm the frame fell through to [chatChunkFromFrame], which
// reads only `choices` and `usage`, and decoded to a chunk carrying no
// content and no error. The stream then ended normally, so the caller
// received a truncated answer that was indistinguishable from a short
// complete one — the failure §3a Rule 10 forbids.
//
// The canonical code is always upstream_error, never the class the vendor's
// envelope would normalize to on a fresh request. By the time a data frame
// arrives the response is committed at HTTP 200 with bytes already sent, so
// a code the executor treats as retryable would be answering a question that
// is no longer open. The vendor's own type survives on Type for triage.
func streamFrameError(data []byte) *provcore.ProviderError {
	errObj := gjson.GetBytes(data, "error")
	if !errObj.IsObject() {
		return nil
	}
	pe := &provcore.ProviderError{
		Status:  http.StatusBadGateway,
		Code:    provcore.CodeUpstreamError,
		Type:    firstNonEmptyStr(errObj.Get("type").String(), errObj.Get("code").String()),
		Message: errObj.Get("message").String(),
		Raw:     data,
	}
	if pe.Message == "" {
		pe.Message = "upstream sent an error frame mid-stream with no message"
	}
	return pe
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// chatChunkFromFrame decodes one non-empty OpenAI chat-completions SSE data
// frame into a canonical Chunk. Extracted so the /v1/responses content copier
// can reuse the exact chat-decode path when an upstream tagged for Responses
// actually returns chat.completion.chunk frames (the bulletproof egress).
func chatChunkFromFrame(ev specutil.SSEEvent) provcore.Chunk {
	chunk := provcore.Chunk{
		RawBytes:    formatSSE(ev.Event, ev.Data),
		NativeEvent: ev.Event,
	}

	// ONE walk of the frame, then one walk of each nested object it needs.
	//
	// gjson has no parse tree: every Get re-scans the raw bytes it is given, so
	// `choice0.Get("delta.content")` followed by `choice0.Get("delta.refusal")`
	// walks the delta object twice, and this function had grown to seven such
	// reads plus two top-level ones — nine scans per frame, on the hottest
	// application function in the streaming path (~15% of gateway CPU at 1000
	// rps, essentially all of it inside gjson). Iterating keys instead reads
	// each object once and dispatches on the key, which costs the same walk a
	// single Get would and answers every field.
	// GetBytes, not ParseBytes: gjson's ParseBytes copies the whole frame into a
	// string, which showed up as ~50% more bytes per frame for the same alloc
	// count — a real cost on a per-token path, traded for two top-level scans
	// that are cheap because both keys sit late in the object either way.
	//
	// `choices.0` is the first ELEMENT, not choice number zero: an n>1 turn
	// interleaves frames that each carry one choice, so the element's own
	// `index` field below is what says which candidate this delta belongs to.
	choice0 := gjson.GetBytes(ev.Data, "choices.0")
	usage := gjson.GetBytes(ev.Data, "usage")

	if choice0.Exists() {
		// reasoning_content is the channel thinking models (DeepSeek-R1/V4, Kimi
		// K2) stream chain-of-thought on, and `reasoning` is the alternate wire
		// name xAI and OpenRouter use for the same thing. Both are collected and
		// the primary spelling wins, matching the shared normalize fold
		// (openai_chat.go Delta.Reasoning) — summing both would double a
		// transcript that carries the same text under two names.
		var reasoningContent, reasoningAlias string
		choice0.ForEach(func(key, value gjson.Result) bool {
			switch key.Str {
			case "index":
				chunk.ChoiceIndex = int(value.Int())
			case "finish_reason":
				// Rides a trailing chunk (delta empty) and is already in the
				// canonical OpenAI vocabulary (stop / length / tool_calls /
				// content_filter). Surfaced so a re-encoder (buffer mode)
				// preserves the real value instead of collapsing to "stop".
				if value.Type == gjson.String && value.Str != "" {
					chunk.FinishReason = value.Str
				}
			case "delta":
				value.ForEach(func(dk, dv gjson.Result) bool {
					switch dk.Str {
					case "content":
						chunk.Delta = dv.String()
					case "reasoning_content":
						reasoningContent = dv.String()
					case "reasoning":
						reasoningAlias = dv.String()
					case "refusal":
						// What a structured-outputs model streams INSTEAD of
						// content when it declines. Every non-refusing chunk
						// carries `"refusal":null`, so the type is checked
						// rather than mere presence.
						if dv.Type == gjson.String && dv.Str != "" {
							chunk.RefusalDelta += dv.Str
						}
					case "tool_calls":
						if !dv.IsArray() {
							return true
						}
						dv.ForEach(func(_, tc gjson.Result) bool {
							var d provcore.ToolCallDelta
							tc.ForEach(func(tk, tv gjson.Result) bool {
								switch tk.Str {
								case "index":
									d.Index = int(tv.Int())
								case "id":
									d.ID = tv.String()
								case "function":
									tv.ForEach(func(fk, fv gjson.Result) bool {
										switch fk.Str {
										case "name":
											d.Name = fv.String()
										case "arguments":
											d.Arguments = fv.String()
										}
										return true
									})
								}
								return true
							})
							chunk.ToolCallDeltas = append(chunk.ToolCallDeltas, d)
							return true
						})
					}
					return true
				})
			}
			return true
		})
		// Route reasoning to its own channel, NOT Delta: every cross-format
		// encoder maps ReasoningDelta to the target's reasoning channel (Gemini
		// `thought:true`, Anthropic `thinking_delta`, OpenAI
		// `delta.reasoning_content`), and appending to Delta instead leaked the
		// chain-of-thought into the visible answer on every transcode. Native
		// openai-family passthrough is unaffected — it forwards RawBytes.
		if reasoningContent != "" {
			chunk.ReasoningDelta += reasoningContent
		} else if reasoningAlias != "" {
			chunk.ReasoningDelta += reasoningAlias
		}
	}

	if usage.IsObject() {
		// Streaming Usage extraction via provcore.ExtractUsage (same parser
		// path as the non-streaming codec, compliance proxy, agent, and Hub
		// audit). The full SSE chunk JSON is passed; shared/normalize finds the
		// usage block and applies the canonical alias chain (Kimi flat /
		// DeepSeek / Moonshot / OpenAI Responses-shape fallbacks).
		//
		// Guard on IsObject (not Exists): OpenAI-stream chunks carry
		// `"usage": null` on EVERY non-final delta, and gjson reports an
		// explicit null as Exists()==true. Running the full normalizer on each
		// null-usage chunk re-ran Tier-1 confidence scoring + canonical assembly
		// per delta — the dominant streaming allocator. A real usage block is
		// always a JSON object, so IsObject() runs ExtractUsage only on the
		// final include_usage chunk while staying correct for every alias shape.
		u := provcore.ExtractUsage(ev.Data, provcore.FormatOpenAI)
		// Zero-value Usage means the alias chain didn't recognise the
		// shape — fall back to nil so downstream stamping treats it as
		// "not reported" instead of "reported zero".
		if u.PromptTokens != nil || u.CompletionTokens != nil ||
			u.TotalTokens != nil || u.CacheReadTokens != nil ||
			u.CacheCreationTokens != nil || u.ReasoningTokens != nil {
			chunk.Usage = &u
		}
	}

	return chunk
}

func (s *openaiStreamSession) Close() error {
	s.done = true
	return s.scanner.Close()
}

// formatSSE rebuilds the canonical SSE frame bytes from the parsed
// event + data pair so the Chunk.RawBytes can be forwarded to the
// client verbatim.
func formatSSE(event string, data []byte) []byte {
	buf := bytes.Buffer{}
	if event != "" {
		buf.WriteString("event: ")
		buf.WriteString(event)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}
