//go:build darwin

package platformshim

import (
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin"
)

// printPlatformBundleInventory prints the macOS-specific bundle stamps
// for `nexus-agent versions`. Same data the daemon logs at NE Start —
// surfaced via CLI so the operator can run it without tailing logs.
func PrintPlatformBundleInventory() {
	b := darwin.InspectBundles()
	fmt.Printf("host_app       short=%s build=%s mtime=%s path=%s%s\n",
		b.HostApp.CFBundleShortVersion,
		b.HostApp.CFBundleVersion,
		b.HostApp.Mtime,
		b.HostApp.Path,
		fmtNote(b.HostApp.Note),
	)
	fmt.Printf("extension_disk short=%s build=%s mtime=%s path=%s%s\n",
		b.ExtensionDisk.CFBundleShortVersion,
		b.ExtensionDisk.CFBundleVersion,
		b.ExtensionDisk.Mtime,
		b.ExtensionDisk.Path,
		fmtNote(b.ExtensionDisk.Note),
	)
	fmt.Printf("extension_live short=%s build=%s mtime=%s path=%s%s\n",
		b.ExtensionLive.CFBundleShortVersion,
		b.ExtensionLive.CFBundleVersion,
		b.ExtensionLive.Mtime,
		b.ExtensionLive.Path,
		fmtNote(b.ExtensionLive.Note),
	)
	if b.ExtensionDisk.CFBundleVersion != b.ExtensionLive.CFBundleVersion &&
		b.ExtensionDisk.CFBundleVersion != "<missing>" &&
		b.ExtensionLive.CFBundleVersion != "<missing>" {
		fmt.Printf("WARNING: VERSION MISMATCH between extension on disk (build=%s) and extension loaded by macOS (build=%s); macOS skipped system-extension replacement, provider process is running stale code\n",
			b.ExtensionDisk.CFBundleVersion, b.ExtensionLive.CFBundleVersion,
		)
	}
}

func fmtNote(n string) string {
	if n == "" {
		return ""
	}
	return " note=\"" + n + "\""
}
