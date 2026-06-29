package pipeline

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// applyModifiedContentToNormalized walks the first text fragments of the
// payload and replaces them with the hook-produced modified text, in order.
// Retained for hooks that still emit ModifiedContent; prefer TransformSpan
// application via normalize.ApplySpans for new hook implementations.
func applyModifiedContentToNormalized(p *normalize.NormalizedPayload, modified []core.ContentBlock) *normalize.NormalizedPayload {
	if p == nil || len(modified) == 0 {
		return p
	}
	out := *p
	out.Messages = make([]normalize.Message, len(p.Messages))
	mi := 0
	for i, m := range p.Messages {
		nm := m
		nm.Content = make([]normalize.ContentBlock, len(m.Content))
		copy(nm.Content, m.Content)
		for j, b := range nm.Content {
			if b.Type != normalize.ContentText {
				continue
			}
			// Skip tool-use entries in the flat ModifiedContent index space:
			// tool-call argument leaves are tagged "tool_use" and are redacted
			// via ordinal-addressed TransformSpans, not this positional text
			// walk. Consuming one as a text slot here would skew every text
			// assignment after a tool_use block (a ContentToolUse preceding a
			// ContentText). Consistent with contentBlocksToNormalized, which
			// drops non-text blocks.
			for mi < len(modified) && modified[mi].Type == "tool_use" {
				mi++
			}
			if mi >= len(modified) {
				break
			}
			nm.Content[j].Text = modified[mi].Text
			mi++
		}
		out.Messages[i] = nm
		if mi >= len(modified) {
			// Copy remaining messages unchanged.
			if i+1 < len(p.Messages) {
				rest := make([]normalize.Message, len(p.Messages)-i-1)
				copy(rest, p.Messages[i+1:])
				out.Messages = append(out.Messages[:i+1], rest...)
			}
			break
		}
	}
	return &out
}
