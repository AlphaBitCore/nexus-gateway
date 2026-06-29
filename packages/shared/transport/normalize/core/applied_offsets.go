package core

import "strings"

// AppliedSpanOffsets relocates each span's [Start, End) to its position in
// the text AFTER ApplySpans has run, so a consumer reading the stored
// (post-redact) payload can locate each Replacement. ApplySpans keeps the
// original (pre-redact) offsets, which only coincide with the post-redact
// positions when a block has a single span or every replacement preserves
// length; for multiple length-changing spans in one block the later ones
// drift. This returns spans whose Start/End bracket the Replacement in the
// redacted text (End = Start + len(Replacement)).
//
// Only spans ApplySpans would actually apply are returned — address must
// resolve and the range must be valid — so the result never carries a
// phantom badge for a span that left the text untouched. Offsets are
// computed per ContentAddress assuming non-overlapping spans, the same
// assumption ApplySpans relies on. p is not mutated.
func AppliedSpanOffsets(p NormalizedPayload, spans []TransformSpan) []TransformSpan {
	if len(spans) == 0 {
		return nil
	}
	groups := map[string][]TransformSpan{}
	order := []string{}
	for _, s := range spans {
		if _, ok := groups[s.ContentAddress]; !ok {
			order = append(order, s.ContentAddress)
		}
		groups[s.ContentAddress] = append(groups[s.ContentAddress], s)
	}
	out := []TransformSpan{}
	for _, addr := range order {
		textLen, ok := resolveTextLen(&p, addr)
		if !ok {
			continue // span did not apply — no badge
		}
		g := append([]TransformSpan(nil), groups[addr]...)
		sortSpansAscending(g)
		delta := 0
		for _, s := range g {
			start, end := s.Start, s.End
			if start < 0 {
				start = 0
			}
			if end > textLen {
				end = textLen
			}
			if start > textLen || start > end {
				continue // skipped by ApplySpans
			}
			adj := s
			adj.Start = start + delta
			adj.End = adj.Start + len(s.Replacement)
			out = append(out, adj)
			delta += len(s.Replacement) - (end - start)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveTextLen returns the byte length of the text addressed by addr,
// read-only — unlike resolveTextRef it does not synthesize map pointers or
// schedule map write-backs, so it is safe to call outside the ApplySpans
// flush cycle.
func resolveTextLen(p *NormalizedPayload, addr string) (int, bool) {
	parts := strings.Split(addr, ".")
	if len(parts) == 0 {
		return 0, false
	}
	switch parts[0] {
	case "messages":
		if len(parts) < 4 || parts[2] != "content" {
			return 0, false
		}
		i, err := parseInt(parts[1])
		if err != nil || i < 0 || i >= len(p.Messages) {
			return 0, false
		}
		j, err := parseInt(parts[3])
		if err != nil || j < 0 || j >= len(p.Messages[i].Content) {
			return 0, false
		}
		block := &p.Messages[i].Content[j]
		if len(parts) > 4 {
			switch parts[4] {
			case "toolResult":
				if block.ToolResult == nil {
					return 0, false
				}
				return len(block.ToolResult.Output), true
			case "toolUse":
				if len(parts) != 7 || parts[5] != "input" || block.ToolUse == nil {
					return 0, false
				}
				ord, err := parseInt(parts[6])
				if err != nil {
					return 0, false
				}
				return toolLeafLen(block.ToolUse.Input, ord)
			default:
				return 0, false
			}
		}
		return len(block.Text), true
	case "inputs":
		// inputs.<i> — see resolveTextRef.
		if len(parts) != 2 {
			return 0, false
		}
		i, err := parseInt(parts[1])
		if err != nil || i < 0 || i >= len(p.Inputs) {
			return 0, false
		}
		return len(p.Inputs[i]), true
	case "http":
		if p.HTTP == nil || p.HTTP.BodyView == nil {
			return 0, false
		}
		if len(parts) == 2 && parts[1] == "bodyView" {
			return len(p.HTTP.BodyView.Text), true
		}
		if len(parts) == 4 && parts[1] == "bodyView" && parts[2] == "form" {
			if p.HTTP.BodyView.Form == nil {
				return 0, false
			}
			v, ok := p.HTTP.BodyView.Form[parts[3]]
			if !ok {
				return 0, false
			}
			return len(v), true
		}
	}
	return 0, false
}
