package codecs

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// Streaming response (SSE)

// Gemini's streamGenerateContent surface emits SSE events whose `data:`
// payloads are partial geminiResponse documents. Each chunk carries
// candidates[0].content.parts[] containing one or more deltas; tokens
// arrive concatenated rather than as a separate "delta" field. We stitch
// the deltas per (candidate-index, part-shape) and emit one assembled
// message per candidate, matching the non-stream output byte-for-byte.
func (n *GeminiGenerateNormalizer) normalizeStreamResponse(raw []byte, meta core.Meta) (core.NormalizedPayload, error) {
	out := core.NormalizedPayload{
		Kind:             core.KindAIChat,
		NormalizeVersion: core.SchemaVersion,
		Protocol:         "gemini-generate",
		Model:            meta.Model,
		Stream:           true,
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	// An ordered accumulator, not per-kind buckets. Grouping by kind loses
	// wire order — and worse, it fuses text that arrived on either side of
	// a tool call or a media part into one block, changing the model's own
	// words. Only CONSECUTIVE text deltas coalesce, which is what makes a
	// streamed row read the same as its non-streamed twin.
	type toolCallState struct {
		use *core.ToolUse
	}
	type candidateState struct {
		role         string
		finishReason string
		blocks       []core.ContentBlock
		// sealed marks the trailing block as closed to coalescing. It is
		// needed only for SYNTHETIC text: a marker is text-shaped but is
		// not the model speaking, so the next delta must start its own
		// block. Blocks of another kind — a tool call, a media element —
		// need no seal, because appendKind's type check already refuses to
		// merge into them.
		sealed     bool
		byPosition map[int]*toolCallState
	}
	// appendText merges a delta into the trailing text block when the last
	// thing seen was also text, and starts a new block otherwise. That is
	// the whole difference between "the model said A then called a tool then
	// said B" and "the model said AB".
	appendKind := func(st *candidateState, kind core.ContentType, s string) {
		if n := len(st.blocks); n > 0 && !st.sealed && st.blocks[n-1].Type == kind {
			st.blocks[n-1].Text += s
			return
		}
		st.blocks = append(st.blocks, core.ContentBlock{Type: kind, Text: s})
		st.sealed = false
	}
	// appendSealed emits a synthetic block that neither merges into what
	// precedes it nor accepts what follows.
	appendSealed := func(st *candidateState, s string) {
		st.blocks = append(st.blocks, core.ContentBlock{Type: core.ContentText, Text: s})
		st.sealed = true
	}
	// Gemini streams functionCall arguments as cumulative snapshots. Keep the
	// first block's position and replace its input with the latest snapshot;
	// repeated snapshots must not become duplicate tool calls.
	upsertToolCall := func(st *candidateState, candidate, position int, p geminiPart) {
		fc := p.FunctionCall
		if st.byPosition == nil {
			st.byPosition = make(map[int]*toolCallState)
		}
		// Part position owns snapshot continuity. Native IDs are not unique
		// enough to merge occurrences: if Gemini repeats one ID in two Parts,
		// preserve both so downstream duplicate-ID validation can fail closed.
		call := st.byPosition[position]
		if call == nil {
			args := "{}"
			if len(fc.Args) > 0 {
				if raw, err := json.Marshal(fc.Args); err == nil {
					args = string(raw)
				}
			}
			id := fc.ID
			if id == "" {
				id = geminiFallbackCallID(fc.Name, args, fmt.Sprintf("candidates.%d.content.parts", candidate), position)
			}
			call = &toolCallState{use: &core.ToolUse{CallID: id, Name: fc.Name, Input: fc.Args, ThoughtSignature: p.ThoughtSignature}}
			st.blocks = append(st.blocks, core.ContentBlock{Type: core.ContentToolUse, ToolUse: call.use})
		} else {
			if fc.ID != "" {
				call.use.CallID = fc.ID
			}
			if fc.Name != "" {
				call.use.Name = fc.Name
			}
			call.use.Input = fc.Args
			if p.ThoughtSignature != "" {
				call.use.ThoughtSignature = p.ThoughtSignature
			}
		}
		st.byPosition[position] = call
	}

	candidates := map[int]*candidateState{}
	var order []int
	var usage *core.Usage
	var sawAny bool
	// Frame coverage: recognized / total data frames ([DONE] and blank
	// data lines are protocol sentinels, counted in neither). A frame is
	// recognized when it carries generateContent structure (candidates,
	// usageMetadata, or the modelVersion envelope); unparseable JSON —
	// typically the cut-off final frame of a truncated capture — counts
	// toward the total only, so a truncated stream folds to its
	// decodable prefix with proportionally lower confidence instead of
	// erroring.
	var totalFrames, recognizedFrames int

	consume := func(payload string) {
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			return
		}
		totalFrames++
		var chunk geminiResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return
		}
		if chunk.UsageMetadata != nil || len(chunk.Candidates) > 0 || chunk.ModelVersion != "" {
			recognizedFrames++
		}
		if out.Model == "" && chunk.ModelVersion != "" {
			out.Model = chunk.ModelVersion
		}
		if chunk.UsageMetadata != nil {
			usage = geminiUsageToCanonical(chunk.UsageMetadata)
			// A usageMetadata-only frame (Gemini's final-flush carrying
			// only token counts, no candidates) is a valid stream
			// terminator; count it as a successful parse so the row
			// stays Tier-1 instead of falling through to Tier-3 with
			// the body still readable as an http-text blob.
			sawAny = true
		}
		for cpos, c := range chunk.Candidates {
			st, ok := candidates[c.Index]
			if !ok {
				st = &candidateState{}
				candidates[c.Index] = st
				order = append(order, c.Index)
			}
			if c.FinishReason != "" {
				st.finishReason = c.FinishReason
				// A finish-reason frame (the STOP frame Gemini emits
				// at end-of-stream with parts:[]) is a meaningful
				// signal that we successfully parsed the stream
				// envelope, even when no content delta accompanies
				// it. Treat it as "saw something" so the parser
				// doesn't bail to core.ErrUnsupported and let the row drop
				// to Tier-3 generic-http fallback.
				sawAny = true
			}
			if c.Content == nil {
				continue
			}
			if c.Content.Role != "" && st.role == "" {
				st.role = c.Content.Role
			}
			// Frame index of the frame being folded, 0-based over
			// data-bearing frames — totalFrames was incremented on entry.
			frame := totalFrames - 1
			for pi, p := range c.Content.Parts {
				switch {
				case p.Text != nil:
					// Reasoning is a kind, not a separate channel. Hoisting
					// it out of the ordered list left the text deltas on
					// either side of it adjacent, so they fused into one
					// utterance — the same defect the accumulator was
					// introduced to remove, on the one kind it skipped.
					kind := core.ContentText
					if p.Thought {
						kind = core.ContentReasoning
					}
					appendKind(st, kind, *p.Text)
					sawAny = true
				case p.FunctionCall != nil:
					upsertToolCall(st, c.Index, pi, p)
					sawAny = true
				case p.FunctionResponse != nil:
					tr := &core.ToolResult{CallID: p.FunctionResponse.ID}
					if len(p.FunctionResponse.Response) > 0 {
						if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
							tr.Output = string(b)
						}
					}
					st.blocks = append(st.blocks, core.ContentBlock{Type: core.ContentToolResult, ToolResult: tr})
					sawAny = true
				case p.InlineData != nil:
					// Gemini delivers a complete inlineData part inside one
					// chunk, so the bytes are whole within this frame and an
					// sse: locator addresses them exactly.
					st.blocks = append(st.blocks, mediaBlock(capturedMedia(
						p.InlineData.MimeType,
						p.InlineData.Data,
						locator.SSE(frame, "candidates."+strconv.Itoa(cpos)+".content.parts."+strconv.Itoa(pi)+".inlineData.data"),
					)))
					sawAny = true
				case p.FileData != nil:
					st.blocks = append(st.blocks, mediaBlock(geminiFileDataMedia(p.FileData.MimeType, p.FileData.FileURI)))
					sawAny = true
				case len(p.unmatched) > 0:
					// The no-silent-drop rule applies to the stream fold too;
					// closing it only on the non-stream path would leave the
					// class half-fixed.
					// Through appendKind, not a raw append: a raw text block
					// becomes a coalescing target, so the next delta fuses
					// into the marker and the model's word is glued to
					// synthetic text. Every text-shaped emission on this
					// path goes through the same door.
					appendSealed(st,
						payloadSafeText("[unrecognised gemini part: ", geminiPartKeys(p.unmatched)+"]"))
					sawAny = true
				}
			}
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		consume(strings.TrimPrefix(line, "data:"))
	}
	if err := scanner.Err(); err != nil {
		// The scanner stopped before the end of the capture (oversized
		// line); the unread tail is at least one frame we could not
		// decode, so it weighs on the coverage like one.
		totalFrames++
	}

	if !sawAny {
		// Vertex / AI Studio sometimes ship a single JSON object instead
		// of SSE on `:streamGenerateContent`; fall through to the
		// non-stream path on the raw bytes so we don't fail a parse that
		// the non-stream decoder would handle.
		var resp geminiResponse
		if err := json.Unmarshal(raw, &resp); err == nil && len(resp.Candidates) > 0 {
			out.Stream = false
			if resp.ModelVersion != "" {
				out.Model = resp.ModelVersion
			}
			for ci, c := range resp.Candidates {
				if c.Content == nil {
					continue
				}
				// Default the WIRE role, then map — the same shape the SSE
				// fold below uses. Mapping first and overriding the result
				// expresses the identical rule a second way, and a rule
				// written twice is a rule that drifts. The default matters
				// because geminiRoleToCanonical("") is user, which is
				// backwards on the response side.
				role := c.Content.Role
				if role == "" {
					role = "model"
				}
				out.Messages = append(out.Messages, core.Message{
					Role: geminiRoleToCanonical(role),
					// This arm handles a whole non-SSE response body that
					// reached the stream normalizer, so plain json: paths
					// address it — no frame indirection.
					Content:      geminiPartsToBlocks(c.Content.Parts, locator.JoinPath("candidates", ci)+".content.parts"),
					FinishReason: c.FinishReason,
				})
			}
			if len(resp.Candidates) > 0 {
				out.FinishReason = resp.Candidates[0].FinishReason
			}
			if resp.UsageMetadata != nil {
				out.Usage = geminiUsageToCanonical(resp.UsageMetadata)
			}
			return out, nil
		}
		return out, fmt.Errorf("gemini-generate: no events decoded: %w", core.ErrUnsupported)
	}

	for _, idx := range order {
		// order indices are appended exactly when the map entry is
		// created with a non-nil state, so the lookup always hits.
		st := candidates[idx]
		role := st.role
		if role == "" {
			role = "model"
		}
		msg := core.Message{Role: geminiRoleToCanonical(role), FinishReason: st.finishReason}
		msg.Content = append(msg.Content, st.blocks...)
		out.Messages = append(out.Messages, msg)
		if out.FinishReason == "" {
			out.FinishReason = st.finishReason
		}
	}
	out.Usage = usage
	if totalFrames > 0 {
		out.Confidence = float64(recognizedFrames) / float64(totalFrames)
	}
	return out, nil
}
