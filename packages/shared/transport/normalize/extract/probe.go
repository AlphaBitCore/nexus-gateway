package extract

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// DetectChatShape iterates KnownChatSpecs and returns the best-scoring
// detection on a request body. Confidence semantics:
//
//   - Locator hit (non-empty array at Locator path, OR — for legacy
//     single-prompt specs — non-empty string at ContentPath) : +0.4
//   - For each message in the array: role present at RolePath +0.15
//     (capped contribution +0.30), content extractable at ContentPath
//     +0.15 (capped +0.30) → up to 0.6 from message inspection
//   - Each SignatureField present at top level: +0.05 (capped +0.20)
//
// Maximum confidence per spec = 1.0. Scores are NOT normalised across
// specs; the spec ordering in KnownChatSpecs is the tiebreaker (earlier
// = higher specificity).
//
// "Extract as much as possible": when a spec matches, the detection
// fills Model / System / Tools too, regardless of confidence level.
// The caller decides whether to use the detection based on Confidence
// alone.
func DetectChatShape(body []byte) ChatDetection {
	if !gjson.ValidBytes(body) {
		return ChatDetection{}
	}
	parsed := gjson.ParseBytes(body)

	var best ChatDetection
	for _, spec := range KnownChatSpecs {
		d := scoreChatSpec(parsed, spec)
		if d.Confidence > best.Confidence {
			best = d
		}
	}
	return best
}

// ScoreChatSpec is the public single-spec scorer used by per-adapter
// Normalizers (Tier 1) that already know which spec to assume — they
// skip the multi-spec iteration in DetectChatShape and call this
// directly with their preferred ChatSpec. Returns a ChatDetection
// with Confidence in [0, 1] and all best-effort extracted fields
// populated regardless of confidence value; the adapter inspects
// Confidence and decides whether to claim or return ErrUnsupported.
func ScoreChatSpec(body []byte, spec ChatSpec) ChatDetection {
	if !gjson.ValidBytes(body) {
		return ChatDetection{}
	}
	return scoreChatSpec(gjson.ParseBytes(body), spec)
}

// ScoreResponseSpec is the response-side equivalent of ScoreChatSpec.
// Auto-routes SSE vs single-json per the spec's StreamFraming.
func ScoreResponseSpec(body []byte, spec ChatResponseSpec) ChatDetection {
	isSSE := LooksLikeSSE(body)
	if spec.StreamFraming == "single-json" && isSSE {
		return ChatDetection{IsResponse: true, IsStream: true}
	}
	if spec.StreamFraming == "sse-event-data" && !isSSE {
		return ChatDetection{IsResponse: true}
	}
	return scoreResponseSpec(body, spec, isSSE)
}

func scoreChatSpec(parsed gjson.Result, spec ChatSpec) ChatDetection {
	d := ChatDetection{SpecID: spec.ID}

	// Locator probe
	var msgs []gjson.Result
	if spec.Locator != "" {
		arr := parsed.Get(spec.Locator)
		if arr.IsArray() {
			msgs = arr.Array()
			if len(msgs) > 0 {
				d.Confidence += 0.4
			}
		}
	} else if spec.ContentPath != "" {
		// Legacy single-prompt spec: locator is the content path itself.
		v := parsed.Get(spec.ContentPath)
		if v.Type == gjson.String && v.String() != "" {
			d.Confidence += 0.4
			d.UserPrompts = []string{v.String()}
			d.MessageRoles = []string{"user"}
			d.MessageContents = []string{v.String()}
		}
	}

	// Per-message role + content probe
	if len(msgs) > 0 {
		roleHits := 0
		contentHits := 0
		for _, m := range msgs {
			role := ""
			if spec.RolePath != "" {
				if r := m.Get(spec.RolePath); r.Type == gjson.String {
					role = r.String()
					roleHits++
				}
			}
			content := extractMessageContent(m, spec.ContentPath, spec.Shape)
			if content != "" {
				contentHits++
			}
			d.MessageRoles = append(d.MessageRoles, role)
			d.MessageContents = append(d.MessageContents, content)
			if role == "user" {
				d.UserPrompts = append(d.UserPrompts, content)
			}
		}
		// Each kind capped at 0.3 — divide by message count, multiply
		// hit count, cap.
		if n := len(msgs); n > 0 {
			roleScore := 0.3 * float64(roleHits) / float64(n)
			contentScore := 0.3 * float64(contentHits) / float64(n)
			d.Confidence += roleScore + contentScore
		}
	}

	// Signature field probes (existence, additive)
	sigHits := 0
	for _, f := range spec.SignatureFields {
		if v := parsed.Get(f); v.Exists() {
			sigHits++
		}
	}
	if sigHits > 0 {
		bonus := 0.05 * float64(sigHits)
		if bonus > 0.2 {
			bonus = 0.2
		}
		d.Confidence += bonus
	}

	// Free wins: model / system / tools
	if spec.ModelPath != "" {
		if v := parsed.Get(spec.ModelPath); v.Type == gjson.String {
			d.Model = v.String()
		}
	}
	if spec.SystemPath != "" {
		if v := parsed.Get(spec.SystemPath); v.Type == gjson.String {
			d.System = v.String()
		}
	}
	if spec.ToolsPath != "" {
		if v := parsed.Get(spec.ToolsPath); v.IsArray() && len(v.Array()) > 0 {
			d.ToolsRaw = json.RawMessage(v.Raw)
		}
	}

	// Cap at 1.0
	if d.Confidence > 1.0 {
		d.Confidence = 1.0
	}
	return d
}

// extractMessageContent reads a message's content field per Shape rules
// and returns a flat text projection. Returns "" when content is empty
// or doesn't match the expected shape.
func extractMessageContent(msg gjson.Result, path string, shape ContentShape) string {
	v := msg.Get(path)
	if !v.Exists() {
		return ""
	}
	switch shape {
	case ContentShapeString:
		if v.Type == gjson.String {
			return v.String()
		}
		// Some specs ship content as a single object — fall through
		// and try blob extraction.
	case ContentShapeBlockArray:
		// Anthropic Messages: content is array of blocks, each {type, text}
		// (plus tool_use / tool_result variants we skip for text projection).
		if !v.IsArray() {
			return ""
		}
		var b strings.Builder
		for _, block := range v.Array() {
			if t := block.Get("type"); t.String() == "text" {
				if txt := block.Get("text"); txt.Type == gjson.String {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(txt.String())
				}
			}
		}
		return b.String()
	case ContentShapeStringArray:
		// ChatGPT-web: content.parts is array of strings (or single string).
		if !v.IsArray() {
			if v.Type == gjson.String {
				return v.String()
			}
			return ""
		}
		var b strings.Builder
		for _, p := range v.Array() {
			if p.Type == gjson.String {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.String())
			}
		}
		return b.String()
	case ContentShapeNestedTextArray:
		// Gemini: parts[]{text}
		if !v.IsArray() {
			return ""
		}
		var b strings.Builder
		for _, p := range v.Array() {
			if txt := p.Get("text"); txt.Type == gjson.String {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(txt.String())
			}
		}
		return b.String()
	}
	return ""
}

// DetectResponseShape iterates KnownResponseSpecs and returns the
// best-scoring response detection. Auto-routes SSE vs single-json
// based on byte sniff; for SSE responses the spec's AccumulatorRule
// dictates how to fold the stream into a single document before
// applying AssistantTextPath.
func DetectResponseShape(body []byte) ChatDetection {
	isSSE := LooksLikeSSE(body)

	var best ChatDetection
	for _, spec := range KnownResponseSpecs {
		// Skip specs whose framing doesn't match the body shape.
		if spec.StreamFraming == "single-json" && isSSE {
			continue
		}
		if spec.StreamFraming == "sse-event-data" && !isSSE {
			continue
		}
		d := scoreResponseSpec(body, spec, isSSE)
		if d.Confidence > best.Confidence {
			best = d
		}
	}
	return best
}

func scoreResponseSpec(body []byte, spec ChatResponseSpec, isSSE bool) ChatDetection {
	d := ChatDetection{
		SpecID:     spec.ID,
		IsResponse: true,
		IsStream:   isSSE,
	}

	// Assemble document — for SSE we run the accumulator first, for
	// single-json we parse the body directly.
	var doc gjson.Result
	var accumulatedText string
	if isSSE {
		switch spec.AccumulatorRule {
		case "json-patch":
			acc := NewJSONPatchAccumulator()
			patchFrames := 0
			_ = WalkSSE(body, func(event, data string) error {
				// Only "delta" event frames carry patch ops; skip
				// keepalives / [DONE] / metadata.
				if event != "" && event != "delta" {
					return nil
				}
				if data == "" || data == "[DONE]" {
					return nil
				}
				if err := acc.ApplyJSON([]byte(data)); err == nil {
					patchFrames++
				}
				return nil
			})
			final, _ := json.Marshal(acc.State())
			doc = gjson.ParseBytes(final)
			// Frame-count bonus mirroring the concat-text branch —
			// a stream we actually accumulated patches from is
			// almost certainly chatgpt-web shape.
			if patchFrames > 0 {
				d.Confidence += 0.3
				if patchFrames >= 3 {
					d.Confidence += 0.2
				}
			}
		case "concat-text":
			// Walk frames; for each data line, parse as JSON and try
			// to extract a text delta at the SPEC-SPECIFIC delta path.
			// Frames that don't carry this spec's expected shape don't
			// count toward the spec's frame counter — that's how we
			// avoid mistaking an Anthropic SSE for an OpenAI SSE.
			var textBuf strings.Builder
			deltaPath := spec.StreamDeltaPath
			if deltaPath == "" {
				// Defensive fallback if a spec author forgot to set it:
				// try the OpenAI canonical path. This keeps probe from
				// crashing but shouldn't fire in practice.
				deltaPath = "choices.0.delta.content"
			}
			matchedFrames := 0
			totalFrames := 0
			_ = WalkSSE(body, func(event, data string) error {
				if data == "" || data == "[DONE]" {
					return nil
				}
				if !gjson.Valid(data) {
					return nil
				}
				totalFrames++
				parsed := gjson.Parse(data)
				if v := parsed.Get(deltaPath); v.Type == gjson.String && v.String() != "" {
					textBuf.WriteString(v.String())
					matchedFrames++
				}
				// Capture model / finish_reason / usage on any frame
				// (these may live on header / tail frames of the spec).
				if v := parsed.Get(spec.ModelPath); v.Type == gjson.String && d.Model == "" {
					d.Model = v.String()
				}
				if v := parsed.Get(spec.FinishReasonPath); v.Type == gjson.String && d.FinishReason == "" {
					d.FinishReason = v.String()
				}
				if v := parsed.Get(spec.UsagePath); v.IsObject() && d.UsageRaw == nil {
					d.UsageRaw = json.RawMessage(v.Raw)
				}
				return nil
			})
			accumulatedText = textBuf.String()
			// Synthesize a doc with _accumulated key so signature
			// probes below still work uniformly.
			syn := map[string]any{
				"_accumulated":    accumulatedText,
				"_frame_count":    totalFrames,
				"_assistant_text": accumulatedText,
			}
			synBytes, _ := json.Marshal(syn)
			doc = gjson.ParseBytes(synBytes)
			// Confidence scaling: only frames matching the spec's
			// delta path count. A stream that produces 0 matched
			// frames (the spec doesn't recognize the body at all)
			// gets no bonus — and AssistantTextPath below will see
			// an empty _accumulated, dropping confidence overall.
			if matchedFrames > 0 {
				d.Confidence += 0.3
				if matchedFrames >= 3 {
					d.Confidence += 0.2
				}
			}
		default:
			return d
		}
	} else {
		// single-json
		if !gjson.ValidBytes(body) {
			return d
		}
		doc = gjson.ParseBytes(body)
	}

	// Assistant text probe (the strongest single signal for response specs)
	if spec.AssistantTextPath != "" {
		v := doc.Get(spec.AssistantTextPath)
		if v.Type == gjson.String && v.String() != "" {
			d.AssistantText = v.String()
			d.Confidence += 0.5
		}
	}
	if d.AssistantText == "" && accumulatedText != "" {
		d.AssistantText = accumulatedText
		d.Confidence += 0.3
	}

	// Signature fields (presence, additive)
	sigHits := 0
	for _, f := range spec.SignatureFields {
		if v := doc.Get(f); v.Exists() {
			sigHits++
		}
	}
	if sigHits > 0 {
		bonus := 0.1 * float64(sigHits)
		if bonus > 0.3 {
			bonus = 0.3
		}
		d.Confidence += bonus
	}

	// Free wins for non-stream specs (single doc has everything).
	if !isSSE {
		if v := doc.Get(spec.ModelPath); v.Type == gjson.String && d.Model == "" {
			d.Model = v.String()
		}
		if v := doc.Get(spec.FinishReasonPath); v.Type == gjson.String && d.FinishReason == "" {
			d.FinishReason = v.String()
		}
		if v := doc.Get(spec.UsagePath); v.IsObject() && d.UsageRaw == nil {
			d.UsageRaw = json.RawMessage(v.Raw)
		}
	}

	if d.Confidence > 1.0 {
		d.Confidence = 1.0
	}
	// Synthesize a single assistant message for the caller.
	if d.AssistantText != "" {
		d.MessageRoles = []string{"assistant"}
		d.MessageContents = []string{d.AssistantText}
	}
	return d
}
