//go:build windows

package platform

// processResourceUsage on Windows is a no-op stub. getrusage(2) is a POSIX
// API; capturing equivalent CPU/RSS data on Windows requires GetProcessTimes
// and GetProcessMemoryInfo via psapi.dll, which is out of scope for L1.
func processResourceUsage() (cpuUser, cpuSys, rssBytes float64) {
	return 0, 0, 0
}
