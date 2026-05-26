//go:build windows

package platformshim

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/catrust"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
)

// cmdInstallCA is the one-shot install-time helper for Windows. It mirrors
// the macOS / Linux variants but uses certutil instead of `security` or
// `update-ca-certificates`.
//
// Operationally the MSI install does NOT need to call this — the daemon's
// platform.Windows.Start() runs `InstallCACert` on every boot, so a fresh
// install + automatic service-start (Start="install" in NexusAgent.wxs)
// covers the trust handoff with no separate CustomAction. This subcommand
// exists for two ops scenarios:
//
//  1. **Re-install of CA after manual deletion** — an operator who deleted
//     the device-ca.pem (or evicted the cert from the Root store via
//     `certutil -delstore Root`) can run:
//
//         nexus-agent.exe install-ca
//
//     from an elevated PowerShell to regenerate + re-trust without
//     restarting the service or reinstalling the MSI.
//
//  2. **Mirroring deb/rpm/.pkg parity** — every Unix package's
//     postinstall.sh runs `nexus-agent install-ca`. Having the same
//     subcommand on Windows means cross-platform docs ("run install-ca to
//     re-trust the agent CA") work everywhere without an OS-specific
//     branch.
//
// Idempotent — re-running after the CA is already on disk + in the Root
// store just reloads the file and re-imports the same cert (certutil
// `-f` no-ops on duplicates).
func CmdInstallCA(args []string) int {
	fs := flag.NewFlagSet("install-ca", flag.ExitOnError)
	defaultPrefix := filepath.Join(paths.DefaultPaths().StateDir, "device-ca")
	caOut := fs.String("device-ca-out", defaultPrefix,
		"path prefix for the device CA (writes <prefix>.pem and <prefix>.key)")
	skipUpdate := fs.Bool("skip-update", false,
		"skip importing the cert into the Windows Root store (dry runs / tests)")
	_ = fs.Parse(args)

	certPath := *caOut + ".pem"
	keyPath := *caOut + ".key"

	if err := os.MkdirAll(filepath.Dir(certPath), 0750); err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", filepath.Dir(certPath), err)
		return 1
	}

	cert, _, generated, err := agentTLS.LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load-or-generate CA: %v\n", err)
		return 1
	}
	if generated {
		slog.Info("device CA generated", "cert", certPath, "key", keyPath)
	} else {
		slog.Info("device CA already present; reusing",
			"cert", certPath, "key", keyPath, "subject", cert.Subject.CommonName)
	}

	if *skipUpdate {
		slog.Info("--skip-update set; not importing cert into Root store")
		return 0
	}

	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", certPath, err)
		return 1
	}
	if err := catrust.InstallCACert(pemBytes, "nexus-agent-device-ca"); err != nil {
		fmt.Fprintf(os.Stderr, "install CA into Root store: %v\n", err)
		return 1
	}
	slog.Info("device CA imported into Windows Root store",
		"label", "nexus-agent-device-ca", "subject", cert.Subject.CommonName)
	return 0
}
