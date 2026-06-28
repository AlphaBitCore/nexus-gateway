package extract

import (
	"github.com/goccy/go-json"
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
// fills Model / Tools too, regardless of confidence level.
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
// The only response framing Tier 2 recognises is the consumer-web
// JSON-Patch SSE stream (standard-API response wires are folded by the
// Tier-1 codecs), so non-SSE bodies score zero. The signature gate is
// strict: at least one signature key must appear in some frame
// (anti-theft for key-missed traffic — see scoreResponseSpec). Adapter
// callers that carry host evidence go through
// ScoreResponseSpecAdapterKeyed instead.
func ScoreResponseSpec(body []byte, spec ChatResponseSpec) ChatDetection {
	if !LooksLikeSSE(body) {
		return ChatDetection{IsResponse: true}
	}
	return scoreResponseSpec(body, spec, false)
}

// ScoreResponseSpecAdapterKeyed scores a response body for a caller
// that has ALREADY resolved the producing adapter by host / URL /
// content-type (NormalizeForAdapter). Host evidence satisfies the
// identification gate, so a stream variant that carries none of the
// signature keys in any frame (e.g. a chatgpt-web delta-encoding
// stream without the resume-token or marker telemetry frames) still
// scores on patch coverage. Key-missed Tier-2 traffic keeps the strict
// gate via ScoreResponseSpec.
func ScoreResponseSpecAdapterKeyed(body []byte, spec ChatResponseSpec) ChatDetection {
	if !LooksLikeSSE(body) {
		return ChatDetection{IsResponse: true}
	}
	return scoreResponseSpec(body, spec, true)
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

	// Free wins: model / tools
	if spec.ModelPath != "" {
		if v := parsed.Get(spec.ModelPath); v.Type == gjson.String {
			d.Model = v.String()
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
// best-scoring response detection. Only the consumer-web JSON-Patch
// SSE framing is recognised; non-SSE response bodies fall straight
// through to the Tier-1 codecs (which the Registry has already tried)
// or Tier 3 verbatim.
func DetectResponseShape(body []byte) ChatDetection {
	var best ChatDetection
	for _, spec := range KnownResponseSpecs {
		d := ScoreResponseSpec(body, spec)
		if d.Confidence > best.Confidence {
			best = d
		}
	}
	return best
}

// scoreResponseSpec folds a JSON-Patch SSE stream (the chatgpt-web
// consumer framing) and scores it with coverage semantics:
//
//   - A frame is a PATCH CANDIDATE when its event name is empty or
//     "delta" and its data JSON carries the patch-op `v` field. Named
//     protocol chrome ("delta_encoding", resume tokens, message
//     markers, [DONE]) never enters the denominator.
//   - Confidence = appliedFrames / candidateFrames — the fraction of
//     the stream's own patch ops the accumulator replayed. A truncated
//     or partially corrupt stream scores proportionally lower.
//   - Identification gate: at least one SignatureField key must appear
//     in some RAW frame's data JSON — probed at the top level AND
//     nested one hop under the patch value (`v.<field>`): the
//     delta-encoding stream variant carries conversation_id only
//     inside the `v` envelope of its seed frames, never top-level.
//     Without a signature hit the stream is some other producer's
//     patch protocol and scores zero — coverage alone must not claim
//     foreign streams. adapterKeyed callers carry host evidence that
//     satisfies the gate by itself, so even a signature-free stream
//     variant scores on coverage.
//
// The assistant text is extracted from the accumulated document at
// AssistantTextPath; extraction is best-effort and independent of the
// confidence score.
func scoreResponseSpec(body []byte, spec ChatResponseSpec, adapterKeyed bool) ChatDetection {
	d := ChatDetection{
		SpecID:     spec.ID,
		IsResponse: true,
		IsStream:   true,
	}

	acc := NewJSONPatchAccumulator()
	appliedFrames := 0
	candidateFrames := 0
	sigHits := map[string]bool{}
	_ = WalkSSE(body, func(event, data string) error {
		if data == "" || data == "[DONE]" || !gjson.Valid(data) {
			return nil
		}
		parsed := gjson.Parse(data)
		// Signature fields probe RAW frames — a key found in any
		// frame's data JSON counts once, top-level or nested under the
		// patch value envelope (`v.<field>`).
		for _, f := range spec.SignatureFields {
			if parsed.Get(f).Exists() || parsed.Get("v."+f).Exists() {
				sigHits[f] = true
			}
		}
		// Model probe — consumer-web streams carry the model identifier
		// as frame metadata (never in the assembled patch document), so
		// it is read off RAW frames at the spec's declared paths. First
		// hit wins; the stream repeats the same value on every frame.
		if d.Model == "" {
			for _, p := range spec.ModelFramePaths {
				if v := parsed.Get(p); v.Type == gjson.String && v.String() != "" {
					d.Model = v.String()
					break
				}
			}
		}
		if event != "" && event != "delta" {
			return nil
		}
		if !parsed.Get("v").Exists() {
			return nil
		}
		candidateFrames++
		if err := acc.ApplyJSON([]byte(data)); err == nil {
			appliedFrames++
		}
		return nil
	})

	if candidateFrames == 0 || (!adapterKeyed && len(sigHits) == 0) {
		return d
	}
	d.Confidence = float64(appliedFrames) / float64(candidateFrames)

	final, _ := json.Marshal(acc.State())
	doc := gjson.ParseBytes(final)
	if spec.AssistantTextPath != "" {
		if v := doc.Get(spec.AssistantTextPath); v.Type == gjson.String && v.String() != "" {
			d.AssistantText = v.String()
		}
	}
	// Synthesize a single assistant message for the caller.
	if d.AssistantText != "" {
		d.MessageRoles = []string{"assistant"}
		d.MessageContents = []string{d.AssistantText}
	}
	return d
}
