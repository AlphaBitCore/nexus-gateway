//go:build !darwin

package platformshim

import "log/slog"

// writeQUICFallbackBundlesFile is a no-op on non-darwin platforms.
// The bundle-ID allowlist exists exclusively to drive the macOS NE
// extension's UDP-close decision; Linux/Windows agents intercept
// traffic differently (transparent-proxy port, kernel filter) and
// have no equivalent QUIC-fallback mechanism wired here.
func WriteQUICFallbackBundlesFile(_ []string, _ *slog.Logger) error {
	return nil
}

// anySlice mirrors the darwin helper — kept on non-darwin builds so
// main.go compiles unchanged across platforms.
func AnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
