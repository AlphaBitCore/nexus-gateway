//go:build darwin

package updater

import (
	"os/exec"
	"syscall"
)

// installerCommand builds the macOS `installer` invocation. Package var so
// tests can dispatch the .pkg path without launching the real installer.
var installerCommand = func(pkgPath string) *exec.Cmd {
	return exec.Command("/usr/sbin/installer", "-pkg", pkgPath, "-target", "/")
}

// installerSysProcAttr starts the installer in a new session (Setsid) so it
// outlives the daemon bootout the .pkg preinstall triggers. Setsid is a
// Unix-only field of syscall.SysProcAttr, so this lives in the darwin file.
func installerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
