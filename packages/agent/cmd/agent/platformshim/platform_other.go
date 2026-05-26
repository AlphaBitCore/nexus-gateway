//go:build !windows

package platformshim

import "context"

// DispatchPlatformCommand is a no-op on non-Windows platforms. Windows builds
// override this in platform_windows.go to handle install/uninstall/run-svc.
// runFn is accepted for API symmetry with the Windows variant but is never called.
func DispatchPlatformCommand(_ string, _ []string, _ func(context.Context) error) bool {
	return false
}
