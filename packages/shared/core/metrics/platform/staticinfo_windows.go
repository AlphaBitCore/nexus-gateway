//go:build windows

package platform

// Windows static-info lookups are best-effort no-ops. Real implementations
// would call RtlGetVersion / GlobalMemoryStatusEx via golang.org/x/sys/windows;
// out of scope for L2.

func osVersion() string     { return "" }
func kernelVersion() string { return "" }
func totalRAMBytes() uint64 { return 0 }
