package specutil

// contentgate.go — what a wire will not carry, declared rather than branched.
//
// Several OpenAI-compatible providers accept the OpenAI request shape but not
// all of its content parts. Forwarding an unsupported part gets the caller the
// vendor's deserializer talking about itself — "invalid part type: file",
// "unknown variant `image_url`" — naming neither the attachment nor what would
// work. Each adapter declares the kinds its wire carries; the shared code owns
// the walk and the wiring.
//
// BOTH DOORS. EncodeRequest is the cross-format door; RewriteNative is the
// same-spec one, which is the path an OpenAI-shaped request to an
// OpenAI-compatible provider actually takes. A gate on only the first passes
// every unit test and changes nothing in production — that shipped once.

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// ContentPolicy is one adapter's statement about the content parts its wire can
// carry. A part kind absent from Allow is refused with the reason Deny gives.
type ContentPolicy struct {
	// Allow names the canonical content part kinds this wire accepts. "text" is
	// implicit and never checked — a wire that cannot take text is not a chat
	// wire.
	Allow map[string]bool
	// Deny maps a refused part kind to the caller-facing explanation. A kind
	// with no entry gets a generic message.
	Deny map[string]string
	// InlineOnlyImageURL refuses an image_url whose URL is not a data: URL, for
	// wires that read images from inline bytes and do not fetch. Empty means the
	// wire fetches (or takes no images at all, in which case Allow decides).
	InlineOnlyImageURL string
	// ImageFormats names the image media types this wire decodes. Empty means
	// the wire's formats are unknown to us and every declared type is forwarded.
	//
	// Declared rather than discovered, because the refusals are worth owning.
	// Measured across the catalog: an unsupported image earns the caller
	// "You uploaded an unsupported image. Please make sure your image has of one
	// the following formats: [...]" from one provider, "Unsupported MIME type:
	// image/svg+xml" from another, and — from a provider that content-sniffs
	// rather than reading the declared type — "unsupported image format:
	// text/plain; charset=utf-8", which names neither the file the caller sent
	// nor anything they can do about it. Five wires, five vocabularies, one
	// question: is this format on the list.
	ImageFormats map[string]bool
	// InlineTextDocuments turns a textual `file` part into a text part instead
	// of refusing it, and names the wire's limitation for the audit trail.
	//
	// Lossless in meaning rather than a capability we invented: the bytes ARE
	// characters. The largest refusal class measured across the catalog — a
	// PDF-only file part answers "Invalid file data … Expected a base64-encoded
	// data URL with an application/pdf MIME type" to a markdown attachment it
	// would have read fine as text.
	//
	// Declared-textual types ONLY. A PDF is not characters; Allow/Deny judges it.
	InlineTextDocuments string
	// TextPartLeadsAudio moves a text part ahead of a lone audio attachment on
	// wires whose audio models only read the attachment when text comes first.
	// Empty means the wire does not care about the order.
	//
	// Measured, deterministic, 6/6 against 0/6 across both OpenAI audio models:
	// `[text, input_audio]` returns the transcript; `[input_audio, text]`
	// returns HTTP 200 and "I'm sorry, but I can't transcribe audio directly."
	// Identical bytes and model.
	//
	// The 200 is why this is worth owning: the failing form is indistinguishable
	// from a model that cannot do audio, in the response and in the traffic
	// event alike. Attachment-first is the natural way to compose the request.
	TextPartLeadsAudio string
}

// PolicyFor resolves the content policy for ONE call target.
//
// A function of the target because the divergence is per MODEL, not per
// adapter: within one OpenAI-compatible family a model reads images while its
// sibling does not, and another REQUIRES audio. A policy fixed at construction
// is wrong for most of that adapter's models whichever way it is set.
type PolicyFor func(provcore.CallTarget) ContentPolicy

// UniformPolicy is the resolver for a wire that carries the same content parts
// on every one of its models.
func UniformPolicy(p ContentPolicy) PolicyFor {
	return func(provcore.CallTarget) ContentPolicy { return p }
}

// GateContent wraps a codec so every request through either door is checked
// against the target's policy before the inner codec sees it.
func GateContent(inner provcore.SchemaCodec, resolve PolicyFor) provcore.SchemaCodec {
	return gatedCodec{SchemaCodec: inner, resolve: resolve}
}

type gatedCodec struct {
	provcore.SchemaCodec
	resolve PolicyFor
}

func (g gatedCodec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte,
	target provcore.CallTarget) (provcore.EncodeResult, error) {
	body, rewrites, err := g.check(canonicalBody, target)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	res, err := g.SchemaCodec.EncodeRequest(endpoint, body, target)
	res.Rewrites = append(res.Rewrites, rewrites...)
	return res, err
}

func (g gatedCodec) RewriteNative(shape typology.WireShape, nativeBody []byte,
	target provcore.CallTarget, stream bool) (provcore.EncodeResult, error) {
	body, rewrites, err := g.check(nativeBody, target)
	if err != nil {
		return provcore.EncodeResult{}, err
	}
	res, err := g.SchemaCodec.RewriteNative(shape, body, target, stream)
	res.Rewrites = append(res.Rewrites, rewrites...)
	return res, err
}

// partsToken is the cheapest thing that must appear in a body carrying any
// structured content part. A message whose content is a plain string — the
// overwhelming majority — never reaches the walk.
var partsToken = []byte(`"type"`)

// check walks the content parts, refusing what the wire cannot carry and
// converting what it can carry in another form. It returns the body to send —
// the input unchanged when nothing was converted — and the rewrites to surface
// as x-nexus-coerced, which is how a conversion tells the caller it happened.
func (g gatedCodec) check(body []byte, target provcore.CallTarget) ([]byte, []string, error) {
	// The pre-scan runs before the policy is resolved: a body whose messages
	// carry no structured content part at all — the overwhelming majority — is
	// not a request any policy has an opinion about, and resolving one for it
	// would be work done on the hot path to reach the same answer.
	if !bytes.Contains(body, partsToken) {
		return body, nil, nil
	}
	policy := g.resolve(target)
	var err error
	var rewrites []string
	// Collected as paths and applied after the walk: mutating the body under
	// the iterator would invalidate the very indices being walked.
	type inlined struct {
		path string
		text string
	}
	var toInline []inlined
	// Messages whose content array has to be reordered so a text part leads.
	// Collected for the same reason as toInline: the walk cannot mutate under
	// itself.
	var toLeadWithText []string
	type relabelled struct{ path, dataURL string }
	var toRelabel []relabelled
	gjson.GetBytes(body, "messages").ForEach(func(mi, msg gjson.Result) bool {
		if policy.TextPartLeadsAudio != "" && needsTextLead(msg.Get("content")) {
			toLeadWithText = append(toLeadWithText, "messages."+mi.String()+".content")
		}
		msg.Get("content").ForEach(func(ci, part gjson.Result) bool {
			kind := part.Get("type").String()
			if kind == "" || kind == "text" {
				return true
			}
			if kind == "file" {
				if text, ok := undeclaredTextBytes(part); ok {
					// On a wire whose document part cannot carry text at all
					// (OpenAI's is PDF-only), relabelling is not enough: the
					// part would reach the wire as a text/plain FILE and 400
					// exactly as octet-stream did. It has to become a text
					// part, and that decision is made below in the same pass —
					// which is why this inlines here rather than deferring.
					//
					// Measured: without this, 17 OpenAI models kept answering
					// "Invalid file data … Expected a base64-encoded data URL
					// with an application/pdf MIME type" after the relabel
					// landed. The relabel is applied AFTER the walk, so the
					// inline decision below had already seen octet-stream and
					// declined.
					if policy.InlineTextDocuments != "" {
						name := part.Get("file.filename").String()
						if name == "" {
							name = "(application/octet-stream)"
						}
						toInline = append(toInline, inlined{
							path: "messages." + mi.String() + ".content." + ci.String(),
							text: "Attached document " + name + ":\n\n" + text,
						})
						rewrites = append(rewrites, "octet_stream_inlined_as_text")
						return true
					}
					// Tier 1: the bytes ARE text, only the label was wrong.
					// Relabelling is lossless and lets every downstream path —
					// the inline-as-text conversion below, and the wires that
					// carry a document natively — treat it as what it is.
					toRelabel = append(toRelabel, relabelled{
						path: "messages." + mi.String() + ".content." + ci.String() +
							".file.file_data",
						dataURL: "data:text/plain;base64," +
							base64.StdEncoding.EncodeToString([]byte(text)),
					})
					rewrites = append(rewrites, "octet_stream_relabelled_as_text")
				} else if e := undeclaredDocumentType(part); e != nil {
					err = e
					return false
				}
			}
			if kind == "file" && policy.InlineTextDocuments != "" {
				if text, name, ok := textDocument(part); ok {
					toInline = append(toInline, inlined{
						path: "messages." + mi.String() + ".content." + ci.String(),
						text: "Attached document " + name + ":\n\n" + text,
					})
					rewrites = append(rewrites, "file_part_inlined_as_text")
					return true
				}
			}
			if !policy.Allow[kind] {
				err = errContentRefused(policy.denyReason(kind))
				return false
			}
			if kind == "image_url" {
				u := part.Get("image_url.url").String()
				if policy.InlineOnlyImageURL != "" && u != "" && !strings.HasPrefix(u, "data:") {
					err = errContentRefused(policy.InlineOnlyImageURL)
					return false
				}
				if e := policy.checkImageFormat(u); e != nil {
					err = e
					return false
				}
			}
			return true
		})
		return err == nil
	})
	if err != nil {
		return nil, nil, err
	}
	for _, in := range toInline {
		var serr error
		body, serr = sjson.SetBytes(body, in.path,
			map[string]any{"type": "text", "text": in.text})
		if serr != nil {
			// The part stays as it was; the wire will refuse it and say so.
			// Silently dropping the attachment would be worse than a refusal.
			return nil, nil, errContentRefused(policy.InlineTextDocuments)
		}
	}
	for _, rl := range toRelabel {
		var serr error
		if body, serr = sjson.SetBytes(body, rl.path, rl.dataURL); serr != nil {
			return nil, nil, errContentRefused("the attachment could not be relabelled")
		}
	}
	for _, path := range toLeadWithText {
		reordered, ok := leadWithText(gjson.GetBytes(body, path))
		if !ok {
			continue
		}
		var serr error
		if body, serr = sjson.SetRawBytes(body, path, reordered); serr != nil {
			// Leave the order alone rather than send a half-rewritten array.
			// The wire will answer as it would have; that is worse than the
			// reorder and much better than a malformed body.
			continue
		}
		rewrites = append(rewrites, "audio_part_moved_after_text")
	}
	return body, rewrites, nil
}

// needsTextLead reports whether a content array carries exactly one audio part
// with no text part ahead of it.
//
// Restricted to ONE attachment on purpose. With two attachments the order is the
// caller's meaning — "compare the first recording with the second" — and moving
// parts would change what they asked. With one attachment and an instruction,
// which order they arrive in is not something the caller expressed an opinion
// about, and one of the two orders does not work.
func needsTextLead(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	audio, text, attachments, textBeforeAudio := 0, 0, 0, false
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text":
			text++
			if audio == 0 {
				textBeforeAudio = true
			}
		case "input_audio":
			audio++
			attachments++
		case "image_url", "file", "video_url", "audio":
			attachments++
		}
		return true
	})
	return audio == 1 && attachments == 1 && text > 0 && !textBeforeAudio
}

// leadWithText returns the content array with the first text part moved to the
// front, everything else keeping its relative order.
func leadWithText(content gjson.Result) ([]byte, bool) {
	var lead gjson.Result
	var rest []gjson.Result
	found := false
	content.ForEach(func(_, part gjson.Result) bool {
		if !found && part.Get("type").String() == "text" {
			lead, found = part, true
			return true
		}
		rest = append(rest, part)
		return true
	})
	if !found {
		return nil, false
	}
	out := make([]byte, 0, len(content.Raw))
	out = append(out, '[')
	out = append(out, lead.Raw...)
	for _, p := range rest {
		out = append(out, ',')
		out = append(out, p.Raw...)
	}
	return append(out, ']'), true
}

// undeclaredTextBytes reports the characters of a `file` part declared
// application/octet-stream whose bytes are actually text.
//
// The label is wrong and the bytes are markdown, so refusing outright was
// measured taking a document Gemini 3.x had been READING and turning it into
// our own 400.
//
// Strict on purpose: a small PDF is printable enough to fool a naive test.
// Valid UTF-8 is necessary and not sufficient — a NUL byte or any known binary
// magic disqualifies it, and anything not clearing all three stays a refusal.
func undeclaredTextBytes(part gjson.Result) (string, bool) {
	data := part.Get("file.file_data").String()
	if !strings.HasPrefix(data, "data:") {
		return "", false
	}
	media, b64, ok := ParseDataURL(data)
	if !ok || media != "application/octet-stream" {
		return "", false
	}
	raw, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return "", false
	}
	for _, magic := range binaryMagics {
		if bytes.HasPrefix(raw, magic) {
			return "", false
		}
	}
	return string(raw), true
}

// binaryMagics are the leading bytes of formats that can pass a UTF-8 check yet
// are not text. Presence here costs a refusal the caller can act on; absence
// costs a relabel that puts mojibake in front of a model, so the list errs
// toward refusing.
var binaryMagics = [][]byte{
	[]byte("%PDF-"), []byte("PK\x03\x04"), []byte("GIF8"), []byte("RIFF"),
	[]byte("\x89PNG"), []byte("\xff\xd8\xff"), []byte("BM"), []byte("II*\x00"),
	[]byte("MM\x00*"), []byte("OggS"), []byte("\x1f\x8b"),
}

// undeclaredDocumentType refuses a document sent as application/octet-stream:
// the caller declined to say what the file is, and no wire routes bytes whose
// type it was not told. Measured — Gemini answers "Unsupported MIME type:
// application/octet-stream", true and unhelpful; a browser sends octet-stream
// for any extension it does not recognise, so the caller usually does not know
// they omitted anything.
//
// Reached only after undeclaredTextBytes declines the payload, so what lands
// here is bytes we could not confirm are text.
func undeclaredDocumentType(part gjson.Result) error {
	data := part.Get("file.file_data").String()
	if !strings.HasPrefix(data, "data:") {
		return nil
	}
	media, _, ok := ParseDataURL(data)
	if !ok || media != "application/octet-stream" {
		return nil
	}
	return errContentRefused("the attachment is declared application/octet-stream, which " +
		"says only that its type is unknown — declare what it actually is (text/markdown, " +
		"text/plain, application/json, application/pdf) so it can be carried correctly")
}

// textDocument reports the characters of a `file` part whose declared type is
// textual, along with a name for it.
//
// The declared type and nothing else, for the same reason IsTextualMediaType
// gives: sniffing would have to decide whether a printable payload is a text
// file or the source of a binary one, and a small PDF is printable enough to
// fool it.
func textDocument(part gjson.Result) (text, name string, ok bool) {
	data := part.Get("file.file_data").String()
	if !strings.HasPrefix(data, "data:") {
		return "", "", false // an id or URL reference carries no bytes to inline
	}
	media, b64, parsed := ParseDataURL(data)
	if !parsed || !IsTextualMediaType(media) {
		return "", "", false
	}
	raw, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil || !utf8.Valid(raw) {
		// Declared textual, and the bytes are not. Inlining would put
		// replacement characters in front of the model and earn a confident
		// answer about nothing; leave it for the wire to refuse.
		return "", "", false
	}
	name = part.Get("file.filename").String()
	if name == "" {
		name = "(" + media + ")"
	}
	return string(raw), name, true
}

// checkImageFormat refuses an inline image whose declared type this wire does
// not decode, naming the type that was sent and the ones that would work.
//
// Only inline images are judged. A fetched URL carries no declared type here —
// the wire discovers it when it fetches — and refusing on a filename extension
// would be a guess about someone else's server.
func (p ContentPolicy) checkImageFormat(url string) error {
	if len(p.ImageFormats) == 0 || !strings.HasPrefix(url, "data:") {
		return nil
	}
	media, _, ok := ParseDataURL(url)
	if !ok || p.ImageFormats[media] {
		// An unparseable data URL is not this check's business; the codec that
		// decodes it will say so in its own terms.
		return nil
	}
	accepted := make([]string, 0, len(p.ImageFormats))
	for f := range p.ImageFormats {
		accepted = append(accepted, f)
	}
	sort.Strings(accepted) // a stable message; map order would churn the text
	return errContentRefused("this model reads " + strings.Join(accepted, ", ") +
		", and this image is " + media +
		" — convert it before sending, or route to a model on a wire that reads it")
}

func (p ContentPolicy) denyReason(kind string) string {
	if r, ok := p.Deny[kind]; ok {
		return r
	}
	return "this provider does not accept a " + kind + " content part"
}

func errContentRefused(detail string) error {
	return &provcore.ProviderError{
		Status:  http.StatusBadRequest,
		Code:    provcore.CodeInvalidRequest,
		Type:    "nexus_field_unsupported",
		Message: "nexus: " + detail,
	}
}
