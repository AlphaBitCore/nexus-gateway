package pipeline

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// positionalBlock reports whether a canonical block occupies a slot in the flat
// ModifiedContent index space.
//
// The rule is "the channels a traffic.Adapter carries in Segments", because that
// is the space the flat list was built to line up with. Reasoning and tool-call
// arguments are scanned and masked through their own ordinal-addressed
// TransformSpan; they hold no slot here, and the producers tag them so.
//
// An empty block holds no slot either: a producer emits an entry only for text it
// actually scanned.
func positionalBlock(b normalize.ContentBlock) bool {
	switch b.Type {
	case normalize.ContentText:
		return true
	case normalize.ContentRefusal:
		return b.Text != ""
	case normalize.ContentToolResult:
		return b.ToolResult != nil && b.ToolResult.Output != ""
	default:
		return false
	}
}

// assignPositional writes the hook's replacement into the field that block kind
// actually carries. A tool result's text lives on ToolResult.Output, not on Text,
// so writing Text for one would leave the masked value in a field nothing reads
// and the original in the field everything does.
func assignPositional(b *normalize.ContentBlock, text string) {
	if b.Type == normalize.ContentToolResult {
		if b.ToolResult != nil {
			b.ToolResult.Output = text
		}
		return
	}
	b.Text = text
}

// applyModifiedContentToNormalized walks the payload's positional blocks and
// replaces each with the hook-produced modified text, in order.
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
			// The positional slot space is "the channels a traffic.Adapter carries
			// in Segments": message text, a refusal (choices[].message.refusal),
			// and a tool result's output. Those are what the producers tag "text",
			// and the two sides have to enumerate the same set or the assignment
			// walks off by one.
			//
			// This used to offer a slot for ContentText alone while the producers
			// marked all three, so a refusal or a tool result anywhere in the
			// message shifted every later assignment — the same defect reasoning
			// had, in two more channels nobody had looked at.
			if !positionalBlock(b) {
				continue
			}
			// Consume only the entries that hold a positional slot. The producers
			// tag every block they scanned, and anything not tagged "text" is a
			// channel the flat adapters neither extract nor rewrite — tool-call
			// argument leaves ("tool_use") and chain-of-thought ("reasoning").
			// Those are masked by their own ordinal-addressed TransformSpan; a
			// walk that consumed them as text slots would skew every assignment
			// that follows.
			//
			// Written as "skip anything that is not text" rather than "skip
			// tool_use", because that is the actual rule and the narrower spelling
			// is how reasoning got through: it was tagged "text", took a slot the
			// walk below never offers, and the user was served the model's
			// redacted thinking in place of its answer while the thinking kept the
			// value the policy had just masked. A new non-wire channel now lands
			// on the safe side by default.
			for mi < len(modified) && modified[mi].Type != "" && modified[mi].Type != "text" {
				mi++
			}
			if mi >= len(modified) {
				break
			}
			assignPositional(&nm.Content[j], modified[mi].Text)
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
