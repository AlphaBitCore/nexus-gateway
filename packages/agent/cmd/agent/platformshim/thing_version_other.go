//go:build !darwin

package platformshim

// composeThingVersion returns the daemon version unchanged on non-macOS
// platforms. The macOS implementation appends bundle inventory.
func ComposeThingVersion(daemonVersion string) string {
	return daemonVersion
}
