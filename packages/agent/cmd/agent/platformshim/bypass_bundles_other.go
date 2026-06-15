//go:build !darwin

package platformshim

import "log/slog"

// WriteBypassBundlesFile is a no-op on non-darwin platforms. The
// source-bundle exemption list exists exclusively to drive the macOS NE
// extension's per-flow bump/passthrough decision; Linux/Windows agents
// intercept traffic differently and have no equivalent file the kernel
// filter reads here.
func WriteBypassBundlesFile(_ []string, _ *slog.Logger) error {
	return nil
}
