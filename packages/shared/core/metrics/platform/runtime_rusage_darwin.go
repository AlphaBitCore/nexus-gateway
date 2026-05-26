//go:build darwin

package platform

// maxRSSScale converts syscall.Rusage.Maxrss into bytes. On macOS the value
// is reported in bytes per getrusage(2), so the scale is 1.
const maxRSSScale uint64 = 1
