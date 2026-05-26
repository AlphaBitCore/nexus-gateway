//go:build !windows

package platform

import "syscall"

// getRusageFn is the syscall used to collect process resource usage.
// It is a package-level variable so tests can inject a failing stub to
// exercise the error-return branch without depending on OS behaviour.
// Production code never reassigns this variable.
var getRusageFn = syscall.Getrusage

// processResourceUsage returns (user CPU seconds, system CPU seconds, RSS bytes)
// from getrusage(RUSAGE_SELF). Best-effort: on error all three are zero.
//
// ru_maxrss units are platform-specific (kilobytes on Linux/BSD, bytes on
// macOS); maxRSSScale is supplied by a build-tagged constant per OS.
func processResourceUsage() (cpuUser, cpuSys, rssBytes float64) {
	var ru syscall.Rusage
	if err := getRusageFn(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0, 0
	}
	cpuUser = float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6
	cpuSys = float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
	rssBytes = float64(uint64(ru.Maxrss) * maxRSSScale)
	return cpuUser, cpuSys, rssBytes
}
