// Package diagnostics collects the data the Dashboard's Diagnostics
// page renders: a tail of the agent's own log, Hub reachability, the
// path to the device certificate. All operations are best-effort —
// any individual source can fail and the page still renders the
// other fields.
package diagnostics

import (
	"bufio"
	"context"
	"net"
	"net/url"
	"os"
	"time"
)

// Snapshot is the JSON shape returned by GET_DIAGNOSTICS. It
// matches status.Diagnostics; redeclaring here avoids an import
// cycle between statusapi and the agent's runtime packages.
type Snapshot struct {
	HubReachable     bool     `json:"hubReachable"`
	CertPath         string   `json:"certPath"`
	LogTail          []string `json:"logTail"`
	InterceptionMode string   `json:"interceptionMode,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// InterceptionModeFn returns the live interception mode (e.g.
// "iptables", "NexusWFP", "NETransparentProxy",
// "SystemProxyFallback"). The Collector calls it once per snapshot
// so the Dashboard's Diagnostics page surfaces the active mechanism.
// Nil leaves the field empty — Dashboard renders "unknown".
type InterceptionModeFn func() string

// Collector composes the diagnostics surface.
type Collector struct {
	HubHTTPURL         string
	CertPath           string
	LogFile            string
	TailLines          int
	InterceptionModeFn InterceptionModeFn
}

// Collect returns a snapshot. Each probe runs with a sub-timeout
// derived from ctx so the whole call stays under the IPC budget
// (2s in statusapi).
func (c *Collector) Collect(ctx context.Context) Snapshot {
	snap := Snapshot{
		CertPath: c.CertPath,
		LogTail:  []string{},
	}

	// Reachability check: a TCP dial to the Hub host. We don't
	// HTTP because the agent's mTLS client requires a device cert
	// that pre-enrollment installs don't have yet — TCP success
	// is enough to tell the user "the network can reach the
	// gateway".
	if host := hostFromURL(c.HubHTTPURL); host != "" {
		dialCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		dialer := net.Dialer{}
		conn, err := dialer.DialContext(dialCtx, "tcp", host)
		if err == nil {
			snap.HubReachable = true
			_ = conn.Close()
		}
	}

	// Log tail: last N lines from the agent log file. We read up
	// to 64 KiB from the file's end to keep the read cheap.
	if c.LogFile != "" {
		lines, err := tail(c.LogFile, max(c.TailLines, 50))
		if err == nil {
			snap.LogTail = lines
		}
	}

	// Active interception mechanism — populates the Diagnostics
	// page's "Interception mode" row + drives the tray icon's
	// degraded-mode color on Windows.
	if c.InterceptionModeFn != nil {
		snap.InterceptionMode = c.InterceptionModeFn()
	}

	return snap
}

func hostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if host == "" {
		return ""
	}
	return host + ":" + port
}

func tail(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	const window = 64 * 1024
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() > window {
		if _, err := f.Seek(stat.Size()-window, 0); err != nil {
			return nil, err
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, window), window*2)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
