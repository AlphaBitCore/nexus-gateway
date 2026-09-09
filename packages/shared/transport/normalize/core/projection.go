package core

import (
	"github.com/goccy/go-json"
	"strings"
)

// TextProjection returns the flat list of text fragments hooks scan for
// content matches. The projection is intentionally narrow:
//
//   - AI kinds: one entry per ContentBlock whose Type is ContentText,
//     ContentToolResult (text payload) or ContentReasoning. System / user /
//     assistant / tool roles all flow into the same flat list — regex-based
//     hooks do not need to distinguish them.
//
//     Reasoning used to be excluded unless a hook opted in, on the reasoning
//     that it is "internal model thinking". It is not internal: the gateway
//     encodes it onto the wire as `delta.reasoning_content` and the client
//     displays it. A card number the model writes while thinking reached the
//     caller unscanned, and the opt-in that would have covered it had no UI,
//     no admin API, no documentation and no persisted column — nobody could
//     turn it on. What is delivered is what is scanned.
//
//   - HTTP kinds: BodyView.Text is returned as a single entry. Form
//     fields are flattened to "key=value" lines. SSE frames project one
//     entry per frame (verbatim DataText, or the re-marshaled Data tree)
//     so content hooks scan stream payloads the same as inline bodies.
//     A JSON tree projects as its compact re-marshaled document.
//
//   - http-binary / unsupported / redacted payloads: empty slice.
//
// One projection serves every regex-based hook in shared/hooks. There is no
// per-hook variant: a knob deciding which delivered text a compliance rule may
// see is a knob that can be set wrong, and the only setting that was ever
// correct is "all of it".
func (p *NormalizedPayload) TextProjection() []string {
	if p == nil || p.Redacted {
		return nil
	}
	if p.Kind.IsAI() {
		return aiTextProjection(p)
	}
	if p.Kind.IsHTTP() {
		return httpTextProjection(p)
	}
	return nil
}

func aiTextProjection(p *NormalizedPayload) []string {
	// KindAIEmbedding payloads carry text in Inputs (not Messages).
	// Include all non-empty input strings so content-scanning hooks
	// (PII detector, keyword filter, safety scanner) can inspect embedding
	// inputs without needing kind-specific awareness.
	if p.Kind == KindAIEmbedding {
		out := make([]string, 0, len(p.Inputs))
		for _, s := range p.Inputs {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	out := make([]string, 0, max(8, 2*len(p.Messages)))
	for _, m := range p.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case ContentText, ContentRefusal:
				// A refusal is assistant text the caller reads. It differs from
				// ContentText only in which wire slot it came from, which is a
				// rewriter's concern, not a scanner's.
				if b.Text != "" {
					out = append(out, b.Text)
				}
			case ContentToolResult:
				if b.ToolResult != nil && b.ToolResult.Output != "" {
					out = append(out, b.ToolResult.Output)
				}
			case ContentToolUse:
				// Tool-call arguments carry caller-supplied values (search
				// queries, email bodies, addresses) that are as PII-laden as
				// any user message but were historically never scanned. Emit
				// each STRING leaf of the structured Input individually — one
				// projection entry per leaf — so label-adjacent regex patterns
				// (e.g. "ssn: <digits>" inside one argument value) stay intact
				// and the entries line up by ordinal with the redaction
				// addressing walk (see ToolUseStringLeaves).
				if b.ToolUse != nil {
					for _, lf := range ToolUseStringLeaves(b.ToolUse.Input) {
						if lf.Value != "" {
							out = append(out, lf.Value)
						}
					}
				}
			case ContentReasoning:
				if b.Text != "" {
					out = append(out, b.Text)
				}
			}
		}
	}
	return out
}

func httpTextProjection(p *NormalizedPayload) []string {
	if p.HTTP == nil || p.HTTP.BodyView == nil {
		return nil
	}
	bv := p.HTTP.BodyView
	if bv.Text != "" {
		return []string{bv.Text}
	}
	if len(bv.Form) > 0 {
		out := make([]string, 0, len(bv.Form))
		for k, v := range bv.Form {
			out = append(out, k+"="+v)
		}
		return out
	}
	if len(bv.SSEFrames) > 0 {
		out := make([]string, 0, len(bv.SSEFrames))
		for _, f := range bv.SSEFrames {
			switch {
			case f.DataText != "":
				out = append(out, f.DataText)
			case f.Data != nil:
				// Data is a decode product (json.Unmarshal output), so
				// re-marshaling cannot fail; the error is structurally dead.
				b, _ := json.Marshal(f.Data)
				out = append(out, string(b))
			}
		}
		return out
	}
	if bv.JSON != nil {
		b, _ := json.Marshal(bv.JSON)
		return []string{string(b)}
	}
	return nil
}

// JoinedText concatenates the projection with separator sep. Convenience
// for hooks (and AI-Guard) that ship a single-string content to a
// remote judge or webhook.
func (p *NormalizedPayload) JoinedText(sep string) string {
	parts := p.TextProjection()
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, sep)
}
