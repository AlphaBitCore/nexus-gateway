package codecs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"strings"
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

	type candidateState struct {
		role         string
		text         strings.Builder
		thoughtText  strings.Builder
		finishReason string
		toolUses     []*core.ToolUse
		toolResults  []*core.ToolResult
		images       []*core.BinaryRef
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
		for _, c := range chunk.Candidates {
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
			for _, p := range c.Content.Parts {
				switch {
				case p.Text != nil:
					if p.Thought {
						st.thoughtText.WriteString(*p.Text)
					} else {
						st.text.WriteString(*p.Text)
					}
					sawAny = true
				case p.FunctionCall != nil:
					st.toolUses = append(st.toolUses, &core.ToolUse{
						CallID: p.FunctionCall.ID,
						Name:   p.FunctionCall.Name,
						Input:  p.FunctionCall.Args,
					})
					sawAny = true
				case p.FunctionResponse != nil:
					tr := &core.ToolResult{CallID: p.FunctionResponse.ID}
					if len(p.FunctionResponse.Response) > 0 {
						if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
							tr.Output = string(b)
						}
					}
					st.toolResults = append(st.toolResults, tr)
					sawAny = true
				case p.InlineData != nil:
					st.images = append(st.images, &core.BinaryRef{
						ContentType: p.InlineData.MimeType,
						Size:        int64(len(p.InlineData.Data)),
						SHA256:      stableHashHint(p.InlineData.Data),
					})
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
			for _, c := range resp.Candidates {
				if c.Content == nil {
					continue
				}
				// Same response-side role-default rule as normalizeResponse:
				// empty role on the response side means assistant, not user.
				role := geminiRoleToCanonical(c.Content.Role)
				if c.Content.Role == "" {
					role = core.RoleAssistant
				}
				out.Messages = append(out.Messages, core.Message{
					Role:         role,
					Content:      geminiPartsToBlocks(c.Content.Parts),
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
		if t := st.thoughtText.String(); t != "" {
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentReasoning, Text: t})
		}
		if t := st.text.String(); t != "" {
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentText, Text: t})
		}
		for _, tu := range st.toolUses {
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentToolUse, ToolUse: tu})
		}
		for _, tr := range st.toolResults {
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentToolResult, ToolResult: tr})
		}
		for _, img := range st.images {
			msg.Content = append(msg.Content, core.ContentBlock{Type: core.ContentImageRef, ImageRef: img})
		}
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
