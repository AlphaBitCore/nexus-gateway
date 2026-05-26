package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/lifecycle/bootstrap"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	config "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/schema"
)

// certDir picks the directory enrollment artifacts (device cert, key, CA,
// identifiers) get written to. Falls back to paths.DefaultPaths().StateDir.
func certDir(cfg *config.AgentConfig) string {
	if cfg.CertFile != "" {
		return filepath.Dir(cfg.CertFile)
	}
	return paths.DefaultPaths().StateDir
}

// enrollCertDir picks the directory enrollment artifacts get written to.
// It loads the agent config and returns certDir(cfg), so enroll writes
// into the same directory the daemon will later read from. When the config
// is missing it falls back to paths.DefaultPaths().StateDir so a fresh
// install on macOS / Linux / Windows still puts certs in the OS-correct
// place without requiring the operator to pre-stage a config file.
func enrollCertDir(configPath string) string {
	if cfg, err := config.LoadFromFile(configPath); err == nil {
		return certDir(cfg)
	} else if !os.IsNotExist(err) {
		slog.Warn("enroll: could not load config, falling back to platform default cert dir",
			"config", configPath, "error", err)
	}
	return paths.DefaultPaths().StateDir
}

// osVersion returns a human-readable OS version string for enrollment
// and status reporting.
func osVersion() string {
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

func cmdEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	hubURL := fs.String("hub-url", "", "Hub HTTP URL (default: read hubHTTPURL from config)")
	hubCA := fs.String("hub-ca", "", "PEM file with the bootstrap CA used to pin TLS for Hub enrollment")
	token := fs.String("token", "", "enrollment token (required)")
	configPath := fs.String("config", paths.DefaultPaths().ConfigFile, "path to agent config file (used to locate cert directory)")
	hostname, _ := os.Hostname()
	_ = fs.Parse(args)

	// Fall back to operator-baked URL in agent.yaml when the flag is empty.
	if *hubURL == "" {
		if cfg, err := config.LoadFromFile(*configPath); err == nil && cfg.HubHTTPURL != "" {
			*hubURL = cfg.HubHTTPURL
		}
	}
	if *token == "" || *hubURL == "" {
		fmt.Fprintln(os.Stderr, "Usage: nexus-agent enroll --hub-url <url> --token <token> [--config <path>] [--hub-ca <ca.pem>]")
		fmt.Fprintln(os.Stderr, "  (--hub-url is required unless hubHTTPURL is set in the config file)")
		os.Exit(1)
	}

	hubEnroller, err := enrollment.NewHubEnrollClient(*hubURL, *hubCA)
	if err != nil {
		slog.Error("failed to init hub enroll client", "error", err)
		os.Exit(1)
	}

	mgr := enrollment.NewManager(enrollCertDir(*configPath), enrollment.WithHubEnroller(hubEnroller))

	if err := mgr.Enroll(context.Background(), *token, hostname, runtime.GOOS, osVersion(), version); err != nil {
		slog.Error("enrollment failed", "error", err)
		os.Exit(1)
	}

	fmt.Printf("Enrolled successfully. Device ID: %s\n", mgr.ThingID())
}

// cmdEnrollSSO drives the SSO self-enrollment flow as a one-shot CLI
// operation. It is the deployment-friendly counterpart to cmdEnroll
// (which requires an admin-issued X-Enrollment-Token).
func cmdEnrollSSO(args []string) {
	fs := flag.NewFlagSet("enroll-sso", flag.ExitOnError)
	hubURL := fs.String("hub-url", "", "Hub HTTP URL (default: read hubHTTPURL from config)")
	hubCA := fs.String("hub-ca", "", "PEM file with the bootstrap CA")
	cpOverride := fs.String("cp-url", "", "optional Control Plane URL override (default: discovered from Hub)")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the browser callback")
	configPath := fs.String("config", paths.DefaultPaths().ConfigFile, "path to agent config file")
	_ = fs.Parse(args)

	if *hubURL == "" {
		if cfg, err := config.LoadFromFile(*configPath); err == nil && cfg.HubHTTPURL != "" {
			*hubURL = cfg.HubHTTPURL
		}
	}
	if *hubURL == "" {
		fmt.Fprintln(os.Stderr, "Usage: nexus-agent enroll-sso --hub-url <url> [--hub-ca <ca.pem>] [--cp-url <override>] [--config <path>] [--timeout 5m]")
		fmt.Fprintln(os.Stderr, "  (--hub-url is required unless hubHTTPURL is set in the config file)")
		os.Exit(1)
	}

	hubEnroller, err := enrollment.NewHubEnrollClient(*hubURL, *hubCA)
	if err != nil {
		slog.Error("failed to init hub enroll client", "error", err)
		os.Exit(1)
	}
	mgr := enrollment.NewManager(enrollCertDir(*configPath), enrollment.WithHubEnroller(hubEnroller))

	// Public bootstrap endpoint — system-root TLS, not pinned.
	bootstrapClient := bootstrap.New(*hubURL, bootstrap.DefaultHTTPClient(), *cpOverride)

	hostname, _ := os.Hostname()
	flow := &enrollment.Flow{
		HubEnroller:  hubEnroller,
		Manager:      mgr,
		Hostname:     hostname,
		OS:           runtime.GOOS,
		OSVersion:    osVersion(),
		AgentVersion: version,
		Timeout:      *timeout,
		ResolveCpURL: buildResolveCpURL(bootstrapClient),
	}
	result, err := flow.Run(context.Background())
	if err != nil {
		slog.Error("SSO enrollment failed", "error", err)
		os.Exit(1)
	}
	fmt.Printf("Enrolled via SSO. Device ID: %s, user: %s\n", result.ThingID, result.Email)
}

func cmdUnenroll(args []string) {
	fs := flag.NewFlagSet("unenroll", flag.ExitOnError)
	configPath := fs.String("config", "agent.yaml", "path to agent config file")
	_ = fs.Parse(args)

	cfg, err := config.LoadFromFile(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	hubEnroller, err := enrollment.NewHubEnrollClient(cfg.HubHTTPURL, cfg.EffectiveHubCA())
	if err != nil {
		slog.Error("failed to init hub enroll client", "error", err)
		os.Exit(1)
	}

	mgr := enrollment.NewManager(certDir(cfg), enrollment.WithHubEnroller(hubEnroller))

	if err := mgr.Unenroll(context.Background()); err != nil {
		slog.Error("unenrollment failed", "error", err)
		os.Exit(1)
	}
	fmt.Println("Unenrolled successfully.")
}
