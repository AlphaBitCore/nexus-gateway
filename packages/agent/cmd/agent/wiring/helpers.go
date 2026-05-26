package wiring

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

// ComposeAgentDownloadURL builds the full URL the UpdateBanner opens
// for "Install update" from the operator-configured Control Plane base
// URL (cfg.CpURL). The path suffix is platform-specific because each
// OS gets a different artefact (.pkg / .exe / Linux binary).
//
// Returns "" when cpURL is empty so the UpdateBanner hides its install
// button rather than open a broken link.
func ComposeAgentDownloadURL(cpURL string) string {
	base := strings.TrimRight(cpURL, "/")
	if base == "" {
		return ""
	}
	var suffix string
	switch runtime.GOOS {
	case "darwin":
		suffix = "/downloads/NexusAgent-latest.pkg"
	case "windows":
		suffix = "/downloads/nexus-agent-windows-latest.exe"
	default: // linux + others — single binary
		suffix = "/downloads/nexus-agent-linux-latest"
	}
	return base + suffix
}

// UserQuitFlagPath returns the filesystem handshake path between the GUI
// menu-bar app and the daemon. Resolved via platform.DefaultPaths so the
// path stays consistent with the GUI's QuitFlag helper.
func UserQuitFlagPath() string {
	return paths.DefaultPaths().UserQuitFlagPath
}

// GuiSocketPath returns the IPC socket path the daemon listens on for
// status queries from the menu-bar / tray UI. Single source of truth:
// paths.DefaultPaths().SocketPath.
func GuiSocketPath() string {
	if runtime.GOOS == "darwin" {
		_ = os.MkdirAll("/var/run", 0755)
	}
	return paths.DefaultPaths().SocketPath
}

// WarmBootstrap primes the agent-bootstrap cache so the status
// collector's DeviceAuthModeFn resolves the configured device-auth mode
// on the very first status snapshot. Failures are logged but never
// block startup.
func WarmBootstrap(ctx context.Context, c *bootstrap.Client, logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}) {
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := c.Get(warmCtx)
	if err != nil {
		logger.Warn("agent-bootstrap warm failed; onboarding UI will stay on 'Contacting the gateway' until a later warm succeeds",
			"error", err)
		return
	}
	logger.Info("agent-bootstrap warmed",
		"controlPlaneURL", info.ControlPlaneURL,
		"deviceAuthMode", info.DeviceAuthMode)
}

// ReadCertExpiry reads the NotAfter field from a PEM-encoded certificate.
// Returns zero time if the file does not exist or cannot be parsed.
func ReadCertExpiry(certFile string) time.Time {
	if certFile == "" {
		return time.Time{}
	}
	data, err := os.ReadFile(certFile)
	if err != nil {
		return time.Time{}
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}
	}
	return cert.NotAfter
}

// OSVersion returns a human-readable OS version string for enrollment
// and status reporting.
func OSVersion() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(context.Background(), "sw_vers", "-productVersion").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case "windows":
		out, err := exec.CommandContext(context.Background(), "cmd", "/c", "ver").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case "linux":
		data, err := os.ReadFile("/etc/os-release")
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					return strings.Trim(line[12:], "\"")
				}
			}
		}
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

// CertDir picks the directory enrollment artifacts (device cert, key, CA,
// identifiers) get written to. Falls back to paths.DefaultPaths().StateDir.
func CertDir(certFile string) string {
	if certFile != "" {
		idx := strings.LastIndex(certFile, string(os.PathSeparator))
		if idx >= 0 {
			return certFile[:idx]
		}
	}
	return paths.DefaultPaths().StateDir
}

// WritePIDFile writes the daemon's PID to the given path, creating parent
// dirs as needed. Used for the self-intercept guard so the NE filter can
// pass through own-process traffic.
func WritePIDFile(pidPath string, logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}) {
	if err := os.MkdirAll(fmt.Sprintf("%s/..", pidPath), 0755); err != nil {
		logger.Warn("self-intercept guard: mkdir failed; NE may loop on daemon outbound", "error", err)
		return
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		logger.Warn("self-intercept guard: write daemon.pid failed", "path", pidPath, "error", err)
		return
	}
	logger.Info("self-intercept guard: wrote daemon PID for NE filter", "path", pidPath, "pid", os.Getpid())
}
