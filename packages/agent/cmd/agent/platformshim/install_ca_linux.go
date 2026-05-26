//go:build linux

package platformshim

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
)

// cmdInstallCA is the one-shot install-time helper invoked by the
// Linux deb/rpm postinstall.sh as root. It:
//
//  1. Loads OR generates the device CA at <device-ca-out>.{pem,key}
//     (0644 for the cert, 0600 for the key).
//  2. Copies the cert PEM to <trust-store-path>/nexus-agent.crt.
//  3. Runs `update-ca-certificates` so the OS trust bundle picks it up,
//     making intercepted TLS connections trusted by every userspace
//     HTTPS client on the host.
//
// The runtime daemon never invokes this command — it only ever
// reads the cert+key produced here via LoadOrGenerateCA. That
// preserves the privilege separation: install time runs as root and
// touches /usr/local/share/ca-certificates/; the daemon runs as the
// unprivileged nexus-agent user and never needs write access there.
func CmdInstallCA(args []string) int {
	fs := flag.NewFlagSet("install-ca", flag.ExitOnError)
	caOut := fs.String("device-ca-out", "/var/lib/nexus-agent/device-ca",
		"path prefix for the device CA (writes <prefix>.pem and <prefix>.key)")
	trustStore := fs.String("trust-store-path", "/usr/local/share/ca-certificates",
		"directory the OS trust-update tool scans for additional CAs")
	skipUpdate := fs.Bool("skip-update", false,
		"skip running update-ca-certificates (for dry runs / tests)")
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
		slog.Info("device CA already present; reusing", "cert", certPath, "key", keyPath, "subject", cert.Subject.CommonName)
	}

	// Copy the cert PEM into the OS trust store directory so
	// update-ca-certificates picks it up.
	if err := os.MkdirAll(*trustStore, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", *trustStore, err)
		return 1
	}
	dest := filepath.Join(*trustStore, "nexus-agent.crt")
	if err := copyFile(certPath, dest, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "copy %s -> %s: %v\n", certPath, dest, err)
		return 1
	}
	slog.Info("device CA copied to OS trust store dir", "dest", dest)

	if *skipUpdate {
		slog.Info("--skip-update set; not running update-ca-certificates")
		return 0
	}

	// Run update-ca-certificates so the bundle picks up the new
	// .crt. On debian/ubuntu it lives at /usr/sbin/update-ca-certificates;
	// fedora/rhel uses /usr/bin/update-ca-trust. Try the former first.
	if path, err := exec.LookPath("update-ca-certificates"); err == nil {
		cmd := exec.Command(path)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "update-ca-certificates: %v\n", err)
			return 1
		}
		slog.Info("OS trust store updated", "tool", "update-ca-certificates")
		return 0
	}
	if path, err := exec.LookPath("update-ca-trust"); err == nil {
		// Fedora / RHEL — needs the cert in /etc/pki/ca-trust/source/anchors/
		// instead, but our trust-store-path default targets the debian
		// layout. Surface the mismatch as a clear error rather than
		// silently misbehaving.
		fmt.Fprintf(os.Stderr,
			"update-ca-trust detected (RHEL/Fedora). Pass "+
				"--trust-store-path=/etc/pki/ca-trust/source/anchors and re-run.\n")
		_ = path
		return 1
	}
	fmt.Fprintf(os.Stderr,
		"neither update-ca-certificates nor update-ca-trust on PATH; "+
			"intercepted TLS will not be trusted by host clients.\n")
	return 1
}

// copyFile copies src to dst with the given mode. Used to drop the
// CA into the trust-store directory.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
