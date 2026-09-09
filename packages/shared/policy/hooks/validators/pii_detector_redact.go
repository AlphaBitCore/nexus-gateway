package validators

import (
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// addressedSegment pairs a projection text segment with its span content
// address inside the Normalized payload.
type addressedSegment struct {
	address string
	text    string
	// blockType tags the flat ModifiedContent block, and the tag decides whether
	// the block occupies a slot in the POSITIONAL index space that
	// ModifiedContent's consumers walk.
	//
	// "text" for message, refusal and tool-result text — the channels a
	// traffic.Adapter both extracts and rewrites, so a slot here lines up with a
	// slot there.
	//
	// Anything else for a channel the flat adapters do not carry: "tool_use" for
	// a tool-call argument leaf, "reasoning" for chain-of-thought. Those are
	// scanned and addressed (their TransformSpan names the block directly and
	// carries the masking), but they must NOT consume a positional slot — the
	// adapter produces no segment for them, so a slot taken here is a slot the
	// consumer never offers, and every assignment after it lands one early. The
	// symptom is not an offset: the user is shown the model's redacted thinking
	// where the answer should be, while the thinking keeps the value the policy
	// just decided to mask.
	blockType string
}

// collectRedactions walks the Normalized payload in projection order and
// returns the redacted content blocks plus the TransformSpans addressing
// every match (Source=hook, SourceID=pattern.id, Action=redact, offsets in
// the original text). Empty spans means no match (or no normalized input).
// Shared by the inflight-redact path and the storage-only-redact scan.
func (pd *PiiDetector) collectRedactions(input *core.HookInput) ([]core.ContentBlock, []normalize.TransformSpan) {
	if input.Normalized == nil {
		return nil, nil
	}
	segments := input.TextSegments()
	if len(segments) == 0 {
		return nil, nil
	}

	// Walk the Normalized payload in projection order so spans get the
	// right content addresses. The walk mirrors the projection, reasoning
	// blocks included — anything the projection scanned must be addressable
	// here, or a match would be found and then have nowhere to be masked.
	addressed := make([]addressedSegment, 0, len(segments))
	// KindAIEmbedding payloads carry text in Inputs (not Messages).
	// Address each input as "inputs.<index>" so span tracking is accurate.
	if input.Normalized.Kind == normalize.KindAIEmbedding {
		for ii, inp := range input.Normalized.Inputs {
			if inp != "" {
				addressed = append(addressed, addressedSegment{
					address:   fmt.Sprintf("inputs.%d", ii),
					text:      inp,
					blockType: "text",
				})
			}
		}
	} else {
		for mi, m := range input.Normalized.Messages {
			for ci, b := range m.Content {
				switch b.Type {
				case normalize.ContentText:
					addressed = append(addressed, addressedSegment{
						address:   fmt.Sprintf("messages.%d.content.%d", mi, ci),
						text:      b.Text,
						blockType: "text",
					})
				case normalize.ContentRefusal:
					// A refusal IS assistant-visible content on the wire
					// (choices[].message.refusal), and the traffic adapters both
					// extract and rewrite it — so it holds a positional slot.
					if b.Text != "" {
						addressed = append(addressed, addressedSegment{
							address:   fmt.Sprintf("messages.%d.content.%d", mi, ci),
							text:      b.Text,
							blockType: "text",
						})
					}
				case normalize.ContentReasoning:
					// Scanned and addressed, but not positional: the adapters put
					// chain-of-thought on ReasoningSegments and the rewrite path
					// walks Segments only, so there is no slot to line up with.
					if b.Text != "" {
						addressed = append(addressed, addressedSegment{
							address:   fmt.Sprintf("messages.%d.content.%d", mi, ci),
							text:      b.Text,
							blockType: "reasoning",
						})
					}
				case normalize.ContentToolResult:
					if b.ToolResult != nil {
						addressed = append(addressed, addressedSegment{
							address:   fmt.Sprintf("messages.%d.content.%d.toolResult", mi, ci),
							text:      b.ToolResult.Output,
							blockType: "text",
						})
					}
				case normalize.ContentToolUse:
					// One segment per non-empty STRING leaf of the tool-call
					// Input, ordinal-addressed via ToolUseStringLeaves so the
					// detection projection, this addressing, and resolveTextRef
					// all agree on leaf order regardless of map randomization.
					if b.ToolUse != nil {
						for _, lf := range normalize.ToolUseStringLeaves(b.ToolUse.Input) {
							if lf.Value == "" {
								continue
							}
							addressed = append(addressed, addressedSegment{
								address:   fmt.Sprintf("messages.%d.content.%d.toolUse.input.%d", mi, ci, lf.Ordinal),
								text:      lf.Value,
								blockType: "tool_use",
							})
						}
					}
				}
			}
		}
	}

	modified := make([]core.ContentBlock, len(addressed))
	spans := make([]normalize.TransformSpan, 0)

	for i, seg := range addressed {
		text := seg.text
		// Collect per-pattern match offsets in *original* text so spans
		// reference the pre-replacement byte ranges; apply replacements
		// to the working text in descending offset order.
		type segMatch struct {
			ruleID, replacement string
			start, end          int
		}
		var matches []segMatch
		for idx := range pd.patterns {
			p := &pd.patterns[idx]
			for _, loc := range p.re.FindAllStringIndex(seg.text, -1) {
				if p.luhn && !luhnValid(seg.text[loc[0]:loc[1]]) {
					continue
				}
				matches = append(matches, segMatch{
					ruleID:      p.id,
					replacement: p.replacement,
					start:       loc[0],
					end:         loc[1],
				})
			}
		}
		// Sort matches by descending start so successive replacements
		// don't shift earlier offsets.
		for a := 1; a < len(matches); a++ {
			for b := a; b > 0 && matches[b].start > matches[b-1].start; b-- {
				matches[b], matches[b-1] = matches[b-1], matches[b]
			}
		}
		for _, m := range matches {
			text = text[:m.start] + m.replacement + text[m.end:]
			spans = append(spans, normalize.TransformSpan{
				Source:         normalize.SourceHook,
				SourceID:       m.ruleID,
				Action:         normalize.ActionRedact,
				ContentAddress: seg.address,
				Start:          m.start,
				End:            m.end,
				Replacement:    m.replacement,
			})
		}
		bt := seg.blockType
		if bt == "" {
			bt = "text"
		}
		modified[i] = core.ContentBlock{Role: "user", Type: bt, Text: text}
	}

	if len(spans) == 0 {
		return nil, nil
	}
	return modified, spans
}
