//go:build darwin

package proc

import (
	"os"
	"strings"
)

// hasAppSuffix reports whether path ends with ".app".
func hasAppSuffix(path string) bool {
	return strings.HasSuffix(path, ".app")
}

// readFile reads the named file and returns its contents.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// indexString returns the index of sep in s, or -1.
func indexString(s, sep string) int {
	return strings.Index(s, sep)
}

// trimSpace returns a slice of s with all leading and trailing
// white space removed, as defined by Unicode.
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}
