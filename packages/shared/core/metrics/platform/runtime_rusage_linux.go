//go:build linux || freebsd || openbsd || netbsd || dragonfly

package platform

// maxRSSScale converts syscall.Rusage.Maxrss into bytes. On Linux and the BSDs
// the value is reported in kilobytes per getrusage(2).
const maxRSSScale uint64 = 1024
