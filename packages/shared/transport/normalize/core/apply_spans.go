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

	// Deferred write-backs are accumulated in a LOCAL writeCtx owned by this
	// one ApplySpans call and threaded through applyToAddress →
	// resolveTextRef → toolLeafRef/mapEntryRef. ApplySpans runs concurrently
	// per request (pipeline join + ToolCallArgsFromPayload), so the pending
	// closures MUST NOT live on a package global: two goroutines appending to
	// and truncating one shared slice would race and one flush could drop the
	// other's masking closure, forwarding unmasked PII. A per-call value has
	// no shared state and needs no lock (keeping the hot path serial-free).
	wc := &writeCtx{}

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
		applied := applyToAddress(&out, addr, g.spans, wc)
		for _, s := range g.spans {
			if !applied[spanKey(s)] {
				skipped = append(skipped, s)
			}
		}
	}
	// Commit the deferred writes this call accumulated. Map entries
	// (http.bodyView.form[<key>]) and tool-use leaves
	// (messages.<i>.content.<j>.toolUse.input.<n>) aren't directly
	// addressable, so resolveTextRef returns a *string view of a boxed local
	// cell; without this flush those mutations would be lost.
	wc.flush()
	if len(skipped) == 0 {
		return out, nil
	}
	return out, skipped
}

// writeCtx accumulates the deferred write-back closures produced while
// applying spans to content that yields no directly addressable *string:
// Go map entries (http.bodyView.form[<key>]) and string leaves buried in a
// ToolUse.Input map/slice tree (messages.<i>.content.<j>.toolUse.input.<n>).
// resolveTextRef hands back a *string view of a boxed local cell and records
// the corresponding write-back here; ApplySpans calls flush after the
// per-address apply loop.
//
// The accumulator is OWNED by a single ApplySpans invocation — a local value
// threaded through applyToAddress → resolveTextRef → toolLeafRef/mapEntryRef.
// Two concurrent ApplySpans calls therefore never share write state, so there
// is no package-global slice to race on and no need for a mutex that would
// serialize the hot path.
type writeCtx struct {
	mapWrites  []mapWriteEntry
	treeWrites []func()
}

func (w *writeCtx) flush() {
	for _, e := range w.mapWrites {
		e.m[e.key] = *e.ptr
	}
	for _, fn := range w.treeWrites {
		fn()
	}
}

// applyToAddress walks the addressed content block in `p` and applies
// the spans to its underlying text. Returns a set of span keys that
// were successfully applied.
func applyToAddress(p *NormalizedPayload, addr string, spans []TransformSpan, wc *writeCtx) map[string]bool {
	applied := map[string]bool{}
	ref, ok := resolveTextRef(p, addr, wc)
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
func resolveTextRef(p *NormalizedPayload, addr string, wc *writeCtx) (*string, bool) {
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
			switch parts[4] {
			case "toolResult":
				if block.ToolResult == nil {
					return nil, false
				}
				return &block.ToolResult.Output, true
			case "toolUse":
				// messages.<i>.content.<j>.toolUse.input.<ordinal> — addresses
				// the ordinal-th STRING leaf of the structured tool-call Input,
				// re-walked via ToolUseStringLeaves so the ordinal resolves to
				// the same leaf detection/addressing chose. The returned *string
				// is a boxed copy with a deferred write-back into the live nested
				// map/slice (committed by writeCtx.flush in ApplySpans).
				if len(parts) != 7 || parts[5] != "input" {
					return nil, false
				}
				ord, err := parseInt(parts[6])
				if err != nil {
					return nil, false
				}
				if block.ToolUse == nil {
					return nil, false
				}
				return toolLeafRef(block.ToolUse.Input, ord, wc)
			default:
				return nil, false
			}
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
			return mapEntryRef(p.HTTP.BodyView.Form, key, wc), true
		}
	}
	return nil, false
}

// mapEntryRef returns a *string view of a map entry by boxing the value in
// a local cell. Maps in Go cannot be addressed directly, so we hand back a
// *string that holds the current value; the caller in applyToAddress writes
// through it once, and the write-back into the map is recorded on the
// per-call writeCtx and committed by ApplySpans after the apply loop.
func mapEntryRef(m map[string]string, key string, wc *writeCtx) *string {
	cell := m[key]
	ptr := &cell
	wc.mapWrites = append(wc.mapWrites, mapWriteEntry{m: m, key: key, ptr: ptr})
	return ptr
}

type mapWriteEntry struct {
	m   map[string]string
	key string
	ptr *string
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
					if b.ToolUse != nil {
						// Deep-copy the ToolUse and its Input tree so masking
						// a leaf in the clone never mutates the caller's map
						// (map[string]any is a reference type; a shallow copy
						// would alias the original's nested values).
						tu := *b.ToolUse
						tu.Input = deepCopyJSONMap(b.ToolUse.Input)
						cs[j].ToolUse = &tu
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
