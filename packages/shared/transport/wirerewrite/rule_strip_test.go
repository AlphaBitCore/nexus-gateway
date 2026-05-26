package wirerewrite

import (
	"regexp"
	"testing"
)

func TestApplyStripRule_NilRegexFailsOpen(t *testing.T) {
	body := []byte(`{"system":"keep me"}`)
	out, c, r := applyStripRule(body, "system", &Rule{Regex: nil})
	if string(out) != string(body) || c != 0 || r != 0 {
		t.Errorf("nil regex: got %s c=%d r=%d, want original/0/0", out, c, r)
	}
}

func TestApplyStripRule_PathAbsentFailsOpen(t *testing.T) {
	body := []byte(`{"other":"x"}`)
	rule := &Rule{Regex: regexp.MustCompile(`secret`)}
	out, c, r := applyStripRule(body, "system", rule)
	if string(out) != string(body) || c != 0 || r != 0 {
		t.Errorf("absent path: got %s c=%d r=%d", out, c, r)
	}
}

func TestApplyStripRule_StringValueStripped(t *testing.T) {
	body := []byte(`{"system":"hello secret world"}`)
	rule := &Rule{Regex: regexp.MustCompile(`secret\s*`)}
	out, c, r := applyStripRule(body, "system", rule)
	if c != 1 {
		t.Errorf("count: %d want 1", c)
	}
	if r != 7 { // "secret " = 7 chars
		t.Errorf("removed bytes: %d want 7", r)
	}
	if string(out) == string(body) {
		t.Errorf("body not modified: %s", out)
	}
}

func TestApplyStripRule_NoMatchPreservesBody(t *testing.T) {
	body := []byte(`{"system":"clean text"}`)
	rule := &Rule{Regex: regexp.MustCompile(`secret`)}
	out, c, r := applyStripRule(body, "system", rule)
	if string(out) != string(body) || c != 0 || r != 0 {
		t.Errorf("no match should preserve: out=%s c=%d r=%d", out, c, r)
	}
}

func TestApplyStripRule_ArrayElementsStripped(t *testing.T) {
	// Anthropic-shape system array — each element's "text" field
	// gets stripped independently.
	body := []byte(`{"system":[{"text":"a secret here"},{"text":"another secret"}]}`)
	rule := &Rule{Regex: regexp.MustCompile(`secret`)}
	out, c, r := applyStripRule(body, "system.#.text", rule)
	if c != 2 {
		t.Errorf("count: %d want 2", c)
	}
	if r != 12 { // "secret" x 2 = 12 bytes
		t.Errorf("removed: %d want 12", r)
	}
	if string(out) == string(body) {
		t.Error("body not modified")
	}
}

func TestApplyStripRule_ArrayMixedTypesSkipsNonString(t *testing.T) {
	// Only string array elements should be candidates; numeric / object
	// entries are skipped silently rather than failing the whole pass.
	body := []byte(`{"system":[{"text":"strip me secret"},{"text":42}]}`)
	rule := &Rule{Regex: regexp.MustCompile(`secret`)}
	out, c, _ := applyStripRule(body, "system.#.text", rule)
	if c != 1 {
		t.Errorf("count: %d want 1 (numeric skipped)", c)
	}
	_ = out
}

func TestResolveArrayPath_ReplacesFirstHash(t *testing.T) {
	if got := resolveArrayPath("system.#.text", 0); got != "system.0.text" {
		t.Errorf("got %q", got)
	}
	if got := resolveArrayPath("system.#.text", 42); got != "system.42.text" {
		t.Errorf("got %q", got)
	}
}

func TestResolveArrayPath_NoHashReturnsAsIs(t *testing.T) {
	// Defensive: no # in path means no array selector to resolve.
	if got := resolveArrayPath("system.text", 0); got != "system.text" {
		t.Errorf("got %q", got)
	}
}

func TestResolveArrayPath_OnlyFirstHashReplaced(t *testing.T) {
	// `messages.#.content.#.text` style paths: only the first # is
	// replaced by the outer iterator; the inner # is left for nested
	// processing if any. Critical: replacing all `#` would corrupt
	// nested-array paths.
	got := resolveArrayPath("messages.#.content.#.text", 3)
	if got != "messages.3.content.#.text" {
		t.Errorf("got %q want messages.3.content.#.text", got)
	}
}
