package dsarstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The erasure receipt is the one screen an operator uses to confirm an Art.17
// request was honoured, and it shipped reporting zero rows anonymised whatever
// the erasure actually did. The UI declared mode / vkRowsAnonymised /
// proxyRowsAnonymised / fulfilledAt; the handler returns this struct, whose
// wire names are vkAnonymised / agentAnonymised / totalAnonymised / … . Not one
// name matched, so every read was undefined and every count rendered 0 —
// affirmative evidence of a no-op that did not occur.
//
// Nothing could catch that. TypeScript parses the response AS the declared
// type, so a name that is never sent is simply undefined at runtime; and a Go
// test asserting struct FIELDS would have passed too, because the fields were
// right and only their json tags mattered. So this test marshals a real value
// and compares the WIRE names against the names the shipped .ts file declares.
//
// Note the trap it also closes: the handler persists a DIFFERENT map to
// dsar_request.outcome — one that does carry `mode` and `fulfilledAt`. Reading
// that map, rather than this struct, is how the UI's declaration came to name
// two fields that genuinely exist somewhere but never on this response.

// wireFieldsOfErasureResult marshals the struct the fulfil handler returns as
// `outcome` and reports the field names that actually reach the wire.
func wireFieldsOfErasureResult(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(DSARErasureResult{})
	if err != nil {
		t.Fatalf("marshal DSARErasureResult: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	// Non-vacuity: an empty or near-empty marshal would make every assertion
	// below pass for the wrong reason.
	if len(out) < 8 {
		t.Fatalf("DSARErasureResult marshalled to only %d field(s) — the struct or its tags are broken, not the UI", len(out))
	}
	return out
}

// uiDeclaredOutcomeFields reads the shipped dsar.ts and returns the field names
// declared inside the ERASURE `outcome?: { … }` block.
func uiDeclaredOutcomeFields(t *testing.T) []string {
	t.Helper()
	path := controlPlaneUIDSARService(t)
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)

	start := strings.Index(src, "outcome?: {")
	if start < 0 {
		t.Fatalf("%s: could not find the `outcome?: {` block — was it renamed? This test is now blind.", path)
	}
	end := strings.Index(src[start:], "};")
	if end < 0 {
		t.Fatalf("%s: could not find the end of the outcome block", path)
	}
	body := src[start : start+end]

	field := regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\??\s*:`)
	var names []string
	for _, m := range field.FindAllStringSubmatch(body, -1) {
		if m[1] == "outcome" {
			continue
		}
		names = append(names, m[1])
	}
	if len(names) < 3 {
		t.Fatalf("parsed only %d field(s) from the outcome block in %s — the parse is broken, not the contract", len(names), path)
	}
	return names
}

func TestErasureReceiptFieldsReachTheWire(t *testing.T) {
	wire := wireFieldsOfErasureResult(t)
	declared := uiDeclaredOutcomeFields(t)

	var missing []string
	for _, name := range declared {
		if !wire[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		var have []string
		for k := range wire {
			have = append(have, k)
		}
		sort.Strings(have)
		t.Errorf("the erasure receipt declares %d field(s) this response never sends, so each renders as "+
			"undefined and its count as 0 on the screen that confirms an Art.17 erasure:\n  missing: %s\n  on the wire: %s",
			len(missing), strings.Join(missing, ", "), strings.Join(have, ", "))
	}
}

// controlPlaneUIDSARService resolves the UI's DSAR service module from this
// package, walking up to the repo's packages/ directory.
func controlPlaneUIDSARService(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 12 {
		if filepath.Base(dir) == "packages" {
			p := filepath.Join(dir, "control-plane-ui", "src", "api", "services", "compliance", "dsar.ts")
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("expected the UI DSAR service at %s: %v", p, err)
			}
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the packages/ directory from this test")
	return ""
}
