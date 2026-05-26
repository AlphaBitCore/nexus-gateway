//go:build darwin

package platformshim

import (
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin"
)

// composeThingVersion appends the macOS bundle inventory to the daemon
// Go version so the resulting ThingVersion field reaches Hub with the
// full picture in one go. Format:
//
//	<daemon-version> host=<NNN> ext_disk=<NNN> ext_live=<NNN>
//
// Hub stores ThingVersion as-is; admin tools can parse the suffix to
// surface bundle stamps and detect mismatches without an extra HTTP
// round-trip. On non-macOS platforms the suffix is omitted (see
// thing_version_other.go).
func ComposeThingVersion(daemonVersion string) string {
	b := darwin.InspectBundles()
	host := bundleStampOrDash(b.HostApp.CFBundleVersion)
	extDisk := bundleStampOrDash(b.ExtensionDisk.CFBundleVersion)
	extLive := bundleStampOrDash(b.ExtensionLive.CFBundleVersion)
	return fmt.Sprintf("%s host=%s ext_disk=%s ext_live=%s",
		daemonVersion, host, extDisk, extLive,
	)
}

func bundleStampOrDash(s string) string {
	if s == "" || s == "<missing>" {
		return "-"
	}
	return s
}
