// A Gemini `part` is the union every modality arrives through — text,
// inline bytes, a Files-API reference, a function call — and reading one is a
// separate job from reading the request and response envelopes around it.
//
// It is also where the no-silent-drop rule is enforced: an unrecognised part
// is NAMED rather than skipped, because a part this build does not know about
// is exactly the thing an operator needs to see.

package codecs

import (
	"crypto/sha1"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// UnmarshalJSON decodes a part and, only when nothing known matched,
// retains the raw bytes so the projection can name what it could not read.
// A recognised part costs exactly one decode, as before — the common path
// pays nothing for the rare one.
func (p *geminiPart) UnmarshalJSON(b []byte) error {
	type alias geminiPart
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*p = geminiPart(a)
	if p.Text == nil && p.InlineData == nil && p.FileData == nil &&
		p.FunctionCall == nil && p.FunctionResponse == nil {
		p.unmatched = append(json.RawMessage(nil), b...)
	}
	return nil
}

// geminiPartsToBlocks projects a Gemini parts[] slice into canonical
// ContentBlocks. Parts may carry text, inlineData (binary), functionCall,
// or functionResponse — each maps to a distinct ContentType.
// base is the gjson path of the parts array (e.g. "contents.0.parts"); an
// empty base means no positional context and media degrades to absent
// rather than pointing at the wrong offset.
func geminiPartsToBlocks(parts []geminiPart, base string) []core.ContentBlock {
	out := make([]core.ContentBlock, 0, len(parts))
	for i, p := range parts {
		path := locator.JoinPath(base, i)
		switch {
		case p.FunctionCall != nil:
			callID := p.FunctionCall.ID
			if callID == "" {
				args := "{}"
				if len(p.FunctionCall.Args) > 0 {
					if raw, err := json.Marshal(p.FunctionCall.Args); err == nil {
						args = string(raw)
					}
				}
				callID = geminiFallbackCallID(p.FunctionCall.Name, args, base, i)
			}
			tu := &core.ToolUse{
				CallID:           callID,
				Name:             p.FunctionCall.Name,
				Input:            p.FunctionCall.Args,
				ThoughtSignature: p.ThoughtSignature,
			}
			out = append(out, core.ContentBlock{Type: core.ContentToolUse, ToolUse: tu})
		case p.FunctionResponse != nil:
			tr := &core.ToolResult{CallID: p.FunctionResponse.ID}
			// Gemini's functionResponse.response is documented as a struct.
			// We project it to a string by serialising — downstream hooks
			// see the same text regardless of provider.
			if len(p.FunctionResponse.Response) > 0 {
				if b, err := json.Marshal(p.FunctionResponse.Response); err == nil {
					tr.Output = string(b)
				}
			}
			out = append(out, core.ContentBlock{Type: core.ContentToolResult, ToolResult: tr})
		case p.InlineData != nil:
			// Modality comes from the declared mime, so audio and PDFs stop
			// being labelled images.
			out = append(out, mediaBlock(capturedMedia(
				p.InlineData.MimeType,
				p.InlineData.Data,
				locator.JSON(locator.JoinSuffix(path, "inlineData.data")),
			)))
		case p.FileData != nil:
			out = append(out, mediaBlock(geminiFileDataMedia(p.FileData.MimeType, p.FileData.FileURI)))
		case p.Text != nil:
			ct := core.ContentText
			if p.Thought {
				ct = core.ContentReasoning
			}
			out = append(out, core.ContentBlock{Type: ct, Text: *p.Text})
		case len(p.unmatched) > 0:
			// No known field matched. Emitting nothing here is what made
			// fileData parts disappear without a trace, so name the keys
			// instead — the reader sees that something was carried and what
			// kind of thing it was.
			out = append(out, core.ContentBlock{
				Type: core.ContentText,
				Text: "[unrecognised gemini part: " + geminiPartKeys(p.unmatched) + "]",
			})
		}
	}
	return out
}

// geminiFallbackCallID is collision-free for same-name/same-args calls by
// binding the deterministic digest to the content/response coordinate and
// Part position.
func geminiFallbackCallID(name, _ string, coordinate string, part int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s\x00%s\x00part:%d", name, coordinate, part)))
	return "call_" + fmt.Sprintf("%x", h)[:10]
}

// geminiFileDataMedia classifies a fileData part.
//
// Scheme is not the discriminator: a Files-API URI is https, exactly like a
// public YouTube link, so keying on "https://" labels provider-held media
// as content we never fetched — inverted custody on Google's own
// recommended mechanism for large media. The Files service is identified
// by its host and path shape instead.
func geminiFileDataMedia(mime, uri string) *core.MediaRef {
	switch {
	case uri == "":
		return &core.MediaRef{Modality: modalityFromMime(mime), Mime: mime, Source: core.MediaAbsent}
	case strings.HasPrefix(uri, "gs://"), strings.HasPrefix(uri, "files/"):
		return providerRefMedia(mime, uri, "")
	case isGoogleFilesHost(uri):
		return providerRefMedia(mime, uri, "")
	default:
		// A third-party URL — a YouTube video input is the documented case.
		return externalMedia(mime, uri, "")
	}
}

// isGoogleFilesHost reports whether uri is served by Google's Files API.
// The check is on the parsed HOST, not a substring: a third-party URL can
// embed the Google hostname in its path, and labelling that "held by the
// provider" is the same class of untruth the scheme check produced.
func isGoogleFilesHost(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host != "generativelanguage.googleapis.com" && host != "aiplatform.googleapis.com" {
		return false
	}
	return strings.Contains(u.Path, "/files/")
}

// geminiPartKeys lists the top-level keys of an unrecognised part so the
// marker names what was dropped. Only unmatched parts reach it.
func geminiPartKeys(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return "unparseable"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return payloadSafeText("", strings.Join(keys, ", "))
}
