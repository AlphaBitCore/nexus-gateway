// Package trayipc is the thin statusapi client used by the Win/Linux
// agent-tray binary. It mirrors the same wire protocol the menu-bar
// app on macOS already talks (statusapi over Unix socket / named
// pipe), but stays inside agent/internal/ so the tray binary doesn't
// pull in the full agent runtime.
//
// Why a third client (alongside the Swift StatusClient and the Wails
// AgentBridge): the tray is a Go process with no React frontend and
// no Cocoa runtime; it wants a small, typed surface (Snapshot,
// PauseResponse, ...) rather than the raw map[string]any the Wails
// bridge shuttles to JavaScript.
package trayipc

import (
	"bufio"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net"
	"runtime"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	"sync"
	"time"
)

// Client dials the daemon's statusapi socket / named pipe and
// dispatches typed JSON commands. Safe for concurrent use; each
// call opens its own connection, so callers can poll on a ticker
// without serialising clicks.
type Client struct {
	socketPath string

	mu sync.Mutex
}

// NewClient builds a Client configured for the running platform. The
// path is derived the same way the agent's guiSocketPath() picks it
// (XDG_RUNTIME_DIR → ~/.nexus/ on Linux, named pipe on Windows).
func NewClient() *Client {
	return &Client{socketPath: defaultSocketPath()}
}

// defaultSocketPath returns the IPC socket path the daemon listens on
// for status queries. Source of truth: paths.DefaultPaths().SocketPath
// (per platform.Paths.SocketPath docstring + agent-paths-abstraction
// binding rule). The daemon's guiSocketPath() resolves to the same value.
func defaultSocketPath() string {
	return paths.DefaultPaths().SocketPath
}

// Snapshot is the subset of the daemon's StatusSnapshot the tray UI
// actually reads. Fields the daemon may emit but the tray doesn't
// need (today stats, recent events, runtime introspection) are
// dropped here — they belong to the Dashboard.
type Snapshot struct {
	State       string    `json:"state"`
	StateReason string    `json:"stateReason"`
	Agent       AgentInfo `json:"agent"`
	Paused      bool      `json:"paused"`
	PausedUntil string    `json:"pausedUntil"`
}

// AgentInfo mirrors the fields the tray reads from
// StatusSnapshot.Agent.
type AgentInfo struct {
	DeviceID string `json:"deviceID"`
	SSOEmail string `json:"ssoEmail"`
}

// PauseResponse mirrors the daemon's PAUSE_PROTECTION /
// RESUME_PROTECTION response shape.
type PauseResponse struct {
	Paused    bool   `json:"paused"`
	ResumesAt string `json:"resumes_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ShutdownResponse mirrors the daemon's SHUTDOWN response.
type ShutdownResponse struct {
	Acknowledged bool   `json:"acknowledged"`
	Error        string `json:"error,omitempty"`
}

func (c *Client) GetStatus(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	if err := c.send(ctx, "GET_STATUS", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) PauseProtection(ctx context.Context, seconds int) (*PauseResponse, error) {
	cmd := "PAUSE_PROTECTION"
	if seconds > 0 {
		cmd = fmt.Sprintf("PAUSE_PROTECTION?seconds=%d", seconds)
	}
	var r PauseResponse
	if err := c.send(ctx, cmd, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ResumeProtection(ctx context.Context) (*PauseResponse, error) {
	var r PauseResponse
	if err := c.send(ctx, "RESUME_PROTECTION", &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) Shutdown(ctx context.Context) (*ShutdownResponse, error) {
	var r ShutdownResponse
	if err := c.send(ctx, "SHUTDOWN", &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// send dispatches a single-line command and decodes the JSON reply
// into out. The deadline is taken from ctx; if absent we fall back to
// 5 s so a stuck daemon doesn't hang the tray's poll loop forever.
func (c *Client) send(ctx context.Context, command string, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	dialTimeout := time.Until(deadline)
	if dialTimeout <= 0 {
		return fmt.Errorf("trayipc: context already expired")
	}
	if dialTimeout > 2*time.Second {
		dialTimeout = 2 * time.Second
	}

	conn, err := dialDeadline(c.socketPath, dialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	_ = conn.SetWriteDeadline(deadline)
	_ = conn.SetReadDeadline(deadline)

	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return fmt.Errorf("trayipc: write: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("trayipc: read: %w", err)
		}
		return fmt.Errorf("trayipc: empty response")
	}
	if err := json.Unmarshal(scanner.Bytes(), out); err != nil {
		// Surface the raw payload's first 200 bytes to make
		// diagnostics easier when the daemon ships an
		// unexpected shape.
		preview := scanner.Text()
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return fmt.Errorf("trayipc: decode: %w (got %q)", err, strings.TrimSpace(preview))
	}
	return nil
}

// dialDeadline performs a platform-aware dial:
//   - Unix sockets via the net package
//   - Windows named pipes via the platform-specific dialPipe (see
//     client_windows.go)
func dialDeadline(path string, timeout time.Duration) (net.Conn, error) {
	if runtime.GOOS == "windows" {
		return dialPipe(path, timeout)
	}
	d := net.Dialer{Timeout: timeout}
	return d.Dial("unix", path)
}
