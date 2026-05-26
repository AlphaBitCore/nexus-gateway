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
	contents := gjson.GetBytes(body, "contents")
	if !contents.Exists() {
		return nil, 0, traffic.ErrUnknownSchema
	}

	out := body
	segIdx := 0
	written := 0
	var err error

	// 1) systemInstruction.parts[].text (optional).
	sys := gjson.GetBytes(out, "systemInstruction.parts")
	if sys.IsArray() {
		parts := sys.Array()
		for pIdx := range parts {
			t := parts[pIdx].Get("text")
			if !t.Exists() || t.Type != gjson.String {
				continue
			}
			if segIdx >= len(content.Segments) {
				return out, written, nil
			}
			p := fmt.Sprintf("systemInstruction.parts.%d.text", pIdx)
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
			if t := partList[pIdx].Get("text"); t.Exists() && t.Type == gjson.String {
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
