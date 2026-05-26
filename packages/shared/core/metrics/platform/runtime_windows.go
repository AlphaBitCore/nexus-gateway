//go:build windows

package platform

// osReadDir is the Windows shim. There is no /proc on Windows; openFDCount
// will see the nil result and return 0. A real Win32-handle count would
// require psapi.dll bindings, which is out of scope for L1.
func osReadDir(path string) ([]string, error) { return nil, nil }
