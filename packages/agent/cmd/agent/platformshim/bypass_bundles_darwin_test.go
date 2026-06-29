//go:build darwin

package platformshim

import (
	"github.com/goccy/go-json"
	"os"
	"path/filepath"
	"testing"
)

// writeBypass redirects the NE-read file to a temp dir, writes via
// WriteBypassBundlesFile, then returns the on-disk path + decoded array so
// tests can assert both the JSON shape and the file mode the unsandboxed NE
// must be able to read.
func writeBypass(t *testing.T, in []string) (string, []string) {
	t.Helper()
	orig := bypassBundlesFilePath
	bypassBundlesFilePath = filepath.Join(t.TempDir(), "bypass-bundles.json")
	t.Cleanup(func() { bypassBundlesFilePath = orig })

	if err := WriteBypassBundlesFile(in, nil); err != nil {
		t.Fatalf("WriteBypassBundlesFile: %v", err)
	}
	data, err := os.ReadFile(bypassBundlesFilePath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return bypassBundlesFilePath, out
}

// TestWriteBypassBundlesFile_Roundtrip confirms the exact bundles an admin
// sets reach the NE-read file unchanged (no stripping — bypass is a
// not-inspected list, not a kill list).
func TestWriteBypassBundlesFile_Roundtrip(t *testing.T) {
	_, got := writeBypass(t, []string{"com.anthropic.claude-code", "com.apple.apsd"})
	if len(got) != 2 || got[0] != "com.anthropic.claude-code" || got[1] != "com.apple.apsd" {
		// com.apple.apsd is intentionally NOT stripped: exempting it from
		// inspection is harmless (system daemons aren't inspected anyway).
		t.Fatalf("bypass list must pass through verbatim, got %v", got)
	}
}

// TestWriteBypassBundlesFile_EmptyIsFailSafe confirms the empty-as-fail-safe
// contract: nil/empty serializes to the literal "[]" (exempt nothing /
// inspect everything), never "null" and never a missing file the NE would
// have to special-case.
func TestWriteBypassBundlesFile_EmptyIsFailSafe(t *testing.T) {
	path, got := writeBypass(t, nil)
	if len(got) != 0 {
		t.Fatalf("empty input must write an empty exemption list, got %v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("empty list must serialize to %q, got %q", "[]", string(raw))
	}
}

// TestWriteBypassBundlesFile_WorldReadable confirms the file mode lets the
// unsandboxed NE extension process (different uid context) read it.
func TestWriteBypassBundlesFile_WorldReadable(t *testing.T) {
	path, _ := writeBypass(t, []string{"codex"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0044 == 0 {
		t.Fatalf("file mode %o is not world/group-readable; NE cannot read it", perm)
	}
}
