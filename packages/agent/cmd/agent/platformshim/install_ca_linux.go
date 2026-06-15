//go:build linux

package platformshim

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/catrust"
)

// CmdInstallCA is the one-shot install-time helper invoked by the Linux
// deb/rpm postinstall.sh AS ROOT. It:
//
//  1. Loads OR generates the device CA at <device-ca-out>.{pem,key}
//     (0644 for the cert, 0600 for the key).
//  2. Installs the cert into the OS trust store via catrust.InstallCACert,
//     which auto-detects the distro layout — Debian/Ubuntu
//     (`/usr/local/share/ca-certificates` + `update-ca-certificates`),
//     RHEL/Fedora/Amazon Linux (`/etc/pki/ca-trust/source/anchors` +
//     `update-ca-trust extract`), Arch, and Alpine. This is why the
//     command no longer takes a `--trust-store-path`: the previous
//     Debian-only default left RPM installs with intercepted TLS
//     untrusted unless the operator knew to override it.
//
// The runtime daemon never invokes this command — it only ever reads the
// cert+key produced here via LoadOrGenerateCA. That preserves the
// privilege separation: install time runs as root and mutates the OS
// trust store; the daemon runs as the unprivileged nexus-agent user and
// never needs write access there.
//
// Idempotent: re-running after the CA already exists on disk reloads it
// (LoadOrGenerateCA returns generated=false) and re-writes the same cert
// into the trust store — update-ca-certificates / update-ca-trust are
// no-ops when the bundle already contains it. An upgrade therefore never
// regenerates the CA.
func CmdInstallCA(args []string) int {
	fs := flag.NewFlagSet("install-ca", flag.ExitOnError)
	caOut := fs.String("device-ca-out", "/var/lib/nexus-agent/device-ca",
		"path prefix for the device CA (writes <prefix>.pem and <prefix>.key)")
	skipUpdate := fs.Bool("skip-update", false,
		"generate/load the CA on disk but do NOT install it into the OS trust store (dry runs / tests / passthrough-only containers)")
	_ = fs.Parse(args)

	certPath := *caOut + ".pem"
	keyPath := *caOut + ".key"

	// Ensure parent dir exists.
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
		slog.Info("--skip-update set; not installing cert into the OS trust store")
		return 0
	}

	// Install into the OS trust store. catrust.InstallCACert picks the
	// correct anchor directory + refresh command for whatever distro this
	// is, so a single code path covers Debian/RHEL/Arch/Alpine.
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", certPath, err)
		return 1
	}
	if err := catrust.InstallCACert(pemBytes, "nexus-agent"); err != nil {
		fmt.Fprintf(os.Stderr, "install CA into OS trust store: %v\n", err)
		return 1
	}
	slog.Info("device CA installed into OS trust store",
		"label", "nexus-agent", "subject", cert.Subject.CommonName)
	return 0
}
