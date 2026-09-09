package iam

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Control Plane UI decides which affordances to render from ACTION_MAP in
// packages/control-plane-ui/src/hooks/usePermission.ts, which maps a UI key to
// an IAM action string. This catalog is the authority for what those strings
// may be — and nothing checked that the map's values actually EXIST here.
//
// Both sides validated only the REGEX SHAPE (TestActionRegexShape above, and
// the frontend's usePermission.coverage.test.ts). A typo, a renamed verb, or an
// action invented from a plausible-looking pattern all pass a shape check —
// and then `usePermission` returns false for every principal, because nobody
// holds an action that does not exist. The control disappears for everyone,
// including the super-admin, and nothing errors: exactly the invisible-button
// failure the hook's own dev warning exists to catch, except that warning only
// fires for UNMAPPED keys, never for mapped-to-nonexistent ones.
//
// This gate lives on the Go side because the catalog is here. It reads the TS
// file as data.
func TestActionMapValuesExistInCatalog(t *testing.T) {
	path := controlPlaneUIPermissionHook(t)
	raw, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)

	start := strings.Index(src, "export const ACTION_MAP")
	if start < 0 {
		t.Fatalf("%s: ACTION_MAP not found — was it renamed? This gate is now blind.", path)
	}
	end := strings.Index(src[start:], "\n};")
	if end < 0 {
		t.Fatalf("%s: could not find the end of ACTION_MAP", path)
	}
	body := src[start : start+end]

	// Only `'key': 'value',` entry lines; comment lines inside the literal are
	// skipped because the pattern requires the quoted pair.
	entry := regexp.MustCompile(`'([^']+)':\s*'([^']+)'`)
	matches := entry.FindAllStringSubmatch(body, -1)

	// Non-vacuity: a regex that matched nothing would make this test pass
	// while checking nothing at all.
	if len(matches) < 50 {
		t.Fatalf("parsed only %d ACTION_MAP entries from %s — the parse is broken, not the map", len(matches), path)
	}

	valid := map[string]bool{}
	for _, a := range AllActions() {
		valid[a] = true
	}
	if len(valid) < 50 {
		t.Fatalf("AllActions() returned %d actions — the catalog side of this comparison is broken", len(valid))
	}

	var unknown []string
	for _, m := range matches {
		key, action := m[1], m[2]
		if !valid[action] {
			unknown = append(unknown, key+" -> "+action)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Errorf("ACTION_MAP names %d action(s) this catalog does not define. usePermission returns false for every principal on these, so the affordance is hidden from EVERYONE with no error:\n  %s",
			len(unknown), strings.Join(unknown, "\n  "))
	}
}

// controlPlaneUIPermissionHook resolves the UI hook from this package, walking
// up to the repo's packages/ directory. Path-based, so it crosses the module
// boundary between shared/ and the UI workspace.
func controlPlaneUIPermissionHook(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 12 {
		if filepath.Base(dir) == "packages" {
			p := filepath.Join(dir, "control-plane-ui", "src", "hooks", "usePermission.ts")
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("expected the UI permission hook at %s: %v", p, err)
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
