//go:build !linux && !darwin && !windows

package platformshim

import (
	"fmt"
	"os"
)

// cmdInstallCA on truly-unsupported OSes (e.g. freebsd build hosts) just
// reports unsupported. The three production targets each have their own
// file (install_ca_{darwin,linux,windows}.go).
func CmdInstallCA(_ []string) int {
	fmt.Fprintln(os.Stderr,
		"install-ca is unsupported on this OS")
	return 1
}
