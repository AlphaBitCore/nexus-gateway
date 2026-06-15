package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUserQuitFlagShouldExit is the F-0312 regression: the user-quit flag must
// be honored only when quitAllowed is true. On a quitAllowed=false (locked /
// compliance) fleet the flag is ignored, so a local user who plants the flag
// file cannot defeat the no-quit policy.
func TestUserQuitFlagShouldExit(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "user-quit")
	if err := os.WriteFile(present, []byte("x"), 0644); err != nil {
		t.Fatalf("write flag: %v", err)
	}
	absent := filepath.Join(dir, "no-such-flag")

	cases := []struct {
		name        string
		flagPath    string
		quitAllowed bool
		want        bool
	}{
		{"present + quitAllowed → exit", present, true, true},
		{"present + quitNOTallowed → IGNORE (F-0312)", present, false, false},
		{"absent + quitAllowed → stay", absent, true, false},
		{"absent + quitNOTallowed → stay", absent, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := userQuitFlagShouldExit(tc.flagPath, tc.quitAllowed); got != tc.want {
				t.Errorf("userQuitFlagShouldExit(%q, quitAllowed=%v) = %v; want %v",
					tc.flagPath, tc.quitAllowed, got, tc.want)
			}
		})
	}
}
