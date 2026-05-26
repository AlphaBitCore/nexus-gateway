//go:build !windows

package platformshim

import (
	"fmt"
	"os"
)

// CmdInstallWfpCheck is the Windows MSI custom-action helper.
// On non-Windows builds it's a guarded no-op: invoking it from a
// macOS or Linux build is a packaging bug, not a runtime concern.
func CmdInstallWfpCheck(_ []string) int {
	fmt.Fprintln(os.Stderr,
		"install-wfp-check is Windows-only; this is a no-op on this platform")
	return 0
}
