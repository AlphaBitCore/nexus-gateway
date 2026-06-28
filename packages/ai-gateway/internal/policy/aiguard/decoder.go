// packages/ai-gateway/internal/policy/aiguard/decoder.go
package aiguard

import (
	"fmt"
	"github.com/goccy/go-json"
	"regexp"
	"sort"
	"strings"
)

// fencePattern matches the common ```json ... ``` wrapper some LLMs emit.
// We pull the first JSON-fenced block; if absent, the whole input is
// assumed to be JSON (possibly with leading prose — we also attempt
// best-effort JSON extraction with a brace-matching fallback).
var fencePattern = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// DecodeJudgeOutput parses the judge backend's response text into the
// Response shape. Handles three common cases:
//
//  1. Raw JSON — parse directly.
//  2. Markdown-fenced JSON (```json ... ```) — extract and parse.
//  3. Mixed prose + JSON — find the first balanced {...} and parse.
//
// Returns an error if none of the above yield a valid Response, or the
// decision value is not one of {approve, reject_hard, block_soft, modify}.
// Confidence is clamped to [0.0, 1.0]; labels are trimmed, lowercased,
// and deduplicated in sorted order for stable tag emission.
//
// Redactions go through sanitizeRedactions which drops entries where
// start > end, end <= start, or both are zero with no replacement.
// Action defaults to "redact" when the judge omits the field.
func DecodeJudgeOutput(raw string) (*Response, error) {
	jsonStr := extractJSON(raw)
	if jsonStr == "" {
		return nil, fmt.Errorf("aiguard: no JSON object in judge output")
	}
	var r Response
	if err := json.Unmarshal([]byte(jsonStr), &r); err != nil {
		return nil, fmt.Errorf("aiguard: parse judge output: %w", err)
	}
	switch r.Decision {
	case "approve", "reject_hard", "block_soft", "modify":
		// ok
	default:
		return nil, fmt.Errorf("aiguard: invalid decision %q", r.Decision)
	}
	if r.Confidence < 0 {
		r.Confidence = 0
	} else if r.Confidence > 1 {
		r.Confidence = 1
	}
	r.Labels = normalizeLabels(r.Labels)
	r.Redactions = sanitizeRedactions(r.Redactions)
	return &r, nil
}

// sanitizeRedactions normalises the judge's redactions array:
//   - drops entries with end <= start (zero-width or inverted)
//   - clamps negative offsets to 0
//   - defaults Action to "redact" when omitted or invalid
//   - sorts ascending by Start so callers can apply with simple
//     descending-iteration logic that stays offset-safe (matching the
//     contract used by normalize.ApplySpans)
func sanitizeRedactions(in []Redaction) []Redaction {
	if len(in) == 0 {
		return nil
	}
	out := make([]Redaction, 0, len(in))
	for _, r := range in {
		if r.Start < 0 {
			r.Start = 0
		}
		if r.End <= r.Start {
			continue
		}
		switch r.Action {
		case "redact", "strip", "replace":
			// ok
		case "":
			r.Action = "redact"
		default:
			r.Action = "redact"
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End < out[j].End
	})
	return out
}

// extractJSON pulls the first JSON object from s via fence detection or
// brace-matching. Returns "" when no candidate is found.
func extractJSON(s string) string {
	if m := fencePattern.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// normalizeLabels trims, lowercases, and dedupes labels in sorted order.
func normalizeLabels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	for _, lbl := range in {
		t := strings.ToLower(strings.TrimSpace(lbl))
		if t == "" {
			continue
		}
		seen[t] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
