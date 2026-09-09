package gemini

import (
	"context"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// RewriteRequestBody reverses ExtractRequest for the Google Gemini
// generateContent API. Iteration order matches the extractor:
// first every text slot under systemInstruction.parts[].text, then every
// contents[i].parts[j].text. Non-text parts (inlineData, functionCall,
// etc.) are left untouched.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	// The embedding wires, dispatched on the same shape check the extractor uses
	// so the two cannot disagree about which bodies take this path.
	if isEmbedBody(body) {
		return rewriteEmbedRequest(body, content)
	}
	contents := gjson.GetBytes(body, "contents")
	// IsArray, not merely Exists: the extractor's ForEach walks an OBJECT's
	// values too, so a `"contents": {...}` body yields segments — while this
	// side calls .Array(), which wraps a non-array in a one-element slice whose
	// `parts` does not exist. That produced zero writes and a nil error: the
	// pipeline recorded the request as redacted and forwarded it untouched.
	// The response side already fails closed this way; this one did not.
	if !contents.Exists() || !contents.IsArray() {
		return nil, 0, traffic.ErrUnknownSchema
	}

	out := body
	segIdx := 0
	written := 0
	var err error

	// 1) The system instruction, under every spelling the extractor reads.
	// Reading only the camelCase key here is what made a snake_case request
	// start its segment index at the first user message.
	for _, key := range systemInstructionKeys {
		sys := gjson.GetBytes(out, key)
		if !sys.IsArray() {
			continue
		}
		parts := sys.Array()
		for pIdx := range parts {
			t := parts[pIdx].Get("text")
			if !t.Exists() || t.Type != gjson.String {
				continue
			}
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("%s.%d.text", key, pIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("gemini: rewrite %s: %w", p, err)
			}
			segIdx++
			written++
		}
	}

	// 2) contents[i].parts[j]:
	//   - parts[].text                              → write back
	//   - parts[].functionResponse.response.result  → write back when
	//     extractor consumed it (the result-wrapper convention)
	//   - parts[].functionResponse.response (string) → write back the
	//     unwrapped variant
	// Non-text / functionCall / inlineData parts are left untouched.
	contentsArr := gjson.GetBytes(out, "contents").Array()
	for cIdx := range contentsArr {
		parts := contentsArr[cIdx].Get("parts")
		if !parts.IsArray() {
			continue
		}
		partList := parts.Array()
		for pIdx := range partList {
			// THE BRANCH ORDER MIRRORS THE EXTRACTOR EXACTLY, first match wins:
			// text, then functionCall, then functionResponse. Single-sourcing
			// the reasoning PREDICATE is not enough — where it is consulted in
			// the walk is itself part of the pairing contract.
			//
			// In particular the reasoning test belongs INSIDE the text branch,
			// because that is where the extractor asks it: it classifies TEXT,
			// not the whole part. Hoisting it to a whole-part skip drops a
			// `thought:true` part that carries a functionResponse — which the
			// extractor DOES put on Segments — and every later slot shifts.
			if t := partList[pIdx].Get("text"); t.Exists() && t.Type == gjson.String {
				// Thinking text is on ReasoningSegments, not Segments, so
				// writing into it consumes a slot belonging to the next real
				// message and leaves the last one unredacted. It would also
				// corrupt the thought text Gemini requires echoed back verbatim
				// across turns.
				if isReasoningPart(partList[pIdx]) {
					continue
				}
				if segIdx >= len(content.Segments) {
					return out, written, nil
				}
				p := fmt.Sprintf("contents.%d.parts.%d.text", cIdx, pIdx)
				out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
				if err != nil {
					return nil, written, fmt.Errorf("gemini: rewrite %s: %w", p, err)
				}
				segIdx++
				written++
				continue
			}
			// A functionCall part is consumed by the extractor into
			// ToolCallSegments and produces NO Segment, so it must consume no
			// slot here either. Without this branch a part carrying both a
			// functionCall and a functionResponse — where the extractor stops at
			// the call — had its functionResponse rewritten with a slot that
			// belonged to a later message.
			if fc := partList[pIdx].Get("functionCall"); fc.IsObject() {
				continue
			}
			if fr := partList[pIdx].Get("functionResponse"); fr.Exists() {
				resp := fr.Get("response")
				switch {
				case resp.Type == gjson.String:
					if segIdx >= len(content.Segments) {
						return out, written, nil
					}
					p := fmt.Sprintf("contents.%d.parts.%d.functionResponse.response", cIdx, pIdx)
					out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
					if err != nil {
						return nil, written, fmt.Errorf("gemini: rewrite %s: %w", p, err)
					}
					segIdx++
					written++
				case resp.IsObject():
					if r := resp.Get("result"); r.Exists() && r.Type == gjson.String {
						if segIdx >= len(content.Segments) {
							return out, written, nil
						}
						p := fmt.Sprintf("contents.%d.parts.%d.functionResponse.response.result", cIdx, pIdx)
						out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
						if err != nil {
							return nil, written, fmt.Errorf("gemini: rewrite %s: %w", p, err)
						}
						segIdx++
						written++
					}
				}
			}
		}
	}
	return out, written, nil
}

// RewriteResponseBody reverses ExtractResponse for Gemini generateContent
// non-streaming responses (candidates[].content.parts[].text).
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, content traffic.NormalizedContent) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return nil, 0, traffic.ErrMalformed
	}
	candidates := gjson.GetBytes(body, "candidates")
	if !candidates.Exists() || !candidates.IsArray() {
		return nil, 0, traffic.ErrUnknownSchema
	}
	out := body
	segIdx := 0
	written := 0
	var err error

	candList := candidates.Array()
	for cIdx := range candList {
		parts := candList[cIdx].Get("content.parts")
		if !parts.IsArray() {
			continue
		}
		partList := parts.Array()
		for pIdx := range partList {
			t := partList[pIdx].Get("text")
			if !t.Exists() || t.Type != gjson.String {
				continue
			}
			// Same slot rule as the request side, and consulted in the same
			// place — inside the text branch, because that is where the
			// extractor asks it. Thinking text is on ReasoningSegments, not
			// Segments; writing into one returned unredacted assistant text to
			// the client.
			if isReasoningPart(partList[pIdx]) {
				continue
			}
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("candidates.%d.content.parts.%d.text", cIdx, pIdx)
			out, err = sjson.SetBytes(out, p, content.Segments[segIdx])
			if err != nil {
				return nil, written, fmt.Errorf("gemini: rewrite response %s: %w", p, err)
			}
			segIdx++
			written++
		}
	}
	return out, written, nil
}
