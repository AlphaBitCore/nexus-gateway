package codec

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/goccy/go-json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// FieldRule is one per-model wire rule on a body-root field: remove the
// field, rename it, or force it to a fixed value, when the rule's model
// gate matches. Rules are the data form of the §3a Rule 3 per-model wire
// quirks — declared once per adapter next to their observed-400 evidence
// (specs/<name>/rewrites) and applied by the identity codec from both of
// its entry points, so the canonical door (EncodeRequest) and the native
// door (RewriteNative) cannot drift apart.
type FieldRule struct {
	// Applies gates the rule on the resolved provider model id. Required.
	Applies func(modelID string) bool
	// Field is the body-root field the rule edits. Required; must be a
	// plain key (no gjson path syntax) and no two rules on one wire may
	// name each other's fields — both enforced at construction.
	Field string
	// RenameTo, when non-empty, makes the rule a rename: Field's value
	// moves to RenameTo when RenameTo is absent (a caller-supplied
	// RenameTo wins), and Field is removed either way. Empty means remove.
	RenameTo string
	// SetRaw, when non-empty, makes the rule a forced value: Field is set
	// to this raw JSON literal when it is absent or carries any other
	// value. Mutually exclusive with RenameTo; must be a scalar literal
	// (string/number/bool/null — container literals cannot be compared
	// for the already-correct skip and are rejected at construction).
	// Used for wires that demand an explicit value (a model family whose
	// default is itself rejected in some body shape).
	SetRaw string
	// WhenPresentNonEmpty, when non-empty, further gates the rule on the
	// body: it fires only when this body-root field carries at least one
	// element (probed as `<field>.0` — one sub-element read, never a copy
	// or parse of the whole array; a scalar or empty-array field does not
	// fire the rule).
	WhenPresentNonEmpty string
	// Label overrides the reported rewrite string. Empty derives
	// "<field>→removed", "<field>→<renameTo>" or "<field>→<setRaw>".
	Label string
}

func (r FieldRule) label() string {
	if r.Label != "" {
		return r.Label
	}
	if r.RenameTo != "" {
		return r.Field + "→" + r.RenameTo
	}
	if r.SetRaw != "" {
		return r.Field + "→" + strings.Trim(r.SetRaw, `"`)
	}
	return r.Field + "→removed"
}

// StructuralRule is the rule class for per-model quirks a body-root
// FieldRule cannot express: value-conditional removals and edits INSIDE
// nested structures (message-array back-fills). A structural rule always
// executes on the decode door — its logic needs the parsed form — so a
// gate-matching model's body routes to one map round-trip, exactly what
// the transitional dispatch callback cost for the same models. Models
// whose gate does not match pay nothing.
type StructuralRule struct {
	// Applies gates the rule on the resolved provider model id. Required.
	Applies func(modelID string) bool
	// Apply edits the decoded payload in place and returns the reported
	// rewrites ("<from>→<to>" / "<field>→removed" / descriptive). It must
	// be idempotent: applied to its own output it returns nothing.
	Apply func(payload map[string]any, modelID string) []string
}

// Contract is one sibling's per-model wire-rule set, one list per request
// wire that carries the affected fields at the body root, plus the
// chat-wire structural rules. The zero value is the empty contract —
// stamp-and-streaming behaviour only, no per-model rules — which is
// correct for OpenAI-compatible siblings with no probed wire quirk (the
// forward-unprobed rows in scripts/quirk-coverage.config.mjs). The
// identity codec constructor takes a Contract explicitly so a sibling
// states its rules, or their absence, on its own wiring line; there is
// no shared default a new sibling could silently inherit.
//
// The legacy /v1/completions wire deliberately has no rule list:
// max_tokens is the correct parameter name there and no evidence-cited
// rule targets that wire.
type Contract struct {
	Chat       []FieldRule
	Responses  []FieldRule
	Embeddings []FieldRule
	// ChatStructural rules run on the chat wire's decode door for
	// gate-matching models (see StructuralRule).
	ChatStructural []StructuralRule
}

// ruleSet is one wire's rule list with its probe paths and report labels
// precomputed at construction. Probing is per-gate-matching-rule: gjson
// has no genuinely fused multi-path scan (GetManyBytes is a per-path
// loop), so the honest cost model is one O(body) walk per probed path —
// and the way to keep the hot path cheap is to probe ONLY the paths of
// rules whose model gate matched, nothing speculative. Rules whose gate
// does not match cost zero body reads.
type ruleSet struct {
	rules []FieldRule
	// condPaths[i] is rules[i].WhenPresentNonEmpty + ".0" ("" when the
	// rule has no condition): probing the FIRST ELEMENT answers
	// "present with at least one element" in one sub-element read,
	// instead of copying (gjson safety-copies Raw on every hit) and
	// parsing the whole array.
	condPaths []string
	// labels[i] is rules[i].label(), precomputed — deriving it on the
	// edit path allocates per request for a value that is static per rule.
	labels []string
}

func newRuleSet(rules []FieldRule) ruleSet {
	if len(rules) > 64 {
		// due-ness rides uint64 masks; a wire contract that large is a
		// programming error caught at adapter construction.
		panic(fmt.Sprintf("codec: contract rule list too large (%d > 64)", len(rules)))
	}
	s := ruleSet{rules: rules}
	names := map[string]int{}
	for i, r := range rules {
		if r.RenameTo != "" && r.SetRaw != "" {
			panic(fmt.Sprintf("codec: rule %s declares both RenameTo and SetRaw", r.Field))
		}
		if r.SetRaw != "" {
			// The already-correct skip compares decoded values on the map
			// door; container values are not comparable and would panic at
			// request time. Reject at construction instead.
			var v any
			if err := json.Unmarshal([]byte(r.SetRaw), &v); err != nil {
				panic(fmt.Sprintf("codec: rule %s SetRaw %q is not valid JSON", r.Field, r.SetRaw))
			}
			switch v.(type) {
			case map[string]any, []any:
				panic(fmt.Sprintf("codec: rule %s SetRaw %q must be a scalar literal", r.Field, r.SetRaw))
			}
		}
		// The two doors evaluate rules against different snapshots (the
		// surgical door decides due-ness before editing; the map door is
		// sequential on current state). That is only equivalent when no
		// rule's field is another rule's field, rename target, or
		// condition — enforce the independence instead of assuming it.
		// Plain keys only: a gjson path metacharacter would silently
		// address a nested path on the probe side while the map door
		// edits a literal top-level key.
		for name, kind := range map[string]string{r.Field: "field", r.RenameTo: "rename target", r.WhenPresentNonEmpty: "condition"} {
			if name == "" {
				continue
			}
			if strings.ContainsAny(name, ".*?|#@\\") {
				panic(fmt.Sprintf("codec: rule %s %s %q contains gjson path syntax; rules address plain body-root keys only", r.Field, kind, name))
			}
		}
		if prev, dup := names[r.Field]; dup {
			panic(fmt.Sprintf("codec: rules %d and %d both address field %s", prev, i, r.Field))
		}
		names[r.Field] = i
	}
	for i, r := range rules {
		for j, other := range rules {
			if i == j {
				continue
			}
			if r.Field == other.RenameTo || r.Field == other.WhenPresentNonEmpty {
				panic(fmt.Sprintf("codec: rule %d field %s is rule %d's rename target or condition; rules must be field-independent", i, r.Field, j))
			}
			if r.RenameTo != "" && r.RenameTo == other.WhenPresentNonEmpty {
				// A rename that CREATES another rule's condition field would
				// make due-ness order-dependent: the surgical door decides
				// all due bits pre-edit, the map door is sequential, and the
				// two would disagree on whether the conditioned rule fires.
				panic(fmt.Sprintf("codec: rule %d rename target %s is rule %d's condition; rules must be field-independent", i, r.RenameTo, j))
			}
		}
	}
	s.condPaths = make([]string, len(rules))
	s.labels = make([]string, len(rules))
	for i, r := range rules {
		if r.WhenPresentNonEmpty != "" {
			s.condPaths[i] = r.WhenPresentNonEmpty + ".0"
		}
		s.labels[i] = r.label()
	}
	return s
}

// dueMask reports which rules gate-match modelID without reading the
// body. Zero means the body needs no probe at all — the empty-contract /
// non-quirk-model fast path.
func (s *ruleSet) dueMask(modelID string) uint64 {
	var due uint64
	for i, r := range s.rules {
		if r.Applies(modelID) {
			due |= 1 << i
		}
	}
	return due
}

// probeDue reads the body once per gate-matching rule and returns which
// rules the BODY makes fire, plus each probed field result (indexed by
// rule) for the edit loop. Remove/rename rules fire when their field is
// present; SetRaw rules fire when the condition (if any) has a first
// element and the field is absent or carries a different raw value.
//
// Known accepted limitations (fail-visible only, pathological bodies):
// gjson reads the FIRST occurrence of a duplicated key while JSON parsers
// take last-wins, so a body duplicating a rule field or condition can
// make a rule not fire here that a full parse would fire; and an
// OBJECT-valued condition with a "0" key satisfies the `<field>.0` probe
// while the map door requires a non-empty array. Both classes forward
// bytes the upstream rejects anyway (wire = audit) and no wrong-value
// edit is possible because nothing is edited when a rule is not due.
func (s *ruleSet) probeDue(body []byte, gate uint64) (uint64, []gjson.Result) {
	fields := make([]gjson.Result, len(s.rules))
	var dueNow uint64
	for i, r := range s.rules {
		if gate&(1<<i) == 0 {
			continue
		}
		if s.condPaths[i] != "" && !gjson.GetBytes(body, s.condPaths[i]).Exists() {
			continue
		}
		fields[i] = gjson.GetBytes(body, r.Field)
		if r.SetRaw != "" {
			if !fields[i].Exists() || fields[i].Raw != r.SetRaw {
				dueNow |= 1 << i
			}
			continue
		}
		if fields[i].Exists() {
			dueNow |= 1 << i
		}
	}
	return dueNow, fields
}

// anyFieldPresent reports whether any gate-matching rule is due against
// body. False without reading the body when no rule gates on modelID.
func (s *ruleSet) anyFieldPresent(body []byte, modelID string) bool {
	gate := s.dueMask(modelID)
	if gate == 0 || len(body) == 0 {
		return false
	}
	dueNow, _ := s.probeDue(body, gate)
	return dueNow != 0
}

// applyBytes is the surgical door: per-gate-matching-rule probes, then
// sjson edits on exactly the due fields. ok=false means the body must
// take the decode door instead — a field about to be edited is duplicated
// (sjson edits the FIRST occurrence while JSON parsers take last-wins, so
// a surgical edit on that shape would target the wrong value), or an
// sjson edit failed. The fast path — nothing due — returns the same slice.
func (s *ruleSet) applyBytes(body []byte, modelID string) ([]byte, []string, bool) {
	gate := s.dueMask(modelID)
	if gate == 0 || len(body) == 0 {
		return body, nil, true
	}
	dueNow, fields := s.probeDue(body, gate)
	if dueNow == 0 {
		return body, nil, true
	}
	// Dup-key precondition, per field about to be edited (present-and-due
	// only; a SetRaw on an absent field creates the key, nothing to
	// mistarget). bytes.Count over-matches (the key inside a string value
	// or a nested object counts too); that only sends more bodies to the
	// always-correct decode door, never the reverse. It runs only here,
	// where an edit is due — never as a hoisted scan on paths that edit
	// nothing.
	for i, r := range s.rules {
		if dueNow&(1<<i) == 0 || !fields[i].Exists() {
			continue
		}
		if bytes.Count(body, []byte(`"`+r.Field+`"`)) > 1 {
			return nil, nil, false
		}
	}
	var rewrites []string
	for i, r := range s.rules {
		if dueNow&(1<<i) == 0 {
			continue
		}
		if r.SetRaw != "" {
			set, err := sjson.SetRawBytes(body, r.Field, []byte(r.SetRaw))
			if err != nil {
				return nil, nil, false
			}
			body = set
			rewrites = append(rewrites, s.labels[i])
			continue
		}
		// Re-read on the current body: earlier edits shift positions.
		// (Due-ness itself cannot change across edits — construction
		// enforces that no rule's field is another rule's field, rename
		// target, or condition.)
		cur := gjson.GetBytes(body, r.Field)
		if !cur.Exists() {
			continue
		}
		if r.RenameTo != "" && !gjson.GetBytes(body, r.RenameTo).Exists() {
			moved, err := sjson.SetRawBytes(body, r.RenameTo, []byte(cur.Raw))
			if err != nil {
				return nil, nil, false
			}
			body = moved
		}
		stripped, err := sjson.DeleteBytes(body, r.Field)
		if err != nil {
			return nil, nil, false
		}
		body = stripped
		rewrites = append(rewrites, s.labels[i])
	}
	return body, rewrites, true
}

// applyMap is the decode door's half of the same rules: it rides a
// payload the caller was forced to decode anyway (streaming back-fills, a
// dup-key fallback), where map edits cost nothing extra and key
// de-duplication makes the edit semantics exact.
func (s *ruleSet) applyMap(payload map[string]any, modelID string) []string {
	var rewrites []string
	for i, r := range s.rules {
		if !r.Applies(modelID) {
			continue
		}
		if r.WhenPresentNonEmpty != "" {
			arr, isArr := payload[r.WhenPresentNonEmpty].([]any)
			if !isArr || len(arr) == 0 {
				continue
			}
		}
		if r.SetRaw != "" {
			var want any
			// Valid by construction (newRuleSet rejects non-JSON SetRaw).
			if err := json.Unmarshal([]byte(r.SetRaw), &want); err != nil {
				continue
			}
			// Scalar by construction, so the comparison cannot panic. An
			// escape-variant scalar ("none") decodes equal here while
			// the bytes door compares raw literals and would rewrite; the
			// wire result is identical either way — only the report can
			// differ, on a body only a pathological client sends.
			if cur, has := payload[r.Field]; has && cur == want {
				continue
			}
			payload[r.Field] = want
			rewrites = append(rewrites, s.labels[i])
			continue
		}
		v, ok := payload[r.Field]
		if !ok {
			continue
		}
		if r.RenameTo != "" {
			if _, has := payload[r.RenameTo]; !has {
				payload[r.RenameTo] = v
			}
		}
		delete(payload, r.Field)
		rewrites = append(rewrites, s.labels[i])
	}
	return rewrites
}
