//go:build !darwin

package updater

import "os/exec"

// installerCommand is unreachable on non-darwin builds — pkgInstallDarwin (its
// only caller) runs only when osName == "darwin". It is defined so the
// cross-platform updater.go compiles everywhere; the `false` command fails
// safely if it is ever invoked off macOS.
var installerCommand = func(_ string) *exec.Cmd {
	return exec.Command("false")
}
