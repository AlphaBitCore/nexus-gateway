package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/cmd/agent/platformshim"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

// Build-time identity. version is overridden via -ldflags
// "-X main.version=…"; commit and builtAt are likewise stamped by
// scripts/build.sh so an operator can correlate a running daemon with
// the exact source revision that produced it. The defaults here keep
// `go run` working without ldflags.
var (
	version = "0.0.0-dev"
	commit  = "unknown"
	builtAt = "unknown"
)

// userQuitFlagPath returns the filesystem handshake between the GUI
// menu-bar app and this daemon. When the user picks Quit in the menu
// the GUI writes this file; the daemon both
//  1. exits immediately on every launchd respawn while the file exists (cold-boot self-exit), and
//  2. watches for the file at runtime (flagWatcher goroutine) and self-exits within a couple of seconds if it appears.
//
// (2) is the load-bearing path for "user clicked Quit on the already-running daemon"
// — IPC SHUTDOWN is a faster optimization but is not required; the watcher catches it either way.
//
// Re-launching the GUI app removes the file so the next launchd respawn brings the daemon back.
// See [[agent-quit-flag-design]].
func userQuitFlagPath() string {
	return paths.DefaultPaths().UserQuitFlagPath
}

// guiSocketPath returns the IPC socket path the daemon listens on for
// status queries from the menu-bar / tray UI. Single source of truth:
// paths.DefaultPaths().SocketPath. The Linux variant has the
// XDG_RUNTIME_DIR → ~/.nexus → /tmp fallback chain encapsulated in
// the platform package (paths_linux.go::linuxStatusSocketPath).
func guiSocketPath() string {
	if runtime.GOOS == "darwin" {
		_ = os.MkdirAll("/var/run", 0755)
	}
	return paths.DefaultPaths().SocketPath
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: nexus-agent <command> [flags]")
		fmt.Println("Commands: run, enroll, enroll-sso, unenroll, install-ca, version")
		os.Exit(1)
	}

	// `version` / `--version` / `-v` are surfaced ahead of the
	// platform-specific dispatch so the operator can run them on a
	// host where the LaunchDaemon is unhealthy. Output goes to stdout
	// in a stable single-line + key=value shape suitable for both
	// humans and `grep`/`awk` in deploy scripts.
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("nexus-agent version=%s commit=%s built=%s os=%s arch=%s\n",
			version, commit, builtAt, runtime.GOOS, runtime.GOARCH)
		return
	case "versions":
		// `versions` (plural) prints the FULL macOS bundle inventory:
		// daemon Go binary + host .app + extension on disk + extension
		// macOS actually loaded. macOS-only; Linux/Windows print the daemon line.
		fmt.Printf("daemon         version=%s commit=%s built=%s os=%s arch=%s\n",
			version, commit, builtAt, runtime.GOOS, runtime.GOARCH)
		platformshim.PrintPlatformBundleInventory()
		return
	}

	// Platform-specific commands (e.g. install/uninstall/run-svc on Windows).
	// On non-Windows builds this is a no-op.
	runFn := func(ctx context.Context) error {
		cmdRun(os.Args[2:])
		return nil
	}
	if handled := platformshim.DispatchPlatformCommand(os.Args[1], os.Args[2:], runFn); handled {
		return
	}

	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "enroll":
		cmdEnroll(os.Args[2:])
	case "enroll-sso":
		cmdEnrollSSO(os.Args[2:])
	case "unenroll":
		cmdUnenroll(os.Args[2:])
	case "install-ca":
		// Linux-only install-time helper invoked by the deb/rpm
		// postinstall.sh as root. No-op on macOS / Windows builds.
		os.Exit(platformshim.CmdInstallCA(os.Args[2:]))
	case "install-wfp-check":
		// Windows-only MSI custom-action helper invoked after the
		// NexusWFP kernel service is started. No-op on macOS / Linux builds.
		os.Exit(platformshim.CmdInstallWfpCheck(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
