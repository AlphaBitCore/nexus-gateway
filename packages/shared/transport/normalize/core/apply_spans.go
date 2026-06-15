package core

import (
	"fmt"
	"strings"
)

// ApplySpans returns a copy of p with each TransformSpan applied to its
// addressed content. The returned payload is independent of p; p itself
// is not mutated.
//
// Spans are applied in *descending* offset order within each addressed
// content block so the byte offsets in later spans remain valid as
// earlier spans are replaced. Cross-block spans are applied in input
// order. Spans whose ContentAddress does not resolve to an existing
// content block are skipped and reported in `skipped` so callers can
// log / surface them.
//
// Address grammar:
//   - AI kinds: "messages.<i>.content.<j>"      → addresses messages[i].content[j].Text
//     "messages.<i>.content.<j>.toolResult"  → addresses tool_result.output
//   - HTTP kinds: "http.bodyView"               → addresses http.body_view.text
//     "http.bodyView.form.<key>"    → addresses http.body_view.form[key]
//
// For inject actions (start == end) the Replacement is inserted at the
// offset; for redact / replace / strip the [start, end) byte range is
// replaced with Replacement (strip uses Replacement = "").
func ApplySpans(p NormalizedPayload, spans []TransformSpan) (NormalizedPayload, []TransformSpan) {
	out := clonePayload(p)
	if len(spans) == 0 {
		return out, nil
	}

	// Group spans by ContentAddress so we can sort offsets per block.
	type byAddr struct {
		addr  string
		spans []TransformSpan
	}
	groups := map[string]*byAddr{}
	order := []string{}
	for _, s := range spans {
		if _, ok := groups[s.ContentAddress]; !ok {
			groups[s.ContentAddress] = &byAddr{addr: s.ContentAddress}
			order = append(order, s.ContentAddress)
		}
		groups[s.ContentAddress].spans = append(groups[s.ContentAddress].spans, s)
	}

	skipped := make([]TransformSpan, 0)
	for _, addr := range order {
		g := groups[addr]
		// Sort by start descending so applying later spans does not shift
		// offsets of earlier spans.
		sortSpansDescending(g.spans)
		applied := applyToAddress(&out, addr, g.spans)
		for _, s := range g.spans {
			if !applied[spanKey(s)] {
				skipped = append(skipped, s)
			}
		}
	}
	// Commit any deferred map writes accumulated by mapEntryRef during
	// the per-address apply loop. Map entries aren't addressable, so
	// resolveTextRef returns a *string view of a local cell; without
	// this flush, mutations to http.bodyView.form[<key>] would be lost.
	flushMapWrites()
	if len(skipped) == 0 {
		return out, nil
	}
	return out, skipped
}

// applyToAddress walks the addressed content block in `p` and applies
// the spans to its underlying text. Returns a set of span keys that
// were successfully applied.
func applyToAddress(p *NormalizedPayload, addr string, spans []TransformSpan) map[string]bool {
	applied := map[string]bool{}
	ref, ok := resolveTextRef(p, addr)
	if !ok {
		return applied
	}
	text := *ref
	for _, s := range spans {
		start, end := s.Start, s.End
		if start < 0 {
			start = 0
		}
		if end > len(text) {
			end = len(text)
		}
		if start > len(text) {
			continue
		}
		if start > end {
			continue
		}
		text = text[:start] + s.Replacement + text[end:]
		applied[spanKey(s)] = true
	}
	*ref = text
	return applied
}

// resolveTextRef walks p to the *string addressed by addr and returns
// a pointer to it for in-place mutation. The bool reports whether the
// path resolved.
func resolveTextRef(p *NormalizedPayload, addr string) (*string, bool) {
	// strings.Split always yields at least one element, so parts[0] is
	// safe to switch on even for an empty addr (it dispatches to the
	// default not-resolved arm).
	parts := strings.Split(addr, ".")
	switch parts[0] {
	case "messages":
		// messages.<i>.content.<j>[.toolResult]
		if len(parts) < 4 || parts[2] != "content" {
			return nil, false
		}
		i, err := parseInt(parts[1])
		if err != nil || i < 0 || i >= len(p.Messages) {
			return nil, false
		}
		j, err := parseInt(parts[3])
		if err != nil || j < 0 || j >= len(p.Messages[i].Content) {
			return nil, false
		}
		block := &p.Messages[i].Content[j]
		if len(parts) > 4 {
			if parts[4] != "toolResult" {
				return nil, false
			}
			if block.ToolResult == nil {
				return nil, false
			}
			return &block.ToolResult.Output, true
		}
		return &block.Text, true
	case "inputs":
		// inputs.<i> — KindAIEmbedding carries its text in Inputs, not
		// Messages; hooks address embedding segments this way.
		if len(parts) != 2 {
			return nil, false
		}
		i, err := parseInt(parts[1])
		if err != nil || i < 0 || i >= len(p.Inputs) {
			return nil, false
		}
		return &p.Inputs[i], true
	case "http":
		if p.HTTP == nil || p.HTTP.BodyView == nil {
			return nil, false
		}
		if len(parts) == 2 && parts[1] == "bodyView" {
			return &p.HTTP.BodyView.Text, true
		}
		// http.bodyView.form.<key>
		if len(parts) == 4 && parts[1] == "bodyView" && parts[2] == "form" {
			if p.HTTP.BodyView.Form == nil {
				return nil, false
			}
			key := parts[3]
			v, ok := p.HTTP.BodyView.Form[key]
			if !ok {
				return nil, false
			}
			// Maps don't yield addressable pointers; rebuild the entry.
			p.HTTP.BodyView.Form[key] = v
			return mapEntryRef(p.HTTP.BodyView.Form, key), true
		}
	}
	return nil, false
}

// mapEntryRef returns a *string view of a map entry by temporarily
// boxing the value behind a small auxiliary struct. Maps in Go cannot
// be addressed directly, so we synthesize a pointer that, when mutated
// by ApplyToAddress, writes back via the closure on return.
//
// To keep ApplySpans pure-ish, we use a sentinel storage slot keyed in
// a side table tied to the map identity. Simpler approach: hand back a
// *string that holds the current value; callers write through it and
// we re-insert into the map after the apply loop completes. Since the
// caller pattern is `*ref = text` immediately after the loop, we
// emulate it with a tiny adapter type below.
func mapEntryRef(m map[string]string, key string) *string {
	// Read-modify-write via local var; reader callers in ApplySpans do
	// `*ref = text` once at the end, so a single write-back is enough.
	cell := m[key]
	ptr := &cell
	// Schedule a write-back when the slot is finalized. ApplyToAddress
	// reads *ref once and writes *ref once; we install the write-back
	// inside a finalizer attached to the pointer via the package-level
	// pendingMapWrites.
	registerMapWrite(m, key, ptr)
	return ptr
}

// pendingMapWrites tracks (map, key, ptr) tuples so ApplySpans can
// flush map writes back at the end of its loop. Single-threaded use
// pattern matches the call site.
var pendingMapWrites []mapWriteEntry

type mapWriteEntry struct {
	m   map[string]string
	key string
	ptr *string
}

func registerMapWrite(m map[string]string, key string, ptr *string) {
	pendingMapWrites = append(pendingMapWrites, mapWriteEntry{m: m, key: key, ptr: ptr})
}

func flushMapWrites() {
	for _, e := range pendingMapWrites {
		e.m[e.key] = *e.ptr
	}
	pendingMapWrites = pendingMapWrites[:0]
}

func sortSpansDescending(spans []TransformSpan) {
	// insertion sort — span counts per block are tiny.
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].Start > spans[j-1].Start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

func sortSpansAscending(spans []TransformSpan) {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].Start < spans[j-1].Start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

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
			if parts[4] != "toolResult" || block.ToolResult == nil {
				return 0, false
			}
			return len(block.ToolResult.Output), true
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

func spanKey(s TransformSpan) string {
	return fmt.Sprintf("%s|%d-%d|%s|%s", s.ContentAddress, s.Start, s.End, s.Source, s.SourceID)
}

// ParseInt parses a non-negative decimal integer string.
// Exported for test access from sibling sub-packages.
func ParseInt(s string) (int, error) { return parseInt(s) }

func parseInt(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// ClonePayload performs a deep-enough copy of NormalizedPayload that
// the caller may mutate either copy without affecting the other.
// Exported for test access from sibling sub-packages.
func ClonePayload(p NormalizedPayload) NormalizedPayload { return clonePayload(p) }

// clonePayload performs a deep-enough copy of NormalizedPayload that
// the caller may mutate either copy without affecting the other.
func clonePayload(p NormalizedPayload) NormalizedPayload {
	out := p
	if p.Messages != nil {
		msgs := make([]Message, len(p.Messages))
		for i, m := range p.Messages {
			msgs[i] = m
			if m.Content != nil {
				cs := make([]ContentBlock, len(m.Content))
				for j, b := range m.Content {
					cs[j] = b
					if b.ToolResult != nil {
						tr := *b.ToolResult
						cs[j].ToolResult = &tr
					}
				}
				msgs[i].Content = cs
			}
		}
		out.Messages = msgs
	}
	if p.Tools != nil {
		ts := make([]ToolDef, len(p.Tools))
		copy(ts, p.Tools)
		out.Tools = ts
	}
	if p.RuleIDs != nil {
		rs := make([]string, len(p.RuleIDs))
		copy(rs, p.RuleIDs)
		out.RuleIDs = rs
	}
	if p.HTTP != nil {
		h := *p.HTTP
		if p.HTTP.BodyView != nil {
			bv := *p.HTTP.BodyView
			if p.HTTP.BodyView.Form != nil {
				form := make(map[string]string, len(p.HTTP.BodyView.Form))
				for k, v := range p.HTTP.BodyView.Form {
					form[k] = v
				}
				bv.Form = form
			}
			h.BodyView = &bv
		}
		if p.HTTP.HeadersFiltered != nil {
			hf := make(map[string]string, len(p.HTTP.HeadersFiltered))
			for k, v := range p.HTTP.HeadersFiltered {
				hf[k] = v
			}
			h.HeadersFiltered = hf
		}
		out.HTTP = &h
	}
	return out
}
