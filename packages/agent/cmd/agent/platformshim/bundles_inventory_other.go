//go:build !darwin

package platformshim

// printPlatformBundleInventory is a no-op on non-macOS platforms. The
// macOS implementation (bundles_inventory_darwin.go) inspects bundles
// at /Applications/NexusAgent.app and /Library/SystemExtensions/<UUID>/.
func PrintPlatformBundleInventory() {
	// No platform-specific bundles to inspect on linux / windows; the
	// `versions` command still emits the daemon line above.
}
