// Package status aggregates agent state into a snapshot for the GUI.
package status

import (
	"github.com/goccy/go-json"
	"sync"
	"time"
)

// InterceptionHealth mirrors platform.InterceptionHealth without
// importing the platform package — keeps the status layer free of an
// OS-shim dependency. Wired at agent/main.go construction time via
// CollectorConfig.InterceptionHealthFn.
type InterceptionHealth struct {
	StartedAt        time.Time
	Connected        bool
	ConnectionsTotal int64
	ActiveSessions   int
	LastFlowAt       time.Time
	// SelfReported + DegradedReason mirror platform.InterceptionHealth.
	// When SelfReported is true the collector trusts DegradedReason
	// (empty = healthy) instead of the connection-count heuristic —
	// see checkInterceptionHealth and the platform-side field docs.
	SelfReported   bool
	DegradedReason string
}

// InterceptionGracePeriod is the post-startup window during which a
// missing capture-layer attach is NOT treated as degraded — the OS
// extension needs a few seconds to load. Kept in sync with
// platform.InterceptionGracePeriod (mirrored, not imported, so the two
// packages stay layer-clean).
const InterceptionGracePeriod = 30 * time.Second

// RecentEvent is a compact event for the GUI's recent activity list.
type RecentEvent struct {
	Time        string `json:"time"`
	ProcessName string `json:"processName"`
	DestHost    string `json:"destHost"`
	Action      string `json:"action"`
}

// TodayStats holds daily interception counters.
type TodayStats struct {
	Inspected   int `json:"inspected"`
	Passthrough int `json:"passthrough"`
	Denied      int `json:"denied"`
	// Latency phase averages over today's audit_events window. Computed
	// by the status collector via a small SQL roll-up; nil/zero when no
	// traffic has phase data yet (e.g. fresh install before first
	// upstream call). The UI Overview tile reads these for the
	// "Today's Latency" chip.
	AvgUsOverheadMs    *int `json:"avgUsOverheadMs,omitempty"`
	AvgUpstreamTotalMs *int `json:"avgUpstreamTotalMs,omitempty"`
}

// AuditQueueInfo holds queue status for display.
type AuditQueueInfo struct {
	UnsyncedCount int    `json:"unsyncedCount"`
	LastSyncTime  string `json:"lastSyncTime"`
}

// AgentInfo holds version and health metadata.
type AgentInfo struct {
	Version         string `json:"version"`
	UpdateAvailable bool   `json:"updateAvailable"`
	// DownloadURL is the full URL the UpdateBanner opens when the
	// user clicks "Install update". Composed daemon-side as
	// `<cpURL>/downloads/NexusAgent-latest.pkg` from the operator-
	// configured CpURL in agent.yaml, so each deployment serves its
	// own artefact host without UI rebuilds. Empty string when CpURL
	// is unset — UI hides the install button rather than open a
	// broken link.
	DownloadURL string `json:"downloadURL"`
	// LastProviderTrafficAt is the RFC3339 timestamp of the most
	// recent intercepted connection whose provider_name was
	// populated (i.e. a real LLM provider call from a local app).
	// The menu-bar UI's live-traffic pulse compares this to "now"
	// each poll tick — values within the last few seconds trigger
	// a brief tray-icon highlight so the user gets at-a-glance
	// "agent is doing AI work right now" feedback. Empty string
	// when no provider traffic has been seen since daemon start.
	LastProviderTrafficAt string `json:"lastProviderTrafficAt"`
	CertExpiresAt         string `json:"certExpiresAt"`
	LastHeartbeat         string `json:"lastHeartbeat"`
	HeartbeatIntervalSec  int    `json:"heartbeatIntervalSec"`
	DeviceID              string `json:"deviceID"`
	// TrustLevel mirrors thing_agent.trust_level (0–3) as last reported
	// by Hub on the most recent enroll/renew. Surfaced via
	// CollectorConfig.TrustLevelFn so callers that don't have an
	// enrollment manager (e.g. tests) can stub it.
	TrustLevel int `json:"trustLevel"`
	// DeviceAuthMode is the operator-configured device-auth posture
	// last observed via Hub's agent-bootstrap endpoint
	// ("mtls-only" / "enterprise-login"). Empty string means
	// "unknown" — the menu bar UI treats it as a conservative default
	// (do not offer SSO).
	DeviceAuthMode string `json:"deviceAuthMode,omitempty"`
	// SSOEmail is the email address the user signed in with on the most
	// recent SSO enrollment. Empty when the device was enrolled via
	// X-Enrollment-Token (mtls-only) or has never been enrolled.
	SSOEmail string `json:"ssoEmail,omitempty"`
	// QuitAllowed mirrors the daemon's runtime policy on whether the
	// SHUTDOWN IPC will be honoured. The menu-bar UI uses this to
	// decide whether to surface "Restart Agent" and "Quit Nexus
	// Agent" — when false (the prod default for compliance
	// always-on), both menu items are omitted so users never see
	// "Restart blocked by policy" errors after the fact. Defaults to
	// true (legacy behaviour and dev / test) when the daemon does not
	// wire a provider.
	QuitAllowed bool `json:"quitAllowed"`
}

// StatusSnapshot is the full state returned to the GUI.
type StatusSnapshot struct {
	State            string            `json:"state"`
	StateReason      string            `json:"stateReason"`
	GatewayConnected bool              `json:"gatewayConnected"`
	TodayStats       TodayStats        `json:"todayStats"`
	RecentEvents     []RecentEvent     `json:"recentEvents"`
	AuditQueue       AuditQueueInfo    `json:"auditQueue"`
	Agent            AgentInfo         `json:"agent"`
	ShutdownWarning  map[string]string `json:"shutdownWarning"`
	DashboardURL     string            `json:"dashboardURL"`
	ConfigSummary    ConfigSummary     `json:"configSummary"`
	// Paused mirrors the kill switch — true means the agent is
	// currently in passthrough mode. Set by user (menu bar) or by
	// admin via Hub shadow; the menu bar reads both via this field
	// so it stays in sync regardless of who paused.
	Paused bool `json:"paused"`
	// PausedUntil is the RFC3339 timestamp the user-pause auto-resumes
	// at, when an auto-resume timer is active. Empty string when
	// paused indefinitely or not paused.
	PausedUntil string `json:"pausedUntil,omitempty"`
}

// CollectorConfig holds the immutable initialization parameters for a Collector.
type CollectorConfig struct {
	Version         string
	DeviceID        string
	DashboardURL    string
	DownloadURL     string // full URL for the "Install update" button; e.g. "<cpURL>/downloads/NexusAgent-latest.pkg".
	CertExpiresAt   time.Time
	HeartbeatSec    int
	UnsyncedCountFn func() int
	// TodayStatsFn is an optional provider invoked by Collect() to refresh
	// TodayStats from the agent's local audit roll-up.
	TodayStatsFn func() TodayStats

	// RecentEventsFn returns the top N most-recent traffic events for
	// the Overview "Recent activity" table. nil = leave RecentEvents
	// empty (legacy behaviour). Wire to audit.Queue.QueryRecent in
	// main.go so the table actually populates (without this wiring, the
	// Overview always renders "No lifecycle events yet").
	RecentEventsFn func(limit int) []RecentEvent
	// ThingClient exposes the subset of *thingclient.Client that
	// BuildConfigSummary reads. Optional — when nil, the emitted
	// StatusSnapshot.ConfigSummary is zero-valued.
	ThingClient ThingStateAccessor
	// TrustLevelFn returns the agent's current trust_level (0–3) as
	// of the most recent enroll/renew. Optional — when nil the
	// snapshot reports level 0.
	TrustLevelFn func() int
	// DeviceAuthModeFn returns the operator-configured device-auth
	// mode last observed via Hub's bootstrap endpoint
	// ("mtls-only" / "enterprise-login"). Optional — when nil the
	// snapshot reports an empty string ("unknown").
	DeviceAuthModeFn func() string
	// SSOEmailFn returns the user identity captured during the most
	// recent SSO enrollment. Optional — when nil the snapshot reports
	// an empty string.
	SSOEmailFn func() string
	// QuitAllowedFn returns the daemon's runtime policy on whether
	// SHUTDOWN-style IPC verbs are honoured. The same predicate that
	// gates status.Server.handleShutdown — surfaced into the
	// snapshot so the menu-bar UI can hide affordances that would
	// otherwise click through to "blocked by policy". Optional — when
	// nil the snapshot reports `quitAllowed=true` so dev / test
	// builds keep the menu items visible.
	QuitAllowedFn func() bool
	// PausedFn returns whether protection is currently paused (kill
	// switch engaged), regardless of whether the cause was the
	// menu-bar UI or an admin shadow push. Optional — when nil the
	// snapshot reports paused=false.
	PausedFn func() bool
	// PausedUntilFn returns the scheduled auto-resume time when a
	// finite user-pause is active, or the zero value otherwise.
	// Optional — when nil the snapshot reports pausedUntil="".
	PausedUntilFn func() (time.Time, bool)
	// InterceptionHealthFn returns the OS-capture-layer attach state
	// (NE Transparent Proxy on macOS, iptables/netfilter on linux,
	// NexusWFP on windows). Optional — when nil the snapshot reports
	// a zero-value Health and the computeState logic skips the "not
	// connected" branch. Wired by main.go from the platform.Platform
	// implementation that satisfies InterceptionHealthReporter.
	InterceptionHealthFn func() InterceptionHealth
	// NowFn overrides the current-time source for tests. Production
	// leaves it nil; collector uses time.Now.
	NowFn func() time.Time
}

// Collector aggregates status from all subsystems. All fields are unexported
// and accessed only through thread-safe methods.
type Collector struct {
	mu sync.RWMutex

	// Immutable after construction (set in NewCollector, never changed).
	version         string
	deviceID        string
	dashboardURL    string
	downloadURL     string
	certExpiresAt   time.Time
	heartbeatSec    int
	unsyncedCountFn func() int

	// Settable once after construction via SetThingClient (the thingclient
	// constructor runs after NewCollector in cmd/agent/main.go). Read under mu.
	thingClient ThingStateAccessor

	// Cat B (HTTP-pulled) shadow snapshot accessor. Optional — when nil,
	// BuildConfigSummary falls back to thingClient.SnapshotDesired() which
	// is empty for HTTP-pulled keys (interception_domains, hooks,
	// exemptions). Setting this fixes the long-standing bug where
	// the menu-bar's ConfigSummary always reported 0 domains/hooks even
	// when 64 domains and 4 hooks were active.
	cacheGetFn func(key string) json.RawMessage

	trustLevelFn         func() int
	deviceAuthModeFn     func() string
	ssoEmailFn           func() string
	quitAllowedFn        func() bool
	pausedFn             func() bool
	pausedUntilFn        func() (time.Time, bool)
	interceptionHealthFn func() InterceptionHealth
	nowFn                func() time.Time
	// todayStatsFn is an optional provider. When non-nil, Collect() calls
	// it to refresh c.todayStats from the local audit queue's today-window
	// SQL roll-up. Nil leaves todayStats at its zero value (today UI
	// surfaces show "—" / 0).
	todayStatsFn func() TodayStats

	// recentEventsFn returns the top-N most-recent traffic events.
	// Wired by main.go to audit.Queue.QueryRecent so the Overview
	// "Recent activity" table populates. Nil leaves the field empty.
	recentEventsFn func(limit int) []RecentEvent

	// Mutable — only written through setter methods under mu.Lock().
	enrolled         bool
	gatewayConnected bool
	lastHeartbeat    time.Time
	updateAvailable  bool
	lastSyncTime     time.Time
	shutdownWarning  map[string]string
	todayStats       TodayStats
	recentEvents     []RecentEvent
	// lastProviderTrafficAt records the timestamp of the most recent
	// audit event whose provider_name field was non-empty — i.e. the
	// agent saw a real LLM call on this device. The menu-bar UI
	// reads this to render a brief tray-icon pulse so the user
	// gets at-a-glance "agent is doing AI work right now" feedback.
	// Zero time = no provider traffic since boot.
	lastProviderTrafficAt time.Time
}

// NewCollector creates a Collector with the given initial configuration.
// All mutable state starts at its zero value except enrolled=true.
func NewCollector(cfg CollectorConfig) *Collector {
	return &Collector{
		version:              cfg.Version,
		deviceID:             cfg.DeviceID,
		dashboardURL:         cfg.DashboardURL,
		downloadURL:          cfg.DownloadURL,
		certExpiresAt:        cfg.CertExpiresAt,
		heartbeatSec:         cfg.HeartbeatSec,
		unsyncedCountFn:      cfg.UnsyncedCountFn,
		todayStatsFn:         cfg.TodayStatsFn,
		recentEventsFn:       cfg.RecentEventsFn,
		thingClient:          cfg.ThingClient,
		trustLevelFn:         cfg.TrustLevelFn,
		deviceAuthModeFn:     cfg.DeviceAuthModeFn,
		ssoEmailFn:           cfg.SSOEmailFn,
		quitAllowedFn:        cfg.QuitAllowedFn,
		pausedFn:             cfg.PausedFn,
		pausedUntilFn:        cfg.PausedUntilFn,
		interceptionHealthFn: cfg.InterceptionHealthFn,
		nowFn:                cfg.NowFn,
		enrolled:             true,
		gatewayConnected:     true,
		lastHeartbeat:        time.Now(),
	}
}

// SetThingClient installs or replaces the thingclient accessor used by
// BuildConfigSummary. Safe to call after construction — writes are guarded by
// the collector mutex.
func (c *Collector) SetThingClient(tc ThingStateAccessor) {
	c.mu.Lock()
	c.thingClient = tc
	c.mu.Unlock()
}

// SetSnapshotCacheGetter wires the Cat B shadow-snapshot reader so
// BuildConfigSummary can count interception_domains / hooks /
// exemptions, which are HTTP-pulled and absent from the
// thingclient desired snapshot. Pass policiesCache.Get from cmd/agent.
// nil leaves ConfigSummary reading only thingClient (and reporting 0
// for HTTP-pulled keys, which is the bug this method fixes).
func (c *Collector) SetSnapshotCacheGetter(fn func(key string) json.RawMessage) {
	c.mu.Lock()
	c.cacheGetFn = fn
	c.mu.Unlock()
}

// SetInterceptionHealthFn wires the OS-capture-layer health provider
// after construction. The platform shim is initialized later in main
// than the status collector, so this setter (mirroring
// SetThingClient's pattern) avoids needing to reorder daemon startup.
// Safe for concurrent use.
// SetTodayStatsFn wires a callback that returns today's TodayStats
// (counters + phase averages) computed from the agent's local audit
// queue. Called once during cmd/agent/main.go wire-up. nil is fine —
// leaves TodayStats permanently at zero.
func (c *Collector) SetTodayStatsFn(fn func() TodayStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.todayStatsFn = fn
}

// SetRecentEventsFn wires a callback that returns the top-N most
// recent traffic events for the Overview "Recent activity" table.
// Called once during main.go wire-up. nil is fine — leaves
// RecentEvents permanently empty (legacy behaviour).
func (c *Collector) SetRecentEventsFn(fn func(limit int) []RecentEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recentEventsFn = fn
}

func (c *Collector) SetInterceptionHealthFn(fn func() InterceptionHealth) {
	c.mu.Lock()
	c.interceptionHealthFn = fn
	c.mu.Unlock()
}

// SetGatewayConnected updates the gateway connection status (thread-safe).
func (c *Collector) SetGatewayConnected(connected bool) {
	c.mu.Lock()
	c.gatewayConnected = connected
	c.mu.Unlock()
}

// SetLastHeartbeat records a successful heartbeat time (thread-safe).
func (c *Collector) SetLastHeartbeat(t time.Time) {
	c.mu.Lock()
	c.lastHeartbeat = t
	c.mu.Unlock()
}

// SetLastSyncTime records the last successful audit sync time (thread-safe).
func (c *Collector) SetLastSyncTime(t time.Time) {
	c.mu.Lock()
	c.lastSyncTime = t
	c.mu.Unlock()
}

// MarkProviderTraffic records "right now" as the most recent moment the
// agent saw a traffic_event with provider_name set (i.e. an LLM call
// from a local app to an AI provider — what the menu's live-traffic
// pulse cares about). connectionBridge.recordEvent calls this every
// time a provider-tagged audit row gets written. Thread-safe.
func (c *Collector) MarkProviderTraffic() {
	c.mu.Lock()
	c.lastProviderTrafficAt = time.Now().UTC()
	c.mu.Unlock()
}

// SetUpdateAvailable marks whether an update is available (thread-safe).
func (c *Collector) SetUpdateAvailable(available bool) {
	c.mu.Lock()
	c.updateAvailable = available
	c.mu.Unlock()
}

// SetShutdownWarning replaces the current per-locale shutdown warning text
// (thread-safe). Called by the agent_settings shadow adapter when CP
// Device Defaults updates land. The map is keyed by locale code ("en",
// "zh", "es", ...); the Wails Dashboard / Swift menu pick the entry that
// matches the user's UI language.
func (c *Collector) SetShutdownWarning(warnings map[string]string) {
	c.mu.Lock()
	// Copy so callers can't mutate our snapshot through their reference
	// after the call returns.
	if warnings == nil {
		c.shutdownWarning = nil
	} else {
		cp := make(map[string]string, len(warnings))
		for k, v := range warnings {
			cp[k] = v
		}
		c.shutdownWarning = cp
	}
	c.mu.Unlock()
}

// Collect builds a StatusSnapshot from current state (thread-safe).
// UnsyncedCountFn is called once outside the lock to avoid blocking I/O
// under the read lock.
func (c *Collector) Collect() StatusSnapshot {
	unsyncedCount := 0
	if c.unsyncedCountFn != nil {
		unsyncedCount = c.unsyncedCountFn()
	}

	// Refresh today's stats from the audit queue's roll-up. Read fn
	// under RLock then call without holding the lock (the callback talks
	// to SQLite and may block). Stash result under Lock(). Safe because
	// Collect itself runs serially on the status poll cadence.
	if c.todayStatsFn != nil {
		ts := c.todayStatsFn()
		c.mu.Lock()
		c.todayStats = ts
		c.mu.Unlock()
	}

	// Refresh recent traffic events from the audit queue. Same
	// off-lock pattern as todayStatsFn — the callback hits SQLite
	// and we don't want to hold the snapshot mutex through the I/O.
	if c.recentEventsFn != nil {
		re := c.recentEventsFn(5)
		c.mu.Lock()
		c.recentEvents = re
		c.mu.Unlock()
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	state, reason := c.computeState(unsyncedCount)

	return StatusSnapshot{
		State:            state,
		StateReason:      reason,
		GatewayConnected: c.gatewayConnected,
		TodayStats:       c.todayStats,
		RecentEvents:     c.recentEvents,
		AuditQueue: AuditQueueInfo{
			UnsyncedCount: unsyncedCount,
			LastSyncTime:  formatTime(c.lastSyncTime),
		},
		Agent: AgentInfo{
			Version:               c.version,
			UpdateAvailable:       c.updateAvailable,
			DownloadURL:           c.downloadURL,
			LastProviderTrafficAt: formatTime(c.lastProviderTrafficAt),
			CertExpiresAt:         formatTime(c.certExpiresAt),
			LastHeartbeat:         formatTime(c.lastHeartbeat),
			HeartbeatIntervalSec:  c.heartbeatSec,
			DeviceID:              c.deviceID,
			TrustLevel:            callIntFn(c.trustLevelFn),
			DeviceAuthMode:        callStringFn(c.deviceAuthModeFn),
			SSOEmail:              callStringFn(c.ssoEmailFn),
			QuitAllowed:           callBoolFnDefault(c.quitAllowedFn, true),
		},
		ShutdownWarning: c.shutdownWarning,
		DashboardURL:    c.dashboardURL,
		ConfigSummary:   BuildConfigSummary(c.thingClient, c.cacheGetFn),
		Paused:          callBoolFn(c.pausedFn),
		PausedUntil:     formatPausedUntil(c.pausedUntilFn),
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// callIntFn / callStringFn defer evaluation of optional provider
// callbacks until Collect runs and tolerate nil providers so tests and
// minimal harnesses don't have to wire every accessor.
func callIntFn(fn func() int) int {
	if fn == nil {
		return 0
	}
	return fn()
}

func callStringFn(fn func() string) string {
	if fn == nil {
		return ""
	}
	return fn()
}

func callBoolFn(fn func() bool) bool {
	if fn == nil {
		return false
	}
	return fn()
}

// callBoolFnDefault evaluates the optional provider, returning the
// supplied default when no provider is wired. Used for flags where the
// safe legacy behaviour is the "true" path (e.g. quitAllowed defaults
// to permitting shutdown so dev / test harnesses without an explicit
// provider behave the same as before this knob was introduced).
func callBoolFnDefault(fn func() bool, def bool) bool {
	if fn == nil {
		return def
	}
	return fn()
}

func formatPausedUntil(fn func() (time.Time, bool)) string {
	if fn == nil {
		return ""
	}
	t, ok := fn()
	if !ok || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
