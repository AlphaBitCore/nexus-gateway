// Package geminiweb implements the gemini-web traffic adapter for
// browser-side traffic to gemini.google.com (the consumer Gemini chat
// product). This is distinct from the public Gemini API
// (`generativelanguage.googleapis.com`, handled by the gemini adapter).
//
// gemini.google.com's internal wire format is **undocumented** and
// historically uses Google's obfuscated RPC framing — POST bodies are
// typically `application/x-www-form-urlencoded` with a single `f.req`
// parameter carrying a JSON-stringified array of arrays, and streaming
// responses arrive with a leading `)]}'` anti-XSSI prefix followed by
// chunked JSON arrays. This adapter is therefore defensive:
//
//  1. JSON-shaped request bodies are parsed via the same shapes the
//     adapter recognises across the wave (top-level `prompt`,
//     Anthropic-Messages-API-style `messages`, or Gemini-API-style
//     `contents`), so if Google migrates gemini.google.com to a JSON
//     API the adapter degrades gracefully.
//  2. Form-encoded RPC bodies are best-effort: the adapter extracts
//     long string-typed leaf nodes from the JSON-stringified `f.req`
//     payload as Segments, conservatively avoiding short tokens
//     (UUIDs, locale codes, ids) by length-filtering. This catches
//     the user's prompt without depending on the exact RPC shape.
//  3. Anything the adapter cannot parse confidently lands in Extra so
//     compliance hooks doing defence-in-depth scans still see it.
//
// **Limitation**: the form-encoded extraction is heuristic (walks all
// string-typed leaf nodes ≥ 16 chars). The package is registered and the
// InterceptionDomain row seeded so audit captures the connection event
// regardless; missing prompt text only weakens hook scanning.
//
// ── Production wire format (verified from live traffic capture) ──────────
//
// REQUEST — POST /_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate
//
//	Content-Type: application/x-www-form-urlencoded
//	Body:         f.req=<url-encoded>&at=<csrf-token>&...
//
//	URL-decoded f.req value is a JSON array: [null, "<inner-JSON-string>"]
//	The inner JSON string, when parsed, has shape:
//
//	  [
//	    ["<user-prompt>", 0, null, null, null, null, 0],  // index [0] = prompt tuple
//	    ["en"],                                            // index [1] = locale
//	    ["c_<conv-id>", "r_<resp-id>", "rc_<rc-id>", ...], // index [2] = context
//	    "<session-state-token>",                           // index [3] = large auth blob
//	    ...
//	  ]
//
//	User's message = outer[1] → parse as JSON → inner[0][0] (first string of first tuple).
//
//	Current heuristic limitation: walkHeuristicSegments collects ALL strings ≥ 16 chars,
//	which picks up the session-state token at inner[3] (~2–4 KB), UUIDs, and URL strings
//	alongside the actual prompt. Hooks therefore receive noisy Segments that include the
//	user's message but cannot reliably identify it.
//
// RESPONSE — POST /_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate
//
//	Content-Type: text/plain (NOT text/event-stream; NOT standard SSE)
//	Transfer-Encoding: chunked
//
//	Google's proprietary Chunked-Length-Prefix streaming format:
//
//	  )]}'                   ← XSSI anti-prefix (one-time, on first chunk only)
//	  \n
//	  177\n                  ← decimal byte-length of next JSON frame
//	  [["wrb.fr",null,"...session-init..."]]\n
//	  1511\n                 ← byte-length
//	  [["wrb.fr",null,"<inner-JSON-string>"]]\n
//	  ...
//
//	Each frame is a JSON array. The outer structure is always:
//	  [["wrb.fr", null, "<inner-JSON-string>"]]
//
//	The inner JSON string (index [0][2]), when parsed, carries the AI response:
//	  [null,
//	   ["c_<conv-id>", "r_<resp-id>"],  // index [1] = conversation ref
//	   null, null,
//	   [                                 // index [4] = candidates array
//	     ["rc_<rc-id>",
//	      ["<cumulative-text>"],         // index [4][0][1][0] = FULL text so far
//	      null, null, null, null, null, null,
//	      [1 or 2],                      // index [4][0][8]: 1=in-progress, 2=done
//	      ...]
//	   ],
//	   ...]
//
//	Key insight: the text at [4][0][1][0] is CUMULATIVE (not a delta). Each frame
//	carries the full response up to that point. The final frame with state=2 (done)
//	contains the complete assistant reply.
//
//	The compliance proxy stores the entire raw HTTP response body (all frames
//	concatenated) in traffic_event.response_body. Hooks receive the full byte
//	stream, not individual parsed frames.
//
// REQUEST — POST /_/BardChatUi/data/batchexecute
//
//	Same f.req form-encoding, but batchexecute is a generic RPC multiplexer.
//	Its f.req outer shape is a batch list: [[["<method-id>","<args>",null,"generic"]]].
//	The method-id is an obfuscated short string (e.g. "GPRiHf", "CNgdBe", "jGArJ").
//
//	Observed method calls and their payloads (from live traffic capture):
//	  "GPRiHf" / args="[]"            → response="[]" (session keepalive / ping)
//	  "CNgdBe" / args=[2,["en"],0]    → response=[null,null,null,[[1,"General"]]]
//	                                     (language / persona settings query)
//	  "jGArJ"  / args=[[1,1,1,1,1,0,1,1,1],30] → response=[] (UI capability flags)
//
//	FINDING: batchexecute contains NO user chat content. All calls observed are
//	session management, UI state sync, and settings queries. The user's actual
//	message and the AI's reply travel exclusively through StreamGenerate.
//	Capturing batchexecute is therefore low-value for AI compliance analysis;
//	it adds event volume without adding useful segments for hook inspection.
package geminiweb

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

const adapterID = "gemini-web"

// jsonRequestKnownKeys lists known top-level fields when the request
// body happens to be plain JSON (forward-compat path). Anything else
// falls into Extra.
var jsonRequestKnownKeys = []string{
	"prompt", "messages", "contents", "systemInstruction",
	"system_instruction", "generationConfig", "safetySettings",
	"tools", "toolConfig", "model", "stream", "session_id",
	"conversation_id", "client_metadata", "metadata",
}

// minHeuristicTextLen is the minimum string length the form-encoded
// RPC walker treats as candidate prompt text. Set conservatively to
// skip UUIDs (32+), locale codes ("en-US"), small ids, and JS-side
// internal tokens. Real user prompts are typically longer than this.
const minHeuristicTextLen = 16

// Adapter implements gemini-web extraction.
type Adapter struct{}

// ID returns the canonical adapter identifier.
func (a *Adapter) ID() string { return adapterID }

// Configure is a no-op — gemini-web has no per-domain configuration.
func (a *Adapter) Configure(_ map[string]any) error { return nil }

// ExtractRequest parses a gemini.google.com request body. Tries three
// shapes in order:
//   - Plain JSON (forward-compat): handles top-level `prompt`,
//     `messages` (Anthropic-Messages-API-style), or `contents`
//     (Gemini-API-style).
//   - Form-encoded `f.req=<json-string>` (current obfuscated RPC):
//     parses the stringified payload and heuristically extracts
//     long string-typed leaf nodes as Segments.
//   - Anything else → ErrUnknownSchema.
func (a *Adapter) ExtractRequest(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	// (1) JSON shape.
	if gjson.ValidBytes(body) {
		return extractJSONRequest(body)
	}

	// (2) Form-encoded RPC shape.
	if isFormEncodedBody(body) {
		return extractFormEncodedRequest(body)
	}

	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

func extractJSONRequest(body []byte) (traffic.NormalizedContent, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	var segments []string
	hadKnown := false

	// `prompt` (claude-web-style).
	if p := root.Get("prompt"); p.Type == gjson.String && p.Str != "" {
		segments = append(segments, p.Str)
		hadKnown = true
	}

	// `messages` (Anthropic-Messages-API-style).
	if msgs := root.Get("messages"); msgs.IsArray() {
		hadKnown = true
		msgs.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.Type == gjson.String {
				segments = append(segments, content.Str)
			} else if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str == "text" {
						segments = append(segments, part.Get("text").Str)
					}
					return true
				})
			}
			return true
		})
	}

	// `contents` (Gemini-API-style).
	if contents := root.Get("contents"); contents.IsArray() {
		hadKnown = true
		contents.ForEach(func(_, c gjson.Result) bool {
			c.Get("parts").ForEach(func(_, part gjson.Result) bool {
				if t := part.Get("text"); t.Type == gjson.String {
					segments = append(segments, t.Str)
				}
				return true
			})
			return true
		})
	}

	if !hadKnown {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	meta := map[string]string{}
	if model := root.Get("model"); model.Type == gjson.String {
		meta["model"] = model.Str
	}

	return traffic.NormalizedContent{
		Segments: segments,
		Metadata: meta,
		Extra:    traffic.CollectExtra(body, jsonRequestKnownKeys),
	}, nil
}

// isFormEncodedBody returns true when the body looks like
// `key=value&key=value` URL-encoded form data. Heuristic — the proxy
// has the Content-Type header for confirmation but we do not depend
// on it because some clients omit it for streaming uploads.
func isFormEncodedBody(body []byte) bool {
	if len(body) > 1024 {
		// Quick reject for obvious non-form payloads (large bodies are
		// unlikely to be form-encoded in the gemini.google.com path
		// since prompts arrive inside f.req).
		body = body[:1024]
	}
	s := string(body)
	if !strings.Contains(s, "=") {
		return false
	}
	// Form keys are conventionally url-safe; the gemini.google.com RPC
	// uses `f.req`, `at`, `bl`, `_reqid`, `rt` keys.
	return strings.HasPrefix(s, "f.req=") || strings.Contains(s, "&f.req=") ||
		strings.HasPrefix(s, "at=") || strings.Contains(s, "&at=")
}

// extractFormEncodedRequest parses the f.req parameter. Its value is a
// JSON-stringified array; the user's prompt typically lives inside as
// a long string-typed leaf. The walk is heuristic: collect all string
// leaves longer than minHeuristicTextLen, dedupe, and emit them as
// Segments. This catches the prompt without depending on the exact
// RPC shape.
//
// Current behaviour is heuristic; the planned deterministic extraction
// (parse outer[1] → inner[0][0]) is described as item 1 in the package
// doc comment.
func extractFormEncodedRequest(body []byte) (traffic.NormalizedContent, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	freq := values.Get("f.req")
	if freq == "" {
		return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
	}

	// f.req's outer shape is `[null,"<inner-json>"]`. Parse the inner.
	outer := gjson.Parse(freq)
	var inner string
	switch {
	case outer.IsArray() && outer.Get("1").Type == gjson.String:
		inner = outer.Get("1").Str
	case outer.Type == gjson.String:
		inner = outer.Str
	default:
		// Fall back: walk the outer payload directly.
		return walkHeuristicSegments([]byte(freq), body), nil
	}

	if !gjson.Valid(inner) {
		return walkHeuristicSegments([]byte(freq), body), nil
	}
	return walkHeuristicSegments([]byte(inner), body), nil
}

// walkHeuristicSegments recursively walks a JSON value and collects
// string-typed leaves whose length is at least minHeuristicTextLen. The
// `originalBody` is captured into Extra["form_body_preview"] so audit
// can re-examine the raw bytes if needed.
func walkHeuristicSegments(payload, originalBody []byte) traffic.NormalizedContent {
	root := gjson.ParseBytes(payload)
	seen := map[string]bool{}
	var segments []string
	var visit func(v gjson.Result)
	visit = func(v gjson.Result) {
		switch {
		case v.Type == gjson.String:
			if len(v.Str) >= minHeuristicTextLen && !seen[v.Str] {
				seen[v.Str] = true
				segments = append(segments, v.Str)
			}
		case v.IsArray() || v.IsObject():
			v.ForEach(func(_, sub gjson.Result) bool {
				visit(sub)
				return true
			})
		}
	}
	visit(root)

	// Audit fallback: a truncated preview of the form body so a hook
	// doing defence-in-depth still has the bytes available.
	preview := originalBody
	if len(preview) > 4096 {
		preview = preview[:4096]
	}
	extra := map[string]string{
		"form_body_preview": string(preview),
	}

	return traffic.NormalizedContent{
		Segments: segments,
		Extra:    extra,
	}
}

// ExtractResponse handles a buffered response. Successful responses
// from gemini.google.com use the `)]}'` XSSI prefix followed by
// chunked JSON arrays — the streaming pipeline strips the prefix and
// feeds chunks via ExtractStreamChunk. ExtractResponse here covers
// only buffered error envelopes:
//
//	{"error":{"code":N,"message":"<text>"}}
//	{"detail":"<text>"}
func (a *Adapter) ExtractResponse(_ context.Context, body []byte, _ string) (traffic.NormalizedContent, error) {
	if len(body) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	body = stripXSSIPrefix(body)
	if !gjson.ValidBytes(body) {
		return traffic.NormalizedContent{}, traffic.ErrMalformed
	}
	if errMsg := gjson.GetBytes(body, "error.message"); errMsg.Type == gjson.String && errMsg.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{errMsg.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	if detail := gjson.GetBytes(body, "detail"); detail.Type == gjson.String && detail.Str != "" {
		return traffic.NormalizedContent{
			Segments: []string{detail.Str},
			Metadata: map[string]string{"error": "true"},
		}, nil
	}
	return traffic.NormalizedContent{}, traffic.ErrUnknownSchema
}

// xssiPrefix is the Google anti-XSSI prefix prepended to every chunk
// of a streaming RPC response. The streaming pipeline can call
// stripXSSIPrefix on each chunk before invoking ExtractStreamChunk to
// deliver clean JSON.
const xssiPrefix = ")]}'"

func stripXSSIPrefix(body []byte) []byte {
	body = trimASCIIWhitespace(body)
	if len(body) >= len(xssiPrefix) && string(body[:len(xssiPrefix)]) == xssiPrefix {
		body = body[len(xssiPrefix):]
		body = trimASCIIWhitespace(body)
	}
	return body
}

func trimASCIIWhitespace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// ExtractStreamChunk parses one chunk of a gemini.google.com streaming
// response. The pipeline strips the XSSI prefix; we receive a JSON
// array. Walk the array heuristically for long string leaves — this
// catches the assistant's text without depending on the exact RPC
// shape, mirroring the request-side fallback.
//
// The fail-open posture: unrecognised JSON shapes return empty rather
// than erroring.
//
// Current behaviour is heuristic. Deterministic extraction would parse
// chunk[0][2] → [4][0][1][0] (cumulative text), gated by state flag at
// [4][0][8] (1=in-progress, 2=done).
func (a *Adapter) ExtractStreamChunk(_ context.Context, chunk []byte, _ string) (traffic.NormalizedContent, error) {
	if len(chunk) == 0 {
		return traffic.NormalizedContent{}, nil
	}
	chunk = stripXSSIPrefix(chunk)
	if !gjson.ValidBytes(chunk) {
		return traffic.NormalizedContent{}, nil
	}
	root := gjson.ParseBytes(chunk)
	seen := map[string]bool{}
	var segments []string
	var visit func(v gjson.Result)
	visit = func(v gjson.Result) {
		switch {
		case v.Type == gjson.String:
			if len(v.Str) >= minHeuristicTextLen && !seen[v.Str] {
				seen[v.Str] = true
				segments = append(segments, v.Str)
			}
		case v.IsArray() || v.IsObject():
			v.ForEach(func(_, sub gjson.Result) bool {
				visit(sub)
				return true
			})
		}
	}
	visit(root)
	return traffic.NormalizedContent{Segments: segments}, nil
}

// DetectRequestMeta returns the gemini-web RequestMeta. Provider is
// "gemini-web". Authentication uses cookies, not Bearer tokens, so
// ApiKey* fields stay empty.
func (a *Adapter) DetectRequestMeta(_ *http.Request, body []byte) traffic.RequestMeta {
	meta := traffic.RequestMeta{Provider: "gemini-web"}
	if !gjson.ValidBytes(body) {
		return meta
	}
	if model := gjson.GetBytes(body, "model"); model.Type == gjson.String {
		meta.Model = model.Str
	}
	return meta
}

// DetectResponseUsage returns the non-LLM-tier sentinel because
// gemini.google.com (consumer product) does not return token usage.
func (a *Adapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
}

// RewriteRequestBody is unsupported. The form-encoded RPC body has
// signed `at` (anti-CSRF) and other client-state tokens that we
// cannot regenerate.
func (a *Adapter) RewriteRequestBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}

// RewriteResponseBody is unsupported.
func (a *Adapter) RewriteResponseBody(_ context.Context, body []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return body, 0, traffic.ErrRewriteUnsupported
}
