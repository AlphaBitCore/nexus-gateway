package core

import (
	"fmt"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// PayloadFromTextSegments is a convenience used by test fixtures and
// transitional adapter paths to construct a NormalizedPayload from a
// flat list of user-role text segments.
func PayloadFromTextSegments(segments []string) *normalize.NormalizedPayload {
	if len(segments) == 0 {
		return &normalize.NormalizedPayload{Kind: normalize.KindAIChat, NormalizeVersion: normalize.SchemaVersion}
	}
	content := make([]normalize.ContentBlock, 0, len(segments))
	for _, s := range segments {
		content = append(content, normalize.ContentBlock{Type: normalize.ContentText, Text: s})
	}
	return &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Protocol:         "synthetic",
		Messages:         []normalize.Message{{Role: normalize.RoleUser, Content: content}},
	}
}

// SpansFromModifiedContent computes TransformSpans for a transitional
// hook implementation that still produces ModifiedContent as a flat
// projection of the input.TextSegments.
func SpansFromModifiedContent(input *HookInput, modified []ContentBlock, source normalize.TransformSource, sourceID string, action normalize.TransformAction) []normalize.TransformSpan {
	if input == nil || input.Normalized == nil || len(modified) == 0 {
		return nil
	}
	original := input.TextSegments()
	if len(original) == 0 {
		return nil
	}
	limit := len(modified)
	if len(original) < limit {
		limit = len(original)
	}
	spans := make([]normalize.TransformSpan, 0, limit)
	idx := 0
	for mi, m := range input.Normalized.Messages {
		for ci, b := range m.Content {
			// Tool-call argument leaves occupy projection slots (one per
			// non-empty string leaf, matching aiTextProjection) in
			// original/modified, but are redacted via ordinal-addressed spans
			// — not this flat positional walk. Advance idx past them so the
			// text / tool_result slots that follow a ContentToolUse block stay
			// aligned with original[idx]/modified[idx].
			if b.Type == normalize.ContentToolUse {
				if b.ToolUse != nil {
					for _, lf := range normalize.ToolUseStringLeaves(b.ToolUse.Input) {
						if lf.Value != "" {
							idx++
						}
					}
				}
				continue
			}
			if b.Type != normalize.ContentText && b.Type != normalize.ContentToolResult {
				continue
			}
			if idx >= limit {
				return spans
			}
			origText := original[idx]
			newText := modified[idx].Text
			idx++
			if origText == newText {
				continue
			}
			addr := fmt.Sprintf("messages.%d.content.%d", mi, ci)
			if b.Type == normalize.ContentToolResult {
				addr = fmt.Sprintf("messages.%d.content.%d.toolResult", mi, ci)
			}
			spans = append(spans, normalize.TransformSpan{
				Source:         source,
				SourceID:       sourceID,
				Action:         action,
				ContentAddress: addr,
				Start:          0,
				End:            len(origText),
				Replacement:    newText,
			})
		}
	}
	return spans
}
