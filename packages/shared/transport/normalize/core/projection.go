package core

import (
	"strings"
)

// TextProjection returns the flat list of text fragments hooks scan for
// content matches. The projection is intentionally narrow:
//
//   - AI kinds: one entry per ContentBlock whose Type is ContentText or
//     ContentToolResult (text payload). System / user / assistant /
//     tool roles all flow into the same flat list — regex-based hooks
//     do not need to distinguish them. Reasoning blocks are excluded by
//     default (their text is "internal model thinking" and is rarely
//     what a compliance hook should match against; future per-hook
//     config can opt-in via IncludeReasoning).
//   - HTTP kinds: the BodyView.Text projection is returned as a single
//     entry. Form fields are flattened to "key=value" lines.
//   - http-binary / unsupported / redacted payloads: empty slice.
//
// One projection serves every regex-based hook in shared/hooks; per-hook
// configuration (e.g. PII-detector wanting to scan reasoning too) can
// supply a TextProjectionOptions when invoking TextProjectionWith.
func (p *NormalizedPayload) TextProjection() []string {
	if p == nil || p.Redacted {
		return nil
	}
	return p.TextProjectionWith(TextProjectionOptions{})
}

// TextProjectionOptions tunes the projection.
type TextProjectionOptions struct {
	// IncludeReasoning, when true, adds ContentReasoning blocks to the
	// projection. Default false: reasoning is informational metadata,
	// not user-spoken content.
	IncludeReasoning bool
}

// TextProjectionWith returns the text fragments under the supplied
// options. Most callers should use TextProjection().
func (p *NormalizedPayload) TextProjectionWith(opts TextProjectionOptions) []string {
	if p == nil || p.Redacted {
		return nil
	}
	if p.Kind.IsAI() {
		return aiTextProjection(p, opts)
	}
	if p.Kind.IsHTTP() {
		return httpTextProjection(p)
	}
	return nil
}

func aiTextProjection(p *NormalizedPayload, opts TextProjectionOptions) []string {
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
			case ContentText:
				if b.Text != "" {
					out = append(out, b.Text)
				}
			case ContentToolResult:
				if b.ToolResult != nil && b.ToolResult.Output != "" {
					out = append(out, b.ToolResult.Output)
				}
			case ContentReasoning:
				if opts.IncludeReasoning && b.Text != "" {
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
