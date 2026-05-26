//go:build darwin

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

// cmdInstallCA is the one-shot install-time helper invoked by the
// macOS .pkg postinstall.sh AS ROOT. It mirrors the Linux flow but
// uses macOS's `security add-trusted-cert` (via the existing
// catrust.InstallCACert wrapper) instead of update-ca-certificates,
// and writes the on-disk CA under
// `/Library/Application Support/com.nexus-gateway.agent/device-ca.{pem,key}`
// (the convention from paths.DefaultPaths().StateDir).
//
// Lifecycle:
//
//   - .pkg install → postinstall.sh → `nexus-agent install-ca`
//     → generate-or-load device CA → persist to disk → add to
//     System Keychain as a trusted root anchor.
//   - LaunchDaemon start → daemon loads CA from disk via
//     tls.LoadOrGenerateCA — does NOT re-generate, does NOT
//     re-call `security add-trusted-cert`. This fixes the
//     keychain-clutter bug where every daemon restart used to
//     install a fresh CA.
//   - .pkg uninstall → uninstall.sh removes the keychain entry +
//     the on-disk files.
//
// Idempotent: re-running this command after the CA already exists
// on disk just reloads it (LoadOrGenerateCA returns generated=false)
// and re-adds the same cert to the keychain — `security
// add-trusted-cert` is a no-op when the cert is already present.
func CmdInstallCA(args []string) int {
	fs := flag.NewFlagSet("install-ca", flag.ExitOnError)
	defaultPrefix := filepath.Join(paths.DefaultPaths().StateDir, "device-ca")
	caOut := fs.String("device-ca-out", defaultPrefix,
		"path prefix for the device CA (writes <prefix>.pem and <prefix>.key)")
	skipUpdate := fs.Bool("skip-update", false,
		"skip adding the cert to the System Keychain (for dry runs / tests)")
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
		slog.Info("--skip-update set; not adding cert to System Keychain")
		return 0
	}

	// Add to System Keychain as a trusted root anchor.
	// catrust.InstallCACert wraps `security add-trusted-cert -d -r
	// trustRoot -k /Library/Keychains/System.keychain` — requires
	// root, which we have because postinstall runs as root.
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", certPath, err)
		return 1
	}
	if err := catrust.InstallCACert(pemBytes, "nexus-agent-device-ca"); err != nil {
		fmt.Fprintf(os.Stderr, "install CA to keychain: %v\n", err)
		return 1
	}
	slog.Info("device CA installed into System Keychain",
		"label", "nexus-agent-device-ca", "subject", cert.Subject.CommonName)
	return 0
}
