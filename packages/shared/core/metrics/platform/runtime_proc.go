//go:build !windows

package platform

import "os"

// osReadDir is the non-Windows shim for reading /proc/self/fd. On platforms
// without /proc (e.g. macOS), os.ReadDir returns an error and openFDCount
// falls back to 0.
func osReadDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}
