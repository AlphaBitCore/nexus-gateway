//go:build darwin

package updater

import "os/exec"

// installerCommand builds the macOS `installer` invocation. Package var so
// tests can dispatch the .pkg path without launching the real installer.
var installerCommand = func(pkgPath string) *exec.Cmd {
	return exec.Command("/usr/sbin/installer", "-pkg", pkgPath, "-target", "/")
}
