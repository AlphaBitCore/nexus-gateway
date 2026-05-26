// Package openbrowser handles the OPEN_BROWSER IPC command from the
// Dashboard. Every shell-out goes through this package, never
// from the WebView directly, so a compromised renderer cannot
// invoke arbitrary commands. The policy:
//
//  1. URL must parse as an absolute URL.
//  2. Scheme must be `https`.
//  3. Host must equal one of an operator-configured allowlist.
//     This is normally just the Control Plane base URL the daemon
//     learns from Hub bootstrap; the Dashboard uses it to open the
//     CP admin views for "Manage in admin console" links.
package openbrowser

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opener validates and dispatches browser open requests.
type Opener struct {
	mu          sync.RWMutex
	allowedHost map[string]struct{}
}

// New constructs an Opener with an empty allowlist. Callers populate
// it via SetAllowedHosts once the operator's configured CP URL is
// known (lazy because Hub bootstrap may resolve after startup).
func New() *Opener {
	return &Opener{allowedHost: map[string]struct{}{}}
}

// SetAllowedHosts replaces the allowlist with the given set. Hosts
// should be bare hostnames (no scheme, no port).
func (o *Opener) SetAllowedHosts(hosts ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.allowedHost = make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			o.allowedHost[h] = struct{}{}
		}
	}
}

// Open validates the URL and dispatches it to the OS-default
// browser. Returns an error when the URL is malformed, non-HTTPS,
// or addressed to a host not on the allowlist. Network errors
// (e.g. xdg-open not installed) also surface.
func (o *Opener) Open(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("only https URLs are permitted (got %q)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("missing host")
	}

	o.mu.RLock()
	_, allowed := o.allowedHost[host]
	o.mu.RUnlock()
	if !allowed {
		return fmt.Errorf("host %q not in allowlist", host)
	}

	// Platform-native open. The 5s timeout prevents a stuck
	// xdg-open child from blocking the IPC handler forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return dispatch(ctx, u.String())
}

func dispatch(ctx context.Context, url string) error {
	// Defense-in-depth: never spawn a real browser when running under
	// `go test`. testing.Testing() is true only in binaries built by
	// `go test`; production binaries take the real shell-out path.
	if testing.Testing() {
		return fmt.Errorf("openbrowser: dispatch called from a test binary; tests must stub dispatch via a seam rather than calling Open")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return cmd.Start()
}
