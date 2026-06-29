package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

// AgentBridge is the Wails-bound Go object the React frontend talks
// to. Each public method becomes a JS function the frontend can
// `await`. The bridge owns a thin Unix-socket client against the
// agent daemon's existing statusapi — no HTTP, no network. Every
// request reuses the same socket-path discovery the menu-bar app
// uses (XDG_RUNTIME_DIR → ~/.nexus/).
//
// The Wails app is launched on demand by the menu bar and exits
// when the user closes the window, so the bridge is short-lived
// and stateless aside from a per-call timeout.
type AgentBridge struct {
	socketPath string
	timeout    time.Duration

	// mu serialises socket dials so a fast UI doesn't open dozens of
	// concurrent connections during a poll burst. The daemon caps
	// connections; back-pressuring on this end keeps us friendly.
	mu sync.Mutex
}

// NewAgentBridge constructs a bridge. Wails will Bind() the
// instance, exposing every exported method to JavaScript.
func NewAgentBridge() *AgentBridge {
	return &AgentBridge{
		socketPath: defaultSocketPath(),
		timeout:    10 * time.Second,
	}
}

// defaultSocketPath resolves the IPC endpoint the daemon listens on for
// status queries. Single source of truth: paths.DefaultPaths().SocketPath —
// the same value the daemon's guiSocketPath() and the tray's trayipc client
// resolve (Unix socket on macOS/Linux, named pipe on Windows). The daemon
// LISTENS where we CONNECT; deriving both from paths.DefaultPaths() keeps
// them in lockstep across platforms instead of duplicating the path logic.
func defaultSocketPath() string {
	return paths.DefaultPaths().SocketPath
}

func (b *AgentBridge) onShutdown(ctx context.Context) {
	// Best-effort: cancel any in-flight SSO enrollment so closing
	// the Dashboard window mid-OAuth doesn't leave the daemon
	// happily finishing enrollment in the background. The IPC is a
	// no-op when no flow is running, so we can fire it
	// unconditionally. 500 ms is enough — the cancel handler is a
	// channel close + atomic store on the daemon; if the daemon
	// can't be reached that fast we're better off letting the OS
	// finish tearing down the process. ctx is part of the Wails
	// OnShutdown signature but the cancel ping uses its own short
	// budget, so it is intentionally not threaded through here.
	_, _ = b.sendJSONWith("AUTHENTICATE CANCEL", 500*time.Millisecond)
}

// Public methods exposed to the React frontend

// GetStatus returns the current full status snapshot the daemon
// produces. The frontend uses it for the Overview page and the
// "agent not running" health check.
func (b *AgentBridge) GetStatus() (map[string]any, error) {
	return b.sendJSON("GET_STATUS")
}

// QueryEvents returns a page of audit events. `filter` mirrors the
// daemon's QUERY_EVENTS parameters: q, action, ai_only, since (Unix
// milliseconds), offset, limit. ai_only and since move the AI filter +
// time window from client-side over-fetch into the daemon's SQL WHERE,
// which keeps pagination + total correct for the Traffic page's
// "Show AI Only" + time-range selector.
func (b *AgentBridge) QueryEvents(filter EventFilter) (map[string]any, error) {
	params := []string{
		"offset=" + intToStr(filter.Offset),
		"limit=" + intToStr(maxInt(filter.Limit, 1)),
	}
	if filter.Search != "" {
		params = append(params, "q="+filter.Search)
	}
	if filter.Action != "" {
		params = append(params, "action="+filter.Action)
	}
	if filter.AIOnly {
		params = append(params, "ai_only=1")
	}
	if filter.SinceUnixMillis > 0 {
		params = append(params, "since="+int64ToStr(filter.SinceUnixMillis))
	}
	return b.sendJSON("QUERY_EVENTS?" + strings.Join(params, "&"))
}

// EventByID fetches one event's full detail (body + normalized + spill refs)
// on demand for the Traffic detail drawer, keeping the list query lightweight.
// A body that spilled to local disk is read back by the daemon and returned
// inline; a body already uploaded to S3 comes back ref-only (the agent has no
// S3 GET credential) so the drawer renders a "view in Control Plane" hint.
func (b *AgentBridge) EventByID(id string) (map[string]any, error) {
	return b.sendJSON("EVENT_BY_ID?id=" + id)
}

// EventFilter mirrors the daemon's QUERY_EVENTS params. The Search
// and Action fields are forwarded verbatim; empty strings are
// dropped from the query string so the daemon receives "no filter"
// rather than "filter on empty". AIOnly + SinceUnixMillis push the
// AI filter and time-window selector down to SQL.
type EventFilter struct {
	Search          string `json:"search"`
	Action          string `json:"action"`
	AIOnly          bool   `json:"aiOnly"`
	SinceUnixMillis int64  `json:"sinceUnixMillis"`
	Offset          int    `json:"offset"`
	Limit           int    `json:"limit"`
}

// QueryLifecycleEvents returns a page of agent lifecycle events from
// the local SQLCipher mirror — the data source for the Dashboard's
// Activity tab. Distinct from QueryEvents which returns per-
// connection audit_events rows. Pagination grammar mirrors
// QueryEvents to keep the frontend hook uniform across pages.
func (b *AgentBridge) QueryLifecycleEvents(filter LifecyclePage) (map[string]any, error) {
	params := []string{
		"offset=" + intToStr(filter.Offset),
		"limit=" + intToStr(maxInt(filter.Limit, 1)),
	}
	return b.sendJSON("QUERY_LIFECYCLE_EVENTS?" + strings.Join(params, "&"))
}

// LifecyclePage is the per-call paging shape for QueryLifecycleEvents.
// Mirrors EventFilter minus the search/action filter knobs — the
// lifecycle table is small enough that we render it whole; filtering
// can be added at the UI layer if volume ever requires it.
type LifecyclePage struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// GetAppliedConfig returns the admin-pushed configuration snapshot
// this device is currently honouring — the data source for the
// Dashboard "Policies" tab. Reads from the in-memory thingclient
// shadow snapshot; no DB or network round-trip. The result shape
// matches policies.AppliedConfig on the Go side.
func (b *AgentBridge) GetAppliedConfig() (map[string]any, error) {
	return b.sendJSON("GET_APPLIED_CONFIG")
}

// RefreshPolicies forces the daemon to re-pull every Cat B config key
// from Hub right now (hooks, interception_domains, policy_rules,
// payload_capture) and apply it locally. Used by the Dashboard
// Policies page "Refresh now" button when the user wants to see CP
// changes immediately instead of waiting for the next shadow tick.
// The IPC blocks until the refresh completes (typ. < 5 s for all four
// keys); the result is {ok: bool, error?: string}.
func (b *AgentBridge) RefreshPolicies() (map[string]any, error) {
	return b.sendJSON("REFRESH_POLICIES")
}

// PauseProtection engages the local kill switch. seconds=0 pauses
// indefinitely; seconds>0 schedules auto-resume after that many
// seconds.
func (b *AgentBridge) PauseProtection(seconds int) (map[string]any, error) {
	cmd := "PAUSE_PROTECTION"
	if seconds > 0 {
		cmd = fmt.Sprintf("PAUSE_PROTECTION?seconds=%d", seconds)
	}
	return b.sendJSON(cmd)
}

// ResumeProtection cancels any auto-resume timer and disengages the
// kill switch.
func (b *AgentBridge) ResumeProtection() (map[string]any, error) {
	return b.sendJSON("RESUME_PROTECTION")
}

// CheckUpdate asks the daemon to query Hub for available updates.
func (b *AgentBridge) CheckUpdate() (map[string]any, error) {
	return b.sendJSON("CHECK_UPDATE")
}

// GetDiagnostics returns the data the Troubleshoot / Diagnostics page
// renders: log tail, Hub connectivity, cert path. Wired through a
// dedicated statusapi command.
func (b *AgentBridge) GetDiagnostics() (map[string]any, error) {
	return b.sendJSON("GET_DIAGNOSTICS")
}

// EnrollWithToken drives the legacy X-Enrollment-Token enrollment
// path from the Dashboard's Onboarding page. On success the daemon
// exits and is respawned by launchd; the frontend's polling layer
// reconnects automatically.
func (b *AgentBridge) EnrollWithToken(token string) (map[string]any, error) {
	t := strings.TrimSpace(token)
	if t == "" {
		return map[string]any{"success": false, "error": "missing token"}, nil
	}
	return b.sendJSON("ENROLL_TOKEN?" + t)
}

// AuthenticateSSO kicks off the SSO enrollment flow. Returns
// confirmation_required when the device is already enrolled; the
// frontend then calls AuthenticateConfirm() / AuthenticateCancel().
//
// On a first-time install (device not yet enrolled) the daemon runs
// the full OAuth flow inline — open browser, wait for IdP sign-in,
// receive callback, exchange code, enroll with Hub — which can take
// many minutes of wall-clock time if the user gets distracted in the
// browser or hits an IdP MFA delay. Match the daemon-side
// ssoenroll.defaultTimeout so the IPC socket never gives up before
// the daemon does.
func (b *AgentBridge) AuthenticateSSO() (map[string]any, error) {
	return b.sendJSONWith("AUTHENTICATE", 30*time.Minute)
}

// AuthenticateConfirm proceeds with an SSO sign-in the user
// confirmed via the prompt. Blocks for up to ~30 min while the
// daemon completes the OAuth round-trip; the frontend should treat
// this Promise as long-running. Same window as AuthenticateSSO and
// the daemon-side ssoenroll.defaultTimeout.
func (b *AgentBridge) AuthenticateConfirm() (map[string]any, error) {
	return b.sendJSONWith("AUTHENTICATE CONFIRM", 30*time.Minute)
}

// AuthenticateCancel aborts an in-progress SSO enrollment.
func (b *AgentBridge) AuthenticateCancel() (map[string]any, error) {
	return b.sendJSON("AUTHENTICATE CANCEL")
}

// Unenroll executes the Dashboard's Sign Out flow. The daemon clears
// its on-disk device-token + thing-id and then exits — launchd
// respawns it which lands back in the onboarding flow. The Dashboard's
// existing reconnect / agent-not-running screens cover the few
// seconds of transition. Returns ack=true on success.
func (b *AgentBridge) Unenroll() (map[string]any, error) {
	return b.sendJSON("UNENROLL")
}

// RestartDaemon kills the daemon via SHUTDOWN IPC; launchd respawns
// it within ThrottleInterval (~10s). Used by the Diagnostics tab's
// "Restart Daemon" recovery button. Does NOT write a user-quit
// flag, so the daemon comes right back up — distinct from the
// menu's Quit which sets the flag to keep the daemon dead until
// the user re-opens the app. Returns acknowledged=false when the
// daemon refuses (quitAllowed=false admin policy).
func (b *AgentBridge) RestartDaemon() (map[string]any, error) {
	return b.sendJSON("SHUTDOWN")
}

// OpenBrowser asks the daemon to open `url` in the user's default
// browser. The daemon validates the URL against the operator-
// configured CP-URL allowlist + HTTPS-only policy, so the webview
// itself never invokes a shell command.
func (b *AgentBridge) OpenBrowser(url string) (map[string]any, error) {
	if strings.TrimSpace(url) == "" {
		return map[string]any{"opened": false, "error": "missing url"}, nil
	}
	return b.sendJSON("OPEN_BROWSER?url=" + url)
}

// StatsFilter mirrors the daemon's QUERY_STATS params. All fields are
// optional. Empty Start/End default to the last 24h on the daemon.
// Metrics is a list of exact metricName matches; the wire form sends
// it comma-joined. DimensionKey="" + SubDimension="" returns global
// (dimensionless) rows only — the same convention the rollup tables
// use to encode "fleet total".
type StatsFilter struct {
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Metrics      []string `json:"metrics"`
	DimensionKey string   `json:"dimension"`
	SubDimension string   `json:"subDimension"`
}

// QueryStats returns pre-aggregated rollup rows for this agent's
// local stats dashboard. Reads from the SQLite
// thing_metric_rollup_local_* tables maintained by the local rollup
// ticker; never touches the network. The Dashboard's Stats page is
// the only caller.
func (b *AgentBridge) QueryStats(filter StatsFilter) (map[string]any, error) {
	params := []string{}
	if s := strings.TrimSpace(filter.Start); s != "" {
		params = append(params, "start="+s)
	}
	if s := strings.TrimSpace(filter.End); s != "" {
		params = append(params, "end="+s)
	}
	if len(filter.Metrics) > 0 {
		// Comma-joined keeps the IPC body small and matches what the
		// daemon's handleQueryStats parses (it also accepts repeated
		// metric=... pairs, but comma-joined is the canonical form
		// the daemon emits if it ever echoes a request back).
		params = append(params, "metric="+strings.Join(filter.Metrics, ","))
	}
	if s := strings.TrimSpace(filter.DimensionKey); s != "" {
		params = append(params, "dimension="+s)
	}
	if s := strings.TrimSpace(filter.SubDimension); s != "" {
		params = append(params, "subDimension="+s)
	}
	cmd := "QUERY_STATS"
	if len(params) > 0 {
		cmd += "?" + strings.Join(params, "&")
	}
	return b.sendJSON(cmd)
}

// Socket plumbing

// sendJSON dispatches a single-line IPC command and decodes the
// daemon's JSON response into a generic map. The default timeout
// covers all interactive commands; AuthenticateConfirm overrides via
// sendJSONWith.
func (b *AgentBridge) sendJSON(command string) (map[string]any, error) {
	return b.sendJSONWith(command, b.timeout)
}

func (b *AgentBridge) sendJSONWith(command string, timeout time.Duration) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Dial timeout is bounded by the request budget. Callers with a
	// 500 ms budget (e.g. onShutdown's cancel ping) don't want a 2s
	// dial hang on a dead daemon. The 2s cap protects long-budget
	// callers (AuthenticateConfirm's 6-min request) from blocking
	// indefinitely if the daemon is at its connection cap.
	dialTimeout := timeout
	if dialTimeout > 2*time.Second {
		dialTimeout = 2 * time.Second
	}
	// Platform-aware dial: Unix socket on macOS/Linux, Windows named pipe
	// on Windows (see dial_windows.go / dial_other.go). The previous
	// hardcoded net.Dial("unix", …) could never reach the daemon's named
	// pipe on Windows, which surfaced as a false "agent not running".
	conn, err := dialStatus(b.socketPath, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial agent socket: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	_ = conn.SetWriteDeadline(deadline)
	_ = conn.SetReadDeadline(deadline)

	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return nil, fmt.Errorf("write IPC command: %w", err)
	}

	// Daemon writes one JSON object terminated by '\n'.
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read IPC response: %w", err)
		}
		return nil, fmt.Errorf("read IPC response: empty")
	}

	var out map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode IPC response: %w", err)
	}
	return out, nil
}

func intToStr(i int) string {
	return fmt.Sprintf("%d", i)
}

func int64ToStr(i int64) string {
	return fmt.Sprintf("%d", i)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
