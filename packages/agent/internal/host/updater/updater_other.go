//go:build !darwin

package updater

import (
	"os/exec"
	"syscall"
)

// installerCommand is unreachable on non-darwin builds — pkgInstallDarwin (its
// only caller) runs only when osName == "darwin". It is defined so the
// cross-platform updater.go compiles everywhere; the `false` command fails
// safely if it is ever invoked off macOS.
var installerCommand = func(_ string) *exec.Cmd {
	return exec.Command("false")
}

// installerSysProcAttr is the non-darwin counterpart. Setsid is a Unix-only
// field, and pkgInstallDarwin never runs off macOS, so nil (no special attrs)
// is correct and keeps the cross-platform updater.go compiling on Windows.
func installerSysProcAttr() *syscall.SysProcAttr {
	return nil
}
