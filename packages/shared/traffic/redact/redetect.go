package redact

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Match is one sensitive byte range a Redetector located within a single
// text block. Start/End are byte offsets into the scanned text;
// Replacement is the marker that replaces the range (empty falls back to
// "[REDACTED_<RULE_ID>]"). Matches carry offsets and rule attribution
// only — never the matched content.
type Match struct {
	RuleID      string
	Start, End  int
	Replacement string
}

// Redetector re-locates sensitive content for the given rule IDs within
// one text block of the storage-bound normalized payload. The hook
// pipeline supplies an implementation backed by the matched hooks'
// compiled patterns; see the package comment for why the patterns cannot
// reach the audit writers directly.
type Redetector func(text string, ruleIDs []string) []Match

// ApplyStorageAction transforms marshalled NormalizedPayload bytes
// before they are persisted (traffic_event_normalized, agent SQLite
// normalized columns), per the operator's onMatch.storageAction:
//
//   - keep / "" / nil raw   → no change
//   - redact                → ApplySpans(payload, spans) → re-marshal
//   - drop-content          → replace payload with the redacted-placeholder
//     {redacted:true, redactedReason:"operator-drop", kind, ruleIds}
//
// For the redact case it also returns the spans relocated to their offsets
// in the redacted bytes (AppliedSpanOffsets), so the audit UI can mark each
// redaction inline. drop-content replaces the whole payload and returns no
// spans; keep changes nothing.
//
// When one or more spans do not resolve on the storage-time payload,
// the redetect function (when non-nil) re-locates the failed rules'
// content on the payload's own text blocks and the redaction is applied
// at the re-detected addresses — the stored copy is the redacted
// conversation, not a placeholder. Redaction failures that survive
// re-detection (no spans, unparsable payload, unresolved addresses with
// no re-detection hit, marshal failure) degrade to the drop-content
// placeholder stamped redactedReason:"redact-degraded" plus a cause and
// the failed content addresses — never persist what cannot be redacted.
// Degradation preserves the original spans on the second return value so
// the audit row keeps the diagnostic span metadata (offsets, rule IDs,
// replacement markers — spans never carry matched content). Unknown
// actions leave the bytes unchanged — the storage policy is
// observability, not a runtime gate.
func ApplyStorageAction(raw json.RawMessage, action string, spans []normcore.TransformSpan, ruleIDs []string, redetect Redetector) (json.RawMessage, []normcore.TransformSpan) {
	if len(raw) == 0 {
		return raw, nil
	}
	switch action {
	case "", "keep":
		return raw, nil
	case "redact":
		if len(spans) == 0 {
			// A hook demanded redaction but produced no spans (e.g. a
			// keyword / content-safety match — those detectors locate no
			// byte ranges). Persisting the raw payload would ignore the
			// operator's policy, so degrade to the drop-content placeholder:
			// never store what we cannot redact.
			return degradedPlaceholder(raw, ruleIDs, normcore.DegradeCauseNoSpans, nil), nil
		}
		var payload normcore.NormalizedPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return degradedPlaceholder(raw, ruleIDs, normcore.DegradeCausePayloadUnmarshal, nil), spans
		}
		patched, skipped := normcore.ApplySpans(payload, spans)
		if len(skipped) > 0 {
			// One or more spans did not resolve to a content block — the
			// patched payload still contains that matched content. Re-locate
			// the failed rules' content on the storage-time payload itself;
			// only when that also fails, degrade to the placeholder rather
			// than persist a partial redaction.
			if b, relocated, ok := redetectAndApply(payload, spans, skipped, redetect); ok {
				reportStorageOutcome(StorageOutcomeRescued, normcore.DegradeCauseSpansUnresolved)
				return b, relocated
			}
			return degradedPlaceholder(raw, ruleIDs, normcore.DegradeCauseSpansUnresolved, failedAddresses(skipped)), spans
		}
		b, err := marshalJSON(patched)
		if err != nil {
			return degradedPlaceholder(raw, ruleIDs, normcore.DegradeCauseMarshalFailed, nil), spans
		}
		return b, normcore.AppliedSpanOffsets(payload, spans)
	case "drop-content":
		return placeholderPayload(raw, ruleIDs, normcore.RedactedReasonOperatorDrop, nil), nil
	}
	return raw, nil
}

// redetectAndApply retries a partially-unresolved redaction by re-locating
// the failed rules' content on the storage-time payload's own text blocks.
// It applies the resolvable original spans plus the re-detected spans in a
// single pass and returns the redacted bytes with relocated span offsets.
// ok=false means the caller must degrade: no redetector, unattributable
// skipped spans, a failed rule whose content could not be re-located, or a
// marshal failure.
func redetectAndApply(payload normcore.NormalizedPayload, spans, skipped []normcore.TransformSpan, redetect Redetector) (json.RawMessage, []normcore.TransformSpan, bool) {
	if redetect == nil {
		return nil, nil, false
	}
	failedRules := CollectRuleIDs(skipped)
	if len(failedRules) == 0 {
		// Skipped spans without rule attribution cannot be re-detected.
		return nil, nil, false
	}
	resolvable := subtractSpans(spans, skipped)
	redetected, covered := redetectSpans(payload, failedRules, redetect, resolvable)
	// Every failed rule must be accounted for in the stored copy — either a
	// re-detected span redacts its content, or its every occurrence already
	// lies inside ranges other spans replace — otherwise we cannot prove
	// the stored copy is clean for that rule.
	if !coversRules(redetected, covered, failedRules) {
		return nil, nil, false
	}
	all := make([]normcore.TransformSpan, 0, len(resolvable)+len(redetected))
	all = append(all, resolvable...)
	all = append(all, redetected...)
	// Every span in `all` applies by construction: the resolvable set
	// already applied on the first pass, and each re-detected span was
	// validated against the very text its address resolves to. The spans
	// are also mutually disjoint per address (redetectSpans trims every
	// match against the ranges already claimed), which is the assumption
	// ApplySpans and AppliedSpanOffsets rely on: with disjoint spans,
	// descending-offset application replaces every original byte of every
	// span range, and badge offsets relocate correctly.
	patched, _ := normcore.ApplySpans(payload, all)
	b, err := marshalJSON(patched)
	if err != nil {
		return nil, nil, false
	}
	return b, normcore.AppliedSpanOffsets(payload, all), true
}

// redetectSpans walks every text block of the payload, asks the redetector
// for the failed rules' matches, and converts them into TransformSpans
// addressed at the block where each match was found.
//
// Overlap handling is byte-precise, not match-granular. Each match is
// trimmed against the ranges already claimed at the same address (the
// resolvable spans that applied on the first pass, plus spans emitted for
// earlier matches): only the uncovered remainder sub-ranges become spans.
// A match whose every byte is already claimed emits nothing and instead
// marks its rule in the returned covered set — those bytes are replaced
// by the claiming spans, so the rule's content does not survive and must
// not force a degradation. Suppressing a partially-covered match outright
// would let its uncovered bytes persist unredacted whenever the rule is
// satisfied by another occurrence; conversely, emitting the overlapping
// range as-is would hand ApplySpans overlapping spans, and its
// descending-offset application only guarantees full byte replacement for
// disjoint spans (a lower-start span whose range extends past a
// higher-start span's replacement can leave that span's trailing original
// bytes in place, and badge offsets drift). Trimming keeps the disjoint
// invariant, at worst rendering one redaction marker per remainder
// sub-range of a single match.
func redetectSpans(p normcore.NormalizedPayload, ruleIDs []string, redetect Redetector, applied []normcore.TransformSpan) ([]normcore.TransformSpan, map[string]bool) {
	var out []normcore.TransformSpan
	covered := map[string]bool{}
	scan := func(addr, text string) {
		if text == "" {
			return
		}
		occupied := occupiedRanges(applied, addr, len(text))
		for _, m := range redetect(text, ruleIDs) {
			if m.RuleID == "" || m.Start < 0 || m.End > len(text) || m.Start >= m.End {
				continue
			}
			remainders := subtractRanges(m.Start, m.End, occupied)
			if len(remainders) == 0 {
				// Fully inside ranges other spans replace (this also
				// absorbs duplicate redetector output). The content is
				// redacted by those spans — count the rule as covered.
				covered[m.RuleID] = true
				continue
			}
			replacement := m.Replacement
			if replacement == "" {
				// Same convention as the hook pipeline's default
				// onMatch replacement template.
				replacement = "[REDACTED_" + strings.ToUpper(m.RuleID) + "]"
			}
			for _, r := range remainders {
				out = append(out, normcore.TransformSpan{
					Source:         normcore.SourceHook,
					SourceID:       m.RuleID,
					Action:         normcore.ActionRedact,
					ContentAddress: addr,
					Start:          r.start,
					End:            r.end,
					Replacement:    replacement,
				})
				occupied = append(occupied, r)
			}
		}
	}
	for mi, msg := range p.Messages {
		for ci, b := range msg.Content {
			switch b.Type {
			case normcore.ContentText, normcore.ContentReasoning:
				scan(fmt.Sprintf("messages.%d.content.%d", mi, ci), b.Text)
			case normcore.ContentToolResult:
				if b.ToolResult != nil {
					scan(fmt.Sprintf("messages.%d.content.%d.toolResult", mi, ci), b.ToolResult.Output)
				}
			}
		}
	}
	for ii, in := range p.Inputs {
		scan(fmt.Sprintf("inputs.%d", ii), in)
	}
	if p.HTTP != nil && p.HTTP.BodyView != nil {
		scan("http.bodyView", p.HTTP.BodyView.Text)
		keys := make([]string, 0, len(p.HTTP.BodyView.Form))
		for k := range p.HTTP.BodyView.Form {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			scan("http.bodyView.form."+k, p.HTTP.BodyView.Form[k])
		}
	}
	return out, covered
}

// byteRange is a half-open [start, end) byte range within one text block.
type byteRange struct {
	start, end int
}

// occupiedRanges projects the spans at addr onto byte ranges of a text of
// length n, clamped the same way applyToAddress clamps before replacing
// (start floored at 0, end capped at n). The input spans all applied on
// the first ApplySpans pass over this very payload, so every clamped
// range is within bounds by construction.
func occupiedRanges(spans []normcore.TransformSpan, addr string, n int) []byteRange {
	var out []byteRange
	for _, s := range spans {
		if s.ContentAddress != addr {
			continue
		}
		start, end := s.Start, s.End
		if start < 0 {
			start = 0
		}
		if end > n {
			end = n
		}
		out = append(out, byteRange{start: start, end: end})
	}
	return out
}

// subtractRanges returns the sub-ranges of [start, end) not covered by any
// occupied range, in ascending order. Occupied ranges may overlap each
// other and arrive in any order.
func subtractRanges(start, end int, occupied []byteRange) []byteRange {
	remaining := []byteRange{{start: start, end: end}}
	for _, o := range occupied {
		var next []byteRange
		for _, r := range remaining {
			if o.end <= r.start || o.start >= r.end {
				next = append(next, r)
				continue
			}
			if r.start < o.start {
				next = append(next, byteRange{start: r.start, end: o.start})
			}
			if o.end < r.end {
				next = append(next, byteRange{start: o.end, end: r.end})
			}
		}
		remaining = next
	}
	return remaining
}

// coversRules reports whether every rule ID is accounted for: attributed
// by at least one re-detected span, or marked covered because its matched
// content lies entirely within ranges other applied spans replace.
func coversRules(spans []normcore.TransformSpan, covered map[string]bool, ruleIDs []string) bool {
	found := make(map[string]bool, len(ruleIDs))
	for _, s := range spans {
		found[s.SourceID] = true
	}
	for _, id := range ruleIDs {
		if !found[id] && !covered[id] {
			return false
		}
	}
	return true
}

// subtractSpans returns the spans in all that are not in remove,
// preserving order. Identity is the same composite key ApplySpans uses to
// report skips.
func subtractSpans(all, remove []normcore.TransformSpan) []normcore.TransformSpan {
	removed := make(map[string]struct{}, len(remove))
	for _, s := range remove {
		removed[redactSpanKey(s)] = struct{}{}
	}
	out := make([]normcore.TransformSpan, 0, len(all))
	for _, s := range all {
		if _, ok := removed[redactSpanKey(s)]; ok {
			continue
		}
		out = append(out, s)
	}
	return out
}

func redactSpanKey(s normcore.TransformSpan) string {
	return fmt.Sprintf("%s|%d-%d|%s|%s", s.ContentAddress, s.Start, s.End, s.Source, s.SourceID)
}
