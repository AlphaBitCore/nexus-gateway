//go:build darwin

package platformshim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readWrittenBundles redirects the NE-read file to a temp dir, writes via
// WriteQUICFallbackBundlesFile, then reads back the JSON array the NE would
// consume. Redirecting the path keeps the test hermetic (no root / no /var/run).
func readWrittenBundles(t *testing.T, in []string) []string {
	t.Helper()
	orig := quicFallbackFilePath
	quicFallbackFilePath = filepath.Join(t.TempDir(), "quic-bundles.json")
	t.Cleanup(func() { quicFallbackFilePath = orig })

	if err := WriteQUICFallbackBundlesFile(in, nil); err != nil {
		t.Fatalf("WriteQUICFallbackBundlesFile: %v", err)
	}
	data, err := os.ReadFile(quicFallbackFilePath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// TestWriteQUICFallbackBundlesFile_StripsProtected is the SEC-M8-01 daemon-side
// defense-in-depth: even when a protected system bundle reaches the writer
// (e.g. an A3 node pushed it into the shadow, bypassing the CP reject gate),
// the file the NE reads must NOT contain it. Legitimate app targets survive.
func TestWriteQUICFallbackBundlesFile_StripsProtected(t *testing.T) {
	got := readWrittenBundles(t, []string{"com.google.Chrome", "com.apple.apsd", "com.apple", "com.apple.Safari"})
	for _, b := range got {
		if b == "com.apple.apsd" || b == "com.apple" {
			t.Errorf("protected entry %q reached the NE-read file; must be stripped (got %v)", b, got)
		}
	}
	hasChrome, hasSafari := false, false
	for _, b := range got {
		if b == "com.google.Chrome" {
			hasChrome = true
		}
		if b == "com.apple.Safari" {
			hasSafari = true
		}
	}
	if !hasChrome || !hasSafari {
		t.Errorf("legitimate app targets must survive the strip; got %v", got)
	}
}

// TestWriteQUICFallbackBundlesFile_AllSafePassThrough confirms the strip is a
// no-op when nothing is protected (no false positives on normal config).
func TestWriteQUICFallbackBundlesFile_AllSafePassThrough(t *testing.T) {
	got := readWrittenBundles(t, []string{"com.google.Chrome", "com.openai.chatgpt"})
	if len(got) != 2 {
		t.Errorf("safe bundles must pass through unchanged; got %v", got)
	}
}
