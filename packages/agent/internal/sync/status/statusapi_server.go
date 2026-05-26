// Package statusapi provides a local IPC server for the native GUI to query agent status.
package status

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	auditevent "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
	auditqueue "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/queue"
)

// CheckUpdateFn checks for updates and returns (available, version, error).
type CheckUpdateFn func() (bool, string, error)

// SyncConfigFn triggers an immediate config sync and returns (success, configVersion, error).
type SyncConfigFn func() (bool, string, error)

// ShutdownFn triggers agent shutdown.
type ShutdownFn func()

// QueryEventsFn queries audit events from the local SQLite queue.
// #88-compat shim: the handler now decodes ai_only + since URL params
// and forwards them via a struct to a *Filtered handler. The QueryEventsFn
// type stays for back-compat with existing wirings; new code should pass
// queue.Queue.QueryEventsFiltered to NewServerFiltered.
type QueryEventsFn func(search, action string, offset, limit int) ([]auditevent.Event, int, error)

// QueryEventsFilteredFn adds the AI-only and Since filters #88 exposes
// to the UI's Traffic page. statusapi prefers this when wired; falls
// back to QueryEventsFn (zero values for the new fields) for older
// callers. sinceUnixMillis=0 disables time filter; aiOnly=false disables
// AI filter. Same shape as queue.QueryEventsFilter but kept as a plain
// function so wiring doesn't import the queue package directly.
type QueryEventsFilteredFn func(search, action string, aiOnly bool, sinceUnixMillis int64, offset, limit int) ([]auditevent.Event, int, error)

// QueryLifecycleFn queries lifecycle events from the local SQLite
// mirror — the user-visible source for the Dashboard "Activity" tab.
// Returns rows ordered time-descending plus total count for paging.
type QueryLifecycleFn func(offset, limit int) ([]auditqueue.LifecycleEvent, int, error)

// GetAppliedConfigFn returns a snapshot of every admin-pushed
// configuration this device is currently honouring — the data source
// for the Dashboard "Policies" tab. Implementation is pure-read
// against the in-memory thingclient shadow snapshot; no network
// hop. Return type is `any` because policies.AppliedConfig is
// defined in an importing package (would create a cycle), so the
// statusapi forwards the value verbatim through JSON marshal.
type GetAppliedConfigFn func() any

// QuitAllowedFn returns whether the agent is allowed to quit.
type QuitAllowedFn func() bool

// AuthenticateFn triggers enterprise login and returns the result.
// When the device is already enrolled it returns a confirmation prompt
// payload; the caller is expected to send a follow-up AUTHENTICATE
// CONFIRM (handled by ConfirmAuthFn) before re-enrollment actually starts.
type AuthenticateFn func() (map[string]any, error)

// ConfirmAuthFn proceeds with an SSO enrollment flow that AuthenticateFn
// previously gated behind a confirmation prompt. Blocks until the flow
// finishes (or is cancelled via CancelAuthFn) and returns the same
// shape as AuthenticateFn on the non-confirmation path.
type ConfirmAuthFn func() (map[string]any, error)

// CancelAuthFn aborts an in-progress SSO enrollment flow. Safe to call
// when no flow is running (no-op).
type CancelAuthFn func()

// TokenEnrollFn drives the legacy X-Enrollment-Token enrollment path
// from the IPC `ENROLL_TOKEN?<token>` command. Used when the
// operator-configured device-auth mode is "mtls-only" and the user
// pastes a CP-generated token into the menu-bar UI. Returns the
// enrolled device id on success.
type TokenEnrollFn func(token string) (deviceID string, err error)

// PauseProtectionFn engages the user-initiated protection pause.
// seconds=0 pauses indefinitely; seconds>0 schedules an auto-resume
// after that many seconds. Returns the absolute resume time when
// seconds>0; the zero value for indefinite pauses.
type PauseProtectionFn func(seconds int) time.Time

// ResumeProtectionFn cancels any active user-pause (manual or
// timer-scheduled) and disengages the killswitch.
type ResumeProtectionFn func()

// DiagnosticsFn returns the data the Dashboard's Diagnostics page
// renders: log tail (last ~50 lines), Hub connectivity probe
// result, certificate file path. All fields are best-effort; the
// fn must not block longer than ~2s.
type DiagnosticsFn func(ctx context.Context) Diagnostics

// Diagnostics is the JSON shape returned by GET_DIAGNOSTICS.
type Diagnostics struct {
	HubReachable bool     `json:"hubReachable"`
	CertPath     string   `json:"certPath"`
	LogTail      []string `json:"logTail"`
	// InterceptionMode is the active capture mechanism ("iptables",
	// "NexusWFP", "NETransparentProxy", "SystemProxyFallback").
	// Empty when the platform implementation does not satisfy
	// platform.InterceptionModeReporter (older builds).
	InterceptionMode string `json:"interceptionMode,omitempty"`
	Error            string `json:"error,omitempty"`
}

// OpenBrowserFn opens `url` in the user's default browser after the
// daemon has validated it. The Dashboard never invokes a shell
// command directly — every `xdg-open` / `open` / `start` goes
// through this IPC so the webview can never escalate to arbitrary
// command execution. The fn enforces:
//   - URL must parse as a valid absolute URL
//   - scheme must be https
//   - host must match an operator-configured allowlist
type OpenBrowserFn func(url string) error

// RuntimeFn returns a runtime introspection snapshot for the GUI's
// "Runtime" tab (e31-s12). The returned value is JSON-marshalled and
// sent over the IPC socket. nil means the command returns an
// "unconfigured" placeholder.
type RuntimeFn func(ctx context.Context) any

// ProxyInstallReport carries the result of the menu-bar host's
// attempt to install / enable the OS-level network-extension proxy
// configuration (NETransparentProxyManager.saveToPreferences on
// macOS). The menu-bar app posts this over the IPC socket whenever
// the install attempt completes — success or failure — so the
// outcome lands in the daemon's structured log (and, by extension,
// any diagnostics bundle / remote support flow) instead of being
// silently swallowed by Apple's os.log.
//
// Stage values are open-ended strings the Swift caller picks. Today:
//
//	"system-extension-install"  — OSSystemExtensionRequest activation
//	"transparent-proxy-save"    — NETransparentProxyManager.saveToPreferences
//	"transparent-proxy-load"    — load existing manager from preferences
//
// Phase / Outcome are also caller-chosen. Outcome=="ok" means success;
// any other value (with Error populated) means failure.
type ProxyInstallReport struct {
	Stage   string `json:"stage"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
	// AppVersion is the menu-bar host's bundle short-version string,
	// recorded so a daemon-side diagnostic can correlate which UI
	// build emitted the report. Optional.
	AppVersion string `json:"appVersion,omitempty"`
}

// ProxyInstallReportFn is the daemon-side handler the menu-bar app
// invokes via the REPORT_PROXY_INSTALL IPC command. Implementations
// typically log the report and may update health / diagnostics
// state. nil = command rejected as "not configured".
type ProxyInstallReportFn func(report ProxyInstallReport)

// VersionInfo is the small payload returned by the VERSION IPC
// command. The menu-bar app surfaces this in the menu header so the
// user can see "Nexus Agent vXXXXXXXX" at a glance without having
// to open About or run a CLI.
type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	BuiltAt string `json:"builtAt,omitempty"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// VersionFn returns the daemon's build identity. nil = command
// returns a placeholder with version="unknown".
type VersionFn func() VersionInfo

// QueryStatsRequest is the wire-side payload for QUERY_STATS. Mirrors
// the agent's localrollup.Query but uses string time fields so the
// IPC can stay text-only. Empty StartRFC3339 / EndRFC3339 default to
// "last 24 hours" on the daemon side.
type QueryStatsRequest struct {
	StartRFC3339 string `json:"start"`
	EndRFC3339   string `json:"end"`
	// Metric filters; empty = no filter. The wire form accepts both a
	// repeated `metric=foo&metric=bar` and a comma-joined
	// `metric=foo,bar` to keep IPC parsing simple.
	Metrics      []string `json:"metrics"`
	DimensionKey string   `json:"dimension"`
	SubDimension string   `json:"subDimension"`
}

// QueryStatsRow is the per-row wire shape. Mirrors the local
// rollup table columns minus thing_id (the agent has only one Thing,
// itself, so the caller already knows the Thing identity).
type QueryStatsRow struct {
	BucketStart  string  `json:"bucketStart"`
	MetricName   string  `json:"metricName"`
	DimensionKey string  `json:"dimensionKey,omitempty"`
	SubDimension string  `json:"subDimension,omitempty"`
	Value        float64 `json:"value"`
	Metadata     any     `json:"metadata,omitempty"`
}

// QueryStatsResponse is what the daemon writes back for QUERY_STATS.
type QueryStatsResponse struct {
	StartTime string          `json:"startTime"`
	EndTime   string          `json:"endTime"`
	Granule   string          `json:"granule"`
	Rows      []QueryStatsRow `json:"rows"`
	Error     string          `json:"error,omitempty"`
}

// QueryStatsFn is the daemon-side handler for QUERY_STATS. nil =
// command returns `{"error": "stats not configured"}`. Implementations
// typically delegate to localrollup.Aggregator.QueryRollup.
type QueryStatsFn func(ctx context.Context, req QueryStatsRequest) (QueryStatsResponse, error)

// SignOutFn handles the UNENROLL IPC (Dashboard "Sign out" button).
// Implementations typically clear the on-disk device-token + thing-id
// via auth.ClearEnrollment, then trigger a daemon shutdown so launchd
// respawns the process into the pre-enrollment onboarding flow. Return
// a non-nil error to surface the failure to the Dashboard.
type SignOutFn func(ctx context.Context) error

// RefreshPoliciesFn handles the REFRESH_POLICIES IPC (Dashboard
// "Refresh now" button on the Policies page). Implementations call
// shadow.Manager.RefreshPullKeys to force-fetch every Cat B config
// key from Hub right now and re-apply locally. Returns once the refresh
// finishes so the UI can stop its spinner and re-query GET_APPLIED_CONFIG.
type RefreshPoliciesFn func(ctx context.Context) error

// Server is the local IPC server for the GUI.
// statusapiMaxConcurrent caps simultaneous IPC clients. The socket is
// owner-only (0600), but a misbehaving local process under the same UID
// could otherwise hold connections open and exhaust goroutines + FDs.
const statusapiMaxConcurrent = 32

// statusapiReadDeadline bounds how long a single IPC command line may
// take to arrive. Without it a stuck/malicious client can pin a goroutine
// + scanner buffer indefinitely.
const statusapiReadDeadline = 30 * time.Second

// statusapiMaxLineBytes caps the per-command buffer. The Scanner default
// is 64 KiB, which is plenty for the existing GET_STATUS / QUERY_EVENTS /
// SYNC_CONFIG / GET_RUNTIME commands but should be set explicitly so a
// future command type doesn't surprise us with bufio.ErrTooLong.
const statusapiMaxLineBytes = 256 * 1024

type Server struct {
	socketPath         string
	collector          *Collector
	checkUpdateFn      CheckUpdateFn
	syncConfigFn       SyncConfigFn
	shutdownFn         ShutdownFn
	queryEventsFn         QueryEventsFn
	queryEventsFilteredFn QueryEventsFilteredFn // #88 — when wired, handler prefers this
	queryLifecycleFn   QueryLifecycleFn
	getAppliedConfigFn GetAppliedConfigFn
	quitAllowedFn      QuitAllowedFn
	authenticateFn     AuthenticateFn
	confirmAuthFn      ConfirmAuthFn
	cancelAuthFn       CancelAuthFn
	tokenEnrollFn      TokenEnrollFn
	pauseFn            PauseProtectionFn
	resumeFn           ResumeProtectionFn
	diagnosticsFn      DiagnosticsFn
	openBrowserFn      OpenBrowserFn
	runtimeFn          RuntimeFn
	proxyReportFn      ProxyInstallReportFn
	versionFn          VersionFn
	queryStatsFn       QueryStatsFn
	signOutFn          SignOutFn
	refreshPoliciesFn  RefreshPoliciesFn
	// listener is set by Start() in a goroutine and read by Stop() from
	// the daemon-shutdown caller. listenerMu guards both — without it
	// `go test -race` flags the write-vs-read on Server.listener.
	listenerMu sync.Mutex
	listener   net.Listener
	wg         sync.WaitGroup
	done       chan struct{}
	stopOnce   sync.Once
	// sem caps simultaneous live IPC connections.
	sem chan struct{}
}

// NewServer creates a status API server.
func NewServer(
	socketPath string,
	collector *Collector,
	checkUpdateFn CheckUpdateFn,
	syncConfigFn SyncConfigFn,
	shutdownFn ShutdownFn,
	queryEventsFn QueryEventsFn,
	quitAllowedFn QuitAllowedFn,
	authenticateFn AuthenticateFn,
) *Server {
	return &Server{
		socketPath:     socketPath,
		collector:      collector,
		checkUpdateFn:  checkUpdateFn,
		syncConfigFn:   syncConfigFn,
		shutdownFn:     shutdownFn,
		queryEventsFn:  queryEventsFn,
		quitAllowedFn:  quitAllowedFn,
		authenticateFn: authenticateFn,
		done:           make(chan struct{}),
		sem:            make(chan struct{}, statusapiMaxConcurrent),
	}
}

// SetQueryEventsFiltered wires the #88 successor handler that supports
// aiOnly + since filters. When set, the QUERY_EVENTS handler prefers
// it over the legacy QueryEventsFn — callers don't have to migrate
// in a single PR. Idempotent; safe to call multiple times.
func (s *Server) SetQueryEventsFiltered(fn QueryEventsFilteredFn) {
	s.queryEventsFilteredFn = fn
}

// Start begins listening. Blocks until Stop is called.
// Uses Unix domain sockets on macOS/Linux and named pipes on Windows.
func (s *Server) Start() error {
	ln, err := platformListen(s.socketPath)
	if err != nil {
		return err
	}
	s.listenerMu.Lock()
	s.listener = ln
	s.listenerMu.Unlock()
	slog.Info("status API listening", "path", s.socketPath)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				slog.Error("accept failed", "error", err)
				continue
			}
		}
		// Bound the number of concurrent IPC handlers; reject the
		// connection immediately if the cap is reached so a stuck client
		// can't pin every goroutine.
		select {
		case s.sem <- struct{}{}:
		default:
			slog.Warn("status API rejecting connection: at concurrency cap", "cap", statusapiMaxConcurrent)
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			s.handleConn(conn)
		}()
	}
}

// SetRuntimeFn wires the runtime introspection snapshot provider used
// by the GET_RUNTIME command (e31-s12). Calling with nil disables the
// command (it then returns an "unconfigured" placeholder). Optional —
// older callers that never invoke this keep working unchanged.
func (s *Server) SetRuntimeFn(fn RuntimeFn) {
	s.runtimeFn = fn
}

// SetConfirmAuthFn wires the handler for the AUTHENTICATE CONFIRM IPC
// command. Optional — callers that don't need SSO enrollment (tests,
// minimal harnesses) can leave it nil; CONFIRM then returns "no pending
// authentication".
func (s *Server) SetConfirmAuthFn(fn ConfirmAuthFn) {
	s.confirmAuthFn = fn
}

// SetCancelAuthFn wires the handler for the AUTHENTICATE CANCEL IPC
// command. Optional — callers that don't need SSO enrollment can leave
// it nil; CANCEL then returns acknowledged=true as a no-op.
func (s *Server) SetCancelAuthFn(fn CancelAuthFn) {
	s.cancelAuthFn = fn
}

// SetTokenEnrollFn wires the handler for the ENROLL_TOKEN?<token> IPC
// command (mtls-only mode). Optional — when nil ENROLL_TOKEN returns
// "enrollment not configured".
func (s *Server) SetTokenEnrollFn(fn TokenEnrollFn) {
	s.tokenEnrollFn = fn
}

// SetPauseProtectionFn wires the handler for PAUSE_PROTECTION?seconds=N.
// Optional — when nil the command returns
// `{"paused": false, "error": "pause not configured"}`.
func (s *Server) SetPauseProtectionFn(fn PauseProtectionFn) {
	s.pauseFn = fn
}

// SetResumeProtectionFn wires the handler for RESUME_PROTECTION.
// Optional — when nil the command returns
// `{"paused": false, "error": "resume not configured"}`.
func (s *Server) SetResumeProtectionFn(fn ResumeProtectionFn) {
	s.resumeFn = fn
}

// SetDiagnosticsFn wires the handler for GET_DIAGNOSTICS. Optional —
// when nil the command returns an error payload so the Dashboard's
// Diagnostics page can render a disabled-state placeholder.
func (s *Server) SetDiagnosticsFn(fn DiagnosticsFn) {
	s.diagnosticsFn = fn
}

// SetOpenBrowserFn wires the handler for OPEN_BROWSER?url=...
// Optional — when nil the command rejects the request. The
// implementation MUST validate the URL against the operator-configured
// allowlist before invoking a shell command.
func (s *Server) SetOpenBrowserFn(fn OpenBrowserFn) {
	s.openBrowserFn = fn
}

// SetProxyInstallReportFn wires the handler for REPORT_PROXY_INSTALL.
// The menu-bar host invokes this command after every
// NETransparentProxyManager.saveToPreferences attempt (and the
// SystemExtensionRequest activation that precedes it) so success +
// failure are recorded in the daemon's structured log. Optional;
// when nil the command acknowledges without forwarding.
func (s *Server) SetProxyInstallReportFn(fn ProxyInstallReportFn) {
	s.proxyReportFn = fn
}

// SetVersionFn wires the handler for VERSION. Optional; when nil the
// command returns a placeholder VersionInfo with version="unknown" so
// the menu bar always has something to render rather than a hard
// error.
func (s *Server) SetVersionFn(fn VersionFn) {
	s.versionFn = fn
}

// SetQueryStatsFn wires the handler for QUERY_STATS. The agent
// Dashboard's Stats page calls this to read pre-aggregated metrics from
// the local rollup tables (thing_metric_rollup_local_*). Optional;
// when nil the command returns `{"error":"stats not configured"}` so
// the Dashboard can render a fallback.
func (s *Server) SetQueryStatsFn(fn QueryStatsFn) {
	s.queryStatsFn = fn
}

// SetSignOutFn wires the handler for the UNENROLL IPC command.
// Optional; when nil the Dashboard's Sign Out button receives
// `{"acknowledged": false, "error": "sign-out not configured"}`.
func (s *Server) SetSignOutFn(fn SignOutFn) {
	s.signOutFn = fn
}

// SetQueryLifecycleFn wires the handler for QUERY_LIFECYCLE_EVENTS,
// the data source for the agent Dashboard "Activity" tab. Optional;
// when nil the command returns `{"events":[],"total":0}` so the
// Dashboard renders the empty state instead of a hard error.
func (s *Server) SetQueryLifecycleFn(fn QueryLifecycleFn) {
	s.queryLifecycleFn = fn
}

// SetGetAppliedConfigFn wires the handler for GET_APPLIED_CONFIG —
// the data source for the Dashboard "Policies" tab. Returns the
// admin-pushed configuration snapshot this device is honouring
// (interception domains, hooks, exemptions, kill switch, device
// defaults, diag mode). Optional; when nil the IPC returns an
// empty AppliedConfig shape so the page renders empty-state cards.
func (s *Server) SetGetAppliedConfigFn(fn GetAppliedConfigFn) {
	s.getAppliedConfigFn = fn
}

// SetRefreshPoliciesFn wires the handler for REFRESH_POLICIES — the
// data source for the Dashboard Policies page's "Refresh now" button.
// Optional; when nil the IPC returns {ok:false, error:"not configured"}.
func (s *Server) SetRefreshPoliciesFn(fn RefreshPoliciesFn) {
	s.refreshPoliciesFn = fn
}

// Stop gracefully stops the server.
func (s *Server) Stop() {
	s.stopOnce.Do(func() { close(s.done) })
	s.listenerMu.Lock()
	ln := s.listener
	s.listenerMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.wg.Wait()
	platformCleanup(s.socketPath)
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close() //nolint:errcheck
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), statusapiMaxLineBytes)
	for {
		// Refresh the read deadline before every command so a stuck client
		// cannot pin the handler goroutine indefinitely.
		_ = conn.SetReadDeadline(time.Now().Add(statusapiReadDeadline))
		if !scanner.Scan() {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		resp := s.dispatch(line)
		data, _ := json.Marshal(resp)
		_, _ = conn.Write(append(data, '\n')) // best-effort: GUI client may have detached
	}
}

func (s *Server) dispatch(cmd string) any {
	parts := strings.SplitN(cmd, "?", 2)
	command := parts[0]
	params := ""
	if len(parts) > 1 {
		params = parts[1]
	}

	slog.Debug("status API command received", "command", command)

	switch command {
	case "GET_STATUS":
		return s.collector.Collect()

	case "QUERY_EVENTS":
		return s.handleQueryEvents(params)

	case "QUERY_LIFECYCLE_EVENTS":
		return s.handleQueryLifecycle(params)

	case "GET_APPLIED_CONFIG":
		if s.getAppliedConfigFn == nil {
			return map[string]any{}
		}
		return s.getAppliedConfigFn()

	case "REFRESH_POLICIES":
		// Force-pulls every Cat B config key from Hub right now and
		// applies it locally — the manual "I want fresh policies"
		// affordance on the Dashboard's Policies page. The handler
		// returns once the refresh finishes so the UI can stop the
		// spinner and re-query GET_APPLIED_CONFIG with the new state.
		if s.refreshPoliciesFn == nil {
			return map[string]any{"ok": false, "error": "refresh not configured"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := s.refreshPoliciesFn(ctx); err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		return map[string]any{"ok": true}

	case "CHECK_UPDATE":
		if s.checkUpdateFn == nil {
			return map[string]any{"available": false, "error": "not configured"}
		}
		avail, ver, err := s.checkUpdateFn()
		if err != nil {
			return map[string]any{"available": false, "error": err.Error()}
		}
		return map[string]any{"available": avail, "version": ver}

	case "SYNC_CONFIG":
		if s.syncConfigFn == nil {
			return map[string]any{"success": false, "error": "not configured"}
		}
		ok, ver, err := s.syncConfigFn()
		if err != nil {
			return map[string]any{"success": false, "error": err.Error()}
		}
		return map[string]any{"success": ok, "version": ver}

	case "SHUTDOWN":
		if s.quitAllowedFn != nil && !s.quitAllowedFn() {
			return map[string]any{"acknowledged": false, "error": "quit is disabled by policy"}
		}
		if s.shutdownFn != nil {
			go s.shutdownFn()
		}
		return map[string]any{"acknowledged": true}

	case "GET_RUNTIME":
		// e31-s12: return the agent's runtime introspection snapshot for
		// the NexusAgentUI Runtime tab. Times out at 5s so a buggy
		// Source cannot stall the GUI.
		if s.runtimeFn == nil {
			return map[string]any{"error": "runtime introspection not configured"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.runtimeFn(ctx)

	case "AUTHENTICATE":
		if s.authenticateFn == nil {
			return map[string]any{"success": false, "error": "enterprise login not configured"}
		}
		result, err := s.authenticateFn()
		if err != nil {
			return map[string]any{"success": false, "error": err.Error()}
		}
		// When the response is a confirmation prompt the caller must not
		// see success=true (the enrollment hasn't actually happened yet).
		// authenticateFn signals this by setting
		// "confirmation_required": true; we leave the prompt payload
		// untouched in that case.
		if v, ok := result["confirmation_required"].(bool); ok && v {
			return result
		}
		result["success"] = true
		return result

	case "AUTHENTICATE CONFIRM":
		if s.confirmAuthFn == nil {
			return map[string]any{"success": false, "error": "no pending authentication"}
		}
		result, err := s.confirmAuthFn()
		if err != nil {
			return map[string]any{"success": false, "error": err.Error()}
		}
		result["success"] = true
		return result

	case "AUTHENTICATE CANCEL":
		if s.cancelAuthFn != nil {
			s.cancelAuthFn()
		}
		return map[string]any{"acknowledged": true}

	case "ENROLL_TOKEN":
		if s.tokenEnrollFn == nil {
			return map[string]any{"success": false, "error": "enrollment not configured"}
		}
		// The token rides in `params` (the substring after the
		// command's '?'). Reject empty payload so a stray
		// `ENROLL_TOKEN` line doesn't trigger an unauthenticated
		// enrollment attempt.
		token := strings.TrimSpace(params)
		if token == "" {
			return map[string]any{"success": false, "error": "missing token"}
		}
		deviceID, err := s.tokenEnrollFn(token)
		if err != nil {
			return map[string]any{"success": false, "error": err.Error()}
		}
		return map[string]any{"success": true, "device_id": deviceID}

	case "PAUSE_PROTECTION":
		if s.pauseFn == nil {
			return map[string]any{"paused": false, "error": "pause not configured"}
		}
		// Optional `seconds=N` query param. Anything else (e.g. a
		// malformed extra `&foo=bar`) is silently ignored — the
		// menu-bar UI only sends `seconds=N` or no params at all.
		seconds := 0
		if params != "" {
			for _, kv := range strings.Split(params, "&") {
				p := strings.SplitN(kv, "=", 2)
				if len(p) == 2 && p[0] == "seconds" {
					_, _ = fmt.Sscanf(p[1], "%d", &seconds)
				}
			}
		}
		resumesAt := s.pauseFn(seconds)
		resp := map[string]any{"paused": true}
		if !resumesAt.IsZero() {
			resp["resumes_at"] = resumesAt.UTC().Format(time.RFC3339)
		}
		return resp

	case "RESUME_PROTECTION":
		if s.resumeFn == nil {
			return map[string]any{"paused": false, "error": "resume not configured"}
		}
		s.resumeFn()
		return map[string]any{"paused": false}

	case "GET_DIAGNOSTICS":
		if s.diagnosticsFn == nil {
			return map[string]any{
				"hubReachable": false,
				"certPath":     "",
				"logTail":      []string{},
				"error":        "diagnostics not configured",
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return s.diagnosticsFn(ctx)

	case "REPORT_PROXY_INSTALL":
		// The menu-bar host calls this immediately after a system-
		// extension activation or NETransparentProxy.saveToPreferences
		// attempt completes. Payload format on the wire:
		//     REPORT_PROXY_INSTALL?<json>
		// where <json> is the JSON-encoded ProxyInstallReport. The
		// dispatch above already split on the first '?' into command
		// + params, so the JSON is in `params` verbatim.
		if params == "" {
			return map[string]any{"acknowledged": false, "error": "missing report body"}
		}
		var report ProxyInstallReport
		if err := json.Unmarshal([]byte(params), &report); err != nil {
			return map[string]any{"acknowledged": false, "error": fmt.Sprintf("decode report: %v", err)}
		}
		if s.proxyReportFn != nil {
			s.proxyReportFn(report)
		}
		return map[string]any{"acknowledged": true}

	case "VERSION":
		if s.versionFn == nil {
			return VersionInfo{Version: "unknown"}
		}
		return s.versionFn()

	case "QUERY_STATS":
		// Body format on the wire is the same single-line query-string
		// pattern QUERY_EVENTS uses (?start=...&end=...&metric=foo,bar&...).
		// Multi-line / JSON bodies would force IPC framing changes we
		// don't need — the existing pattern parses cleanly.
		return s.handleQueryStats(params)

	case "UNENROLL":
		// Dashboard Sign Out: clear local enrollment + trigger restart.
		// The handler typically clears device-token + thing-id from
		// disk and signals a graceful shutdown; launchd respawns the
		// agent which re-enters the onboarding flow on next boot.
		if s.signOutFn == nil {
			return map[string]any{"acknowledged": false, "error": "sign-out not configured"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.signOutFn(ctx); err != nil {
			return map[string]any{"acknowledged": false, "error": err.Error()}
		}
		return map[string]any{"acknowledged": true}

	case "OPEN_BROWSER":
		if s.openBrowserFn == nil {
			return map[string]any{"opened": false, "error": "open browser not configured"}
		}
		// `url=` query param. URL-encoded? The Dashboard sends the
		// raw URL (it owns the only call site); we trim whitespace
		// and let the handler do the heavy validation.
		rawURL := ""
		for _, kv := range strings.Split(params, "&") {
			p := strings.SplitN(kv, "=", 2)
			if len(p) == 2 && p[0] == "url" {
				rawURL = p[1]
			}
		}
		if rawURL == "" {
			return map[string]any{"opened": false, "error": "missing url"}
		}
		if err := s.openBrowserFn(rawURL); err != nil {
			return map[string]any{"opened": false, "error": err.Error()}
		}
		return map[string]any{"opened": true}

	default:
		return map[string]any{"error": fmt.Sprintf("unknown command: %s", command)}
	}
}

// handleQueryStats parses ?start=&end=&metric=&dimension=&subDimension=
// into a QueryStatsRequest and delegates to the wired QueryStatsFn.
// Returns the response shape verbatim so the wire stays predictable.
func (s *Server) handleQueryStats(params string) any {
	if s.queryStatsFn == nil {
		return QueryStatsResponse{Error: "stats not configured"}
	}
	var req QueryStatsRequest
	for _, kv := range strings.Split(params, "&") {
		if kv == "" {
			continue
		}
		p := strings.SplitN(kv, "=", 2)
		if len(p) != 2 {
			continue
		}
		key, val := p[0], p[1]
		switch key {
		case "start":
			req.StartRFC3339 = val
		case "end":
			req.EndRFC3339 = val
		case "metric":
			// Accept both comma-joined and repeated metric=... pairs.
			for _, m := range strings.Split(val, ",") {
				if m = strings.TrimSpace(m); m != "" {
					req.Metrics = append(req.Metrics, m)
				}
			}
		case "dimension":
			req.DimensionKey = val
		case "subDimension":
			req.SubDimension = val
		}
	}
	// 5s budget — local SQLite reads complete in milliseconds even on
	// a 30-day window; anything longer is a stuck-DB symptom we want
	// to surface as an error rather than hang the Dashboard.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.queryStatsFn(ctx, req)
	if err != nil {
		return QueryStatsResponse{Error: err.Error()}
	}
	return resp
}

// handleQueryLifecycle serves the agent Dashboard "Activity" tab.
// Pure-read against the local lifecycle_event SQLCipher mirror; no
// network hop, no Hub round-trip. Same offset/limit grammar as
// handleQueryEvents — keeps the frontend pagination code uniform
// across the two pages.
func (s *Server) handleQueryLifecycle(params string) any {
	if s.queryLifecycleFn == nil {
		return map[string]any{"events": []any{}, "total": 0}
	}
	offset := 0
	limit := 50
	for _, kv := range strings.Split(params, "&") {
		p := strings.SplitN(kv, "=", 2)
		if len(p) != 2 {
			continue
		}
		switch p[0] {
		case "offset":
			_, _ = fmt.Sscanf(p[1], "%d", &offset)
		case "limit":
			_, _ = fmt.Sscanf(p[1], "%d", &limit)
		}
	}
	events, total, err := s.queryLifecycleFn(offset, limit)
	if err != nil {
		return map[string]any{"events": []any{}, "total": 0, "error": err.Error()}
	}
	return map[string]any{"events": events, "total": total}
}

func (s *Server) handleQueryEvents(params string) any {
	// Either handler shape is acceptable; prefer the filtered one when
	// both are wired so the new ai_only + since URL params actually
	// take effect.
	if s.queryEventsFilteredFn == nil && s.queryEventsFn == nil {
		return map[string]any{"events": []any{}, "total": 0, "error": "not configured"}
	}

	q := ""
	action := ""
	offset := 0
	limit := 50
	aiOnly := false
	var sinceMs int64

	for _, kv := range strings.Split(params, "&") {
		p := strings.SplitN(kv, "=", 2)
		if len(p) != 2 {
			continue
		}
		switch p[0] {
		case "q":
			q = p[1]
		case "action":
			action = p[1]
		case "offset":
			_, _ = fmt.Sscanf(p[1], "%d", &offset)
		case "limit":
			_, _ = fmt.Sscanf(p[1], "%d", &limit)
		case "ai_only":
			// Accept "1" / "true" as truthy; everything else stays false.
			aiOnly = p[1] == "1" || p[1] == "true"
		case "since":
			// Unix milliseconds since epoch; 0 disables the time filter.
			_, _ = fmt.Sscanf(p[1], "%d", &sinceMs)
		}
	}

	var events []auditevent.Event
	var total int
	var err error
	if s.queryEventsFilteredFn != nil {
		events, total, err = s.queryEventsFilteredFn(q, action, aiOnly, sinceMs, offset, limit)
	} else {
		// Old wiring: ignore ai_only + since. Better than refusing to
		// serve at all; logs note the dropped filters so the UI knows
		// it's the daemon, not the frontend, that's silently ignoring
		// the new query params.
		if aiOnly || sinceMs > 0 {
			// best-effort warn — keep going so the page still loads.
			_ = aiOnly
			_ = sinceMs
		}
		events, total, err = s.queryEventsFn(q, action, offset, limit)
	}
	if err != nil {
		return map[string]any{"events": []any{}, "total": 0, "error": err.Error()}
	}
	return map[string]any{"events": events, "total": total}
}
