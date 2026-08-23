// Replicate's `output` field is polymorphic — a string, an array of
// strings, or an object — and classifying it is the whole difficulty of this
// codec. It lives apart from the envelope parsing because the two change for
// unrelated reasons, and because this is where every defect in this area has
// been: concatenating before classifying, unwrapping whitespace that was not
// wrapping, eliding a run that had already swallowed a word.

package codecs

import (
	"strings"
	"unicode"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// replicateOutputBlocks projects Replicate's polymorphic `output` field.
//
// Classification happens PER ELEMENT, where the branch that matched is
// still known. Concatenating first and classifying the result cannot work:
// one leading space defeats a prefix test and lets a whole payload through
// as prose, a two-element array fuses two URLs into one that never existed,
// and a language-model answer that merely begins with a URL is swallowed
// whole. Knowing the branch also yields the path, so inline bytes are
// addressable instead of degrading to "never there".
func replicateOutputBlocks(out json.RawMessage) []core.ContentBlock {
	if len(out) == 0 || string(out) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(out, &s); err == nil {
		return []core.ContentBlock{replicateElementBlock(s, "output")}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(out, &arr); err == nil {
		blocks := make([]core.ContentBlock, 0, len(arr))
		var text strings.Builder
		for i, item := range arr {
			var v string
			if err := json.Unmarshal(item, &v); err != nil {
				// Not a string. Dropping it silently is the no-silent-drop
				// rule broken: the element vanishes while Confidence still
				// reports a good parse, so nothing downstream can tell an
				// empty answer from a shape this codec does not read yet.
				// Every other codec in this package renders an unrecognised
				// part instead, bounded.
				if t := text.String(); t != "" {
					blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: t})
					text.Reset()
				}
				blocks = append(blocks, core.ContentBlock{
					Type: core.ContentText,
					// A constant prefix, so nothing here is caller-controlled, and
					// payloadSafeRaw carries the bound. Wrapping it in
					// payloadSafeText as well elided the content at 96 chars,
					// which made the marker name a shape it could no longer
					// show.
					Text: "[unrecognised replicate output element: " + payloadSafeRaw(item) + "]",
				})
				continue
			}
			// JSON null unmarshals into a string without error, so it
			// arrives here indistinguishable from "". Dropping it fuses the
			// text either side into one utterance that was never sent —
			// round 8's class, entered through the one value the type
			// system cannot tell apart.
			if string(item) == "null" {
				if t := text.String(); t != "" {
					blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: t})
					text.Reset()
				}
				blocks = append(blocks, core.ContentBlock{
					Type: core.ContentText,
					Text: "[unrecognised replicate output element: null]",
				})
				continue
			}
			if b, isMedia := replicateMediaElement(v, locator.JoinPath("output", i)); isMedia {
				// Text accumulated before this artifact stays its own
				// block, in order — a caption is not part of the image.
				if t := text.String(); t != "" {
					blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: t})
					text.Reset()
				}
				blocks = append(blocks, b)
				continue
			}
			// Adjacent text elements are one utterance: token streaming
			// arrives as an array of fragments.
			text.WriteString(v)
		}
		if t := text.String(); t != "" {
			blocks = append(blocks, core.ContentBlock{Type: core.ContentText, Text: t})
		}
		return blocks
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err == nil {
		for _, key := range []string{"text", "answer", "completion", "message"} {
			if v, ok := obj[key].(string); ok && v != "" {
				return []core.ContentBlock{replicateElementBlock(v, "output."+key)}
			}
		}
	}
	return nil
}

// replicateMediaElement reports whether a whole element IS a media
// reference. The test is on the entire value, never a prefix: prose that
// merely mentions a URL is prose.
func replicateMediaElement(v, path string) (core.ContentBlock, bool) {
	// Trim first, then require the WHOLE trimmed value to be the reference.
	// Testing a prefix lets a leading space smuggle a payload through as
	// prose; testing for any whitespace at all rejects that same payload
	// for having one. Neither is the question — the question is whether
	// the element is a reference and nothing else.
	// trimIgnorable, not TrimSpace: this codec pre-filters before anything in
	// media.go runs, so its own normalisation IS the classifier here. Using a
	// narrower one meant the two classes closed at the class site stayed open
	// on this path — and this fall-through is the verbatim declared-text
	// channel, so a payload landed whole in a scanned block rather than in a
	// bounded reference field.
	v = locator.TrimIgnorable(v)
	// Whitespace anywhere means this is not a whole-value reference.
	//
	// An earlier version tried to unwrap RFC-2045 line wrapping here, to
	// keep a wrapped payload out of the prose channel. No discriminator
	// separates a wrapped URI from a URI with prose after it: segment
	// lengths and the base64 alphabet both accept `…QUJD\nDone` exactly as
	// they accept a real wrap, and East Asian prose carries no ASCII space
	// to key on. Getting it wrong DELETES the model's words from the
	// record, and two adjacent URIs fuse into a third that never existed.
	// Silently losing output is worse than classifying a hypothetical
	// wrap as prose — no measured wire wraps a JSON string value — so the
	// payload is bounded in the prose path instead, where the question is
	// answerable without guessing intent.
	// unicode.IsSpace, not an ASCII set: an ideographic space or a line
	// separator is whitespace to the writer and invisible to a byte set,
	// so the value would reach the media switch with a sentence attached
	// and the sentence would be lost.
	if strings.IndexFunc(v, unicode.IsSpace) >= 0 {
		return core.ContentBlock{}, false
	}
	switch {
	case locator.HasSchemeFold(v, "data:"):
		return mediaBlock(inlineOrExternal(v, path, "")), true
	case locator.HasSchemeFold(v, "https://"), locator.HasSchemeFold(v, "http://"):
		// Provider-hosted and expiring; the gateway never fetched it.
		return mediaBlock(externalMedia(mimeFromURLPath(v), v, "")), true
	}
	return core.ContentBlock{}, false
}

func replicateElementBlock(v, path string) core.ContentBlock {
	if b, ok := replicateMediaElement(v, path); ok {
		return b
	}
	// Verbatim. `output` is a DECLARED text field — the model's own answer,
	// not structure this codec rendered — and media.go states the rule for
	// those: a declared text part preserves its content verbatim by design.
	//
	// Two attempts were made to bound a base64 run inside it and both
	// deleted the model's words. Unwrapping line breaks to reclassify the
	// value fused `…QUJD\nDone` into the payload, because D, o, n and e are
	// base64 characters. Eliding the run instead moved the same
	// undecidability one function over: the run has to cross line breaks to
	// cover a wrapped payload, and once it does it eats any base64-alphabet
	// word that follows. Neither is a tuning problem — nothing distinguishes
	// "payload the classifier missed" from "base64 the model chose to
	// emit", and guessing costs real answers.
	//
	// So the bound is not attempted here. What arrives is what the model
	// said, which is exactly what a compliance scanner should see; the
	// bounded renderings elsewhere in this package cover text this codec
	// FABRICATES, which is a different thing.
	return core.ContentBlock{Type: core.ContentText, Text: v}
}

// mimeFromURLPath infers a mime from a URL's file extension so a generated
// artifact renders as its modality rather than as an opaque file.
func mimeFromURLPath(u string) string {
	i := strings.LastIndexByte(u, '.')
	if i < 0 {
		return ""
	}
	ext := strings.ToLower(u[i:])
	if j := strings.IndexAny(ext, "?#"); j >= 0 {
		ext = ext[:j]
	}
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	default:
		return ""
	}
}
