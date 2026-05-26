//go:build darwin

package platform

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// staticInfoCmdTimeout caps each best-effort shell-out to system info
// utilities. CaptureStaticInfo runs once at startup and tolerates partial
// failure, so a short ceiling here protects boot time on a sick host.
const staticInfoCmdTimeout = 2 * time.Second

// runShellOnceFn is the function used to execute one-shot shell commands for
// hardware/OS introspection. It is a package-level variable so tests can
// inject deterministic stubs without requiring PATH manipulation (which is
// not race-safe across parallel test packages). Production code never
// reassigns this variable.
var runShellOnceFn = func(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), staticInfoCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func runShellOnce(name string, args ...string) (string, error) {
	return runShellOnceFn(name, args...)
}

// osVersion shells out to `sw_vers -productVersion` (e.g. "14.4.1"). Returns
// empty string on any failure.
func osVersion() string {
	out, err := runShellOnce("sw_vers", "-productVersion")
	if err != nil {
		return ""
	}
	return out
}

// kernelVersion shells out to `uname -r` (e.g. "23.4.0"). Returns empty
// string on any failure.
func kernelVersion() string {
	out, err := runShellOnce("uname", "-r")
	if err != nil {
		return ""
	}
	return out
}

// totalRAMBytes reads `sysctl -n hw.memsize`, returning bytes. Returns 0 on
// any failure.
func totalRAMBytes() uint64 {
	out, err := runShellOnce("sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(out, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
