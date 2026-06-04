package thingclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/platform"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
)

// dialHTTPClient returns an *http.Client whose Transport carries the
// process-wide [nexushttp.GlobalDialControl] callback (Linux agent
// installs SO_MARK on every outbound socket). When no global control
// is set (macOS, Windows, services that don't intercept their own
// traffic), the returned client is functionally equivalent to
// http.DefaultClient with a dialer tuned for short connection holds.
func dialHTTPClient() *http.Client {
	control := nexushttp.GlobalDialControl()
	if control == nil {
		return nexushttp.New(nexushttp.Config{})
	}
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   control,
		}).DialContext,
		ForceAttemptHTTP2: true,
	}
	return &http.Client{Transport: tr}
}

// Config holds all settings for the Thing Client.
type Config struct {
	// HubURL is the WebSocket endpoint of the Hub server.
	// Example: "wss://hub.nexus.internal:3060/ws"
	HubURL string

	// HubHTTPURL is the HTTP base URL for fallback mode and audit upload.
	// Example: "https://hub.nexus.internal:3060"
	// If empty, derived from HubURL by stripping the /ws path and changing scheme.
	HubHTTPURL string

	// ThingType identifies what kind of service this is.
	// One of: "control-plane", "ai-gateway", "compliance-proxy", "agent"
	ThingType string

	// ThingID is the unique identifier for this Thing instance.
	ThingID string

	// PhysicalID is the stable natural-key identity of this Thing.
	//
	//   - Agents: 32-hex hardware fingerprint (sha256 of IOPlatformUUID +
	//     serial + MAC + cpu brand) computed by `shared/opsmetrics`. Hub
	//     enforces a partial UNIQUE constraint on agent rows so a reinstall
	//     reuses the same thing_id.
	//   - Services: optional operator-supplied stable id (typically the
	//     yaml `id` field). Empty is fine — the row's primary key (ThingID,
	//     usually hostname+type+port) is already the natural key for
	//     services and physical_id is informational. The DB unique
	//     constraint does NOT apply to non-agent rows.
	PhysicalID string

	// ThingName is a human-readable display label for this Thing in the
	// admin UI (Nodes list, overrides, etc.). Optional — when empty the
	// Hub falls back to ThingID so the column never renders blank. Each
	// service typically populates it from os.Hostname() or a config
	// override at startup.
	ThingName string

	// ThingVersion is the software version of this service binary.
	ThingVersion string

	// ListenAddress is the host:port this service listens on (nil for agent).
	ListenAddress string

	// RuntimeAPIURL is the runtime config endpoint (nil for agent).
	RuntimeAPIURL string

	// MetricsURL is the Prometheus metrics endpoint (nil for agent).
	MetricsURL string

	// ManagementURL is the HTTP base URL for the management API (e.g. http://host:port).
	// Used by the Control Plane to discover management endpoints such as /management/ca-cert.
	// Nil for agents.
	ManagementURL string

	// Role is the operational role of this Thing (e.g., "default", "primary", "canary").
	Role string

	// Token is the Bearer token for authenticating with Hub.
	Token string

	// Logger is the structured logger. Must not be nil.
	Logger *slog.Logger

	// MetricsRegisterer is the Prometheus registerer for client metrics.
	// If nil, prometheus.DefaultRegisterer is used.
	MetricsRegisterer prometheus.Registerer

	// MetricsNamespace is the Prometheus namespace prefix (default "nexus").
	MetricsNamespace string

	// MQProducer is the optional MQ producer for server-side Things.
	// Nil for Agent Things that do not have direct MQ access.
	MQProducer mq.Producer

	// ReconnectMaxBackoff is the maximum backoff duration for reconnection.
	// Default: 30 seconds.
	ReconnectMaxBackoff time.Duration

	// ReconnectInitialBackoff is the initial backoff duration for reconnection.
	// Default: 1 second.
	ReconnectInitialBackoff time.Duration

	// HeartbeatInterval is how often to send heartbeat in HTTP fallback mode.
	// Default: 15 seconds.
	HeartbeatInterval time.Duration

	// WSFailureThreshold is the number of consecutive WS failures before
	// switching to HTTP fallback mode. Default: 3.
	WSFailureThreshold int

	// MQBufferSize is the capacity of the local event buffer for MQ failures.
	// Default: 10000.
	MQBufferSize int

	// ShutdownTimeout is the max time to wait for graceful shutdown.
	// Default: 5 seconds.
	ShutdownTimeout time.Duration

	// OpsMetricsSampler is the optional ops-metrics + L1/L3 sampler. When
	// non-nil, the client invokes Sampler.Collect() on every heartbeat tick
	// and pushes the resulting SampleBatch to Hub via PushMetricsSample.
	//
	// Per ops-metrics spec §7.1 a metrics_sample WS message is emitted on
	// every heartbeat tick (default 15s, spec target 30s — set
	// HeartbeatInterval to override). The metrics ticker runs independently
	// of the WS / HTTP-fallback transport state machine, so samples flow in
	// both modes.
	//
	// Leave nil if the binary has not yet wired its L3 metric registry —
	// the client emits no metrics_sample messages and existing behavior is
	// unchanged.
	OpsMetricsSampler *opsmetrics.Sampler
}

func (c *Config) setDefaults() {
	if c.ReconnectMaxBackoff == 0 {
		c.ReconnectMaxBackoff = 30 * time.Second
	}
	if c.ReconnectInitialBackoff == 0 {
		c.ReconnectInitialBackoff = 1 * time.Second
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.WSFailureThreshold == 0 {
		c.WSFailureThreshold = 3
	}
	if c.MQBufferSize == 0 {
		c.MQBufferSize = 10000
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 5 * time.Second
	}
	if c.MetricsNamespace == "" {
		c.MetricsNamespace = "nexus"
	}
	if c.MetricsRegisterer == nil {
		c.MetricsRegisterer = prometheus.DefaultRegisterer
	}
}

// Mode represents the current operating mode of the client.
type Mode int

const (
	ModeDisconnected Mode = iota
	ModeWSConnecting
	ModeWSConnected
	ModeHTTPFallback
)

func (m Mode) String() string {
	switch m {
	case ModeDisconnected:
		return "disconnected"
	case ModeWSConnecting:
		return "ws_connecting"
	case ModeWSConnected:
		return "ws_connected"
	case ModeHTTPFallback:
		return "http_fallback"
	default:
		return "unknown"
	}
}

// ConfigState represents a single config key's state in the shadow.
type ConfigState struct {
	State   json.RawMessage `json:"state"`
	Version int64           `json:"version"`
}

// hubMessage is the envelope for all Hub->Thing WebSocket messages.
//
// Two payload shapes are supported:
//   - Full snapshot: Desired + DesiredVer populated (sent on "connected" and
//     legacy "config_changed" full-map pushes)
//   - Per-key delta: ConfigKey + State + DesiredVer populated (sent on
//     "config_changed" per-key delta pushes from Hub)
type hubMessage struct {
	Type       string                 `json:"type"`
	ThingID    string                 `json:"thingId,omitempty"`
	Desired    map[string]ConfigState `json:"desired,omitempty"`
	DesiredVer int64                  `json:"desiredVer,omitempty"`
	ConfigKey  string                 `json:"configKey,omitempty"`
	State      json.RawMessage        `json:"state,omitempty"`
	// Force, when true, tells the client to run the OnConfigChanged callback
	// and emit a shadow_report even if DesiredVer <= reportedVer. Used by
	// admin-triggered "Re-sync this key" replays where the Hub deliberately
	// does not bump the template version but still wants the Thing to
	// re-apply + re-report the current state.
	Force bool `json:"force,omitempty"`
}

// thingMessage is the envelope for all Thing->Hub WebSocket messages.
//
// Break-glass fields (Reason / SourceIP / ActorTokenID / KeyVersions) are
// only populated on a `shadow_report_break_glass` message — they carry the
// audit context Hub needs to write the emergency_override row and queue the
// reconciliation job. Plain `shadow_report` leaves them zero.
type thingMessage struct {
	Type string `json:"type"`
	// Reported carries per-key raw state, matching the shape Hub stores for
	// thing.desired (set via jsonb_set from UpsertConfigTemplate). Sending
	// flat raw state keeps thing.reported[key] byte-comparable with
	// thing.desired[key] so GetShadowComparison reports accurate per-key
	// sync status. Per-key version metadata travels on the global
	// ReportedVer field below and on the separate KeyVersions map for
	// break-glass reports — it is not wrapped into individual entries.
	Reported    map[string]json.RawMessage `json:"reported,omitempty"`
	ReportedVer int64                      `json:"reportedVer,omitempty"`
	// ReportedOutcomes carries the per-key apply outcome (last successful
	// applied_at/applied_version + most recent apply_error). Lives
	// alongside the byte-comparable Reported map rather than wrapped
	// inside it so old Hub instances ignore the field (unknown JSON
	// keys are dropped on decode) and the existing per-key byte-diff
	// flow stays unchanged. Empty map when the Thing has no apply
	// outcomes to report (initial registration, before any apply ran).
	ReportedOutcomes map[string]ApplyOutcome `json:"reportedOutcomes,omitempty"`

	// --- break-glass metadata (shadow_report_break_glass only) ---
	Reason       string           `json:"reason,omitempty"`
	SourceIP     string           `json:"sourceIp,omitempty"`
	ActorTokenID string           `json:"actorTokenId,omitempty"`
	KeyVersions  map[string]int64 `json:"keyVersions,omitempty"`
}

// Client is the Thing Client that manages the connection to Hub.
// All exported methods are safe for concurrent use.
type Client struct {
	cfg    Config
	logger *slog.Logger

	mu     sync.RWMutex
	mode   Mode
	wsConn *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	desiredVer  atomic.Int64
	reportedVer atomic.Int64

	// perKeyVersion tracks the per-config_key version last observed from Hub.
	// Keys are config_key names (e.g. "killswitch"), values are the version
	// attached to each ConfigState. Populated by connectWS (full snapshot)
	// and handleHubMessage (per-key delta) and the HTTP fallback paths;
	// read by KeyVersion(). Uses sync.Map to keep the hot-path lock-free.
	perKeyVersion sync.Map

	// lastReportedAt holds the RFC3339 timestamp of the most recent
	// successful shadow_report. Populated by sendShadowReportWS /
	// sendShadowReportHTTP; read by LastReportedAt(). atomic.Value wraps
	// a string so the read path can use Load()/Store() without a mutex.
	lastReportedAt atomic.Value // string

	// lastReportedAtNanos mirrors lastReportedAt as unix nanoseconds for
	// callers that need an age computation without parsing RFC3339. Zero
	// means no report has been sent yet. Written at the same sites as
	// lastReportedAt; read by LastReportedAtTime().
	lastReportedAtNanos atomic.Int64

	onConfigChanged OnConfigChangedFunc
	// outcomes is the per-client apply-outcome ledger. Services call
	// c.Outcomes().Record(key, ver, err) from inside their dispatch
	// switch; the Client lifts Snapshot() into every outgoing
	// shadow_report so Hub stores the latest per-key outcomes for the
	// Nodes-page apply-error / last-good-version indicators.
	outcomes     *OutcomeTracker
	onDisconnect func()
	onReconnect  func()
	// onHeartbeatTick fires after every successful heartbeat tick
	// (metrics-sample push). Callers use this to refresh a "last
	// heartbeat" status field that reflects live liveness, not just
	// the most recent WS reconnect time. nil = no callback.
	onHeartbeatTick func()

	// desiredCache is the client's authoritative view of desired state, merged
	// from the initial snapshot plus every per-key delta received from Hub.
	// Guarded by mu for write access. Callbacks receive a shallow copy —
	// callers must not mutate State bytes in-place.
	desiredCache map[string]ConfigState

	// outChControl carries must-deliver, low-rate Thing→Hub messages
	// (shadow_report, shadow_report_break_glass, static_info). writePump
	// drains this channel preferentially so a flood of metrics_sample /
	// diag_event cannot starve a config-apply report under back-pressure.
	outChControl chan []byte
	// outChMetrics carries high-rate, drop-tolerable telemetry
	// (metrics_sample, diag_event). Sized larger because the heartbeat
	// ticker bursts more often than the shadow path.
	outChMetrics chan []byte

	wsConsecutiveFailures atomic.Int32

	promMetrics *clientMetrics

	hc       *httpClient
	mqBuffer *ringBuffer

	// connectWSFn allows tests to inject a fake WS dialer. Production code leaves
	// it nil; runHTTPFallback falls back to c.connectWS in that case.
	connectWSFn func(context.Context) error

	// runHTTPFallbackFn allows tests to inject a fake HTTP fallback loop. Production
	// code leaves it nil; runLoop falls back to c.runHTTPFallback in that case.
	runHTTPFallbackFn func(context.Context)

	// heartbeatIntervalNS allows runtime cadence changes without restarting tickers.
	// 0 means "use cfg.HeartbeatInterval". SetHeartbeatInterval updates the
	// atomic AND broadcasts on heartbeatKick so existing tickers re-arm with
	// the new value on the very next iteration.
	heartbeatIntervalNS atomic.Int64
	heartbeatKick       atomic.Pointer[chan struct{}]
}

// CurrentHeartbeatInterval returns the cadence in effect right now,
// taking SetHeartbeatInterval overrides into account. Used by both
// internal tickers and by tests that want to assert a shadow change
// took effect.
func (c *Client) CurrentHeartbeatInterval() time.Duration {
	if ns := c.heartbeatIntervalNS.Load(); ns > 0 {
		return time.Duration(ns)
	}
	return c.cfg.HeartbeatInterval
}

// SetHeartbeatInterval swaps the heartbeat / metrics-push cadence at runtime.
// The new interval is applied via atomic.Int64 and a broadcast kick channel
// so both the metrics-sample ticker and the HTTP-fallback heartbeat ticker
// re-arm on their next loop iteration without waiting out the old interval.
//
// Non-positive durations are ignored — the caller would otherwise disable
// heartbeat entirely, which would silently break drift detection on Hub.
func (c *Client) SetHeartbeatInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	c.heartbeatIntervalNS.Store(int64(d))
	c.broadcastHeartbeatKick()
}

// broadcastHeartbeatKick closes the current kick channel (waking every
// ticker selecting on it) and installs a fresh channel for the next
// round. Idempotent under concurrent Set calls — even if two Sets race
// to close the same channel, exactly one wins (close panics are
// avoided by the swap-then-close ordering).
func (c *Client) broadcastHeartbeatKick() {
	newCh := make(chan struct{})
	old := c.heartbeatKick.Swap(&newCh)
	if old != nil {
		close(*old)
	}
}

// New creates a new Thing Client with the given configuration.
// Call Start() to begin the connection lifecycle.
func New(cfg Config) (*Client, error) {
	cfg.setDefaults()

	if cfg.HubURL == "" {
		return nil, fmt.Errorf("thingclient: HubURL is required")
	}
	if cfg.ThingType == "" {
		return nil, fmt.Errorf("thingclient: ThingType is required")
	}
	if cfg.ThingID == "" {
		return nil, fmt.Errorf("thingclient: ThingID is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("thingclient: Token is required")
	}
	if cfg.Logger == nil {
		return nil, fmt.Errorf("thingclient: Logger is required")
	}

	logger := cfg.Logger.With(
		slog.String("thing_id", cfg.ThingID),
		slog.String("thing_type", cfg.ThingType),
	)

	c := &Client{
		cfg:          cfg,
		logger:       logger,
		mode:         ModeDisconnected,
		desiredCache: make(map[string]ConfigState),
		outcomes:     NewOutcomeTracker(),
		// Total buffer of 96 slots, biased toward metrics (high rate).
		// Control buffer is small but writePump drains it preferentially,
		// so even at queue depth 32 a shadow_report is sent before any
		// metrics_sample sitting on outChMetrics.
		outChControl: make(chan []byte, 32),
		outChMetrics: make(chan []byte, 64),
		done:         make(chan struct{}),
	}

	// Seed the heartbeat kick channel so the first SetHeartbeatInterval
	// has something to close (and the ticker loops have a non-nil
	// channel to select on).
	initialKick := make(chan struct{})
	c.heartbeatKick.Store(&initialKick)

	c.promMetrics = newClientMetrics(cfg.MetricsRegisterer, cfg.MetricsNamespace)

	return c, nil
}

// Start begins the Thing Client lifecycle. It connects to Hub (or falls
// back to HTTP) and runs until ctx is cancelled or Close() is called.
// Start is non-blocking — it launches background goroutines and returns.
func (c *Client) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()

	c.startBufferDrainer(runCtx)

	go c.runLoop(runCtx)

	if c.cfg.OpsMetricsSampler != nil {
		go c.runMetricsTicker(runCtx)
	}

	return nil
}

// runMetricsTicker periodically invokes tickHeartbeat to push a metrics_sample
// to Hub. Runs independently of the WS / HTTP-fallback transport state machine
// so samples flow in both modes; PushMetricsSample is best-effort and drops
// on outbox backpressure (counted via outbox_dropped_total).
//
// Cadence comes from CurrentHeartbeatInterval, which honors SetHeartbeatInterval
// overrides. Each iteration re-arms a Timer from the live value; SetHeartbeatInterval
// also broadcasts on heartbeatKick so an in-flight wait wakes early instead of
// running out the old interval.
func (c *Client) runMetricsTicker(ctx context.Context) {
	for {
		d := c.CurrentHeartbeatInterval()
		kickPtr := c.heartbeatKick.Load()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			c.tickHeartbeat(ctx)
		case <-*kickPtr:
			// Interval changed; stop the in-flight timer and loop back
			// to re-read CurrentHeartbeatInterval.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

// tickHeartbeat performs the per-tick housekeeping that is common across
// transport modes. Two responsibilities:
//
//   - Push a metrics_sample to Hub (optional, gated on
//     Config.OpsMetricsSampler being configured).
//   - Fire the registered OnHeartbeatTick callback so listeners can
//     refresh "live liveness" fields (e.g. statusapi.lastHeartbeat
//     which previously only moved on WS reconnect, masking a fully
//     healthy daemon as silent for hours).
//
// The callback fires even when OpsMetricsSampler is nil — heartbeat
// liveness is independent of metrics push wiring.
//
// Push errors are logged at debug level — push failures are best-effort
// and visible via outbox_dropped_total / Hub-side metrics.dropped_total.
// The OnHeartbeatTick callback still fires on push failure so the UI
// reflects "agent is alive trying" rather than going silent on first
// failure.
//
// Exposed on *Client (not an inner closure) so unit tests can drive it
// synchronously without a real ticker.
func (c *Client) tickHeartbeat(ctx context.Context) {
	if c.cfg.OpsMetricsSampler != nil {
		batch := c.cfg.OpsMetricsSampler.Collect()
		if err := c.PushMetricsSample(ctx, batch); err != nil {
			c.logger.Debug("metrics_sample push failed",
				slog.String("event", "metrics_sample_dropped"),
				slog.String("error", err.Error()),
			)
		}
	}

	c.mu.RLock()
	cb := c.onHeartbeatTick
	c.mu.RUnlock()
	if cb != nil {
		// Run in a separate goroutine so a slow listener cannot stall
		// the metrics ticker (it loops every HeartbeatInterval and
		// the ops-metrics push above shares this goroutine).
		go cb()
	}
}

// transport is the current connection mode inside runLoop's state machine.
type transport int

const (
	transportWS transport = iota
	transportHTTP
)

// runLoop is the main lifecycle loop implemented as an explicit transport
// state machine. It attempts WebSocket, switches to HTTP fallback after
// WSFailureThreshold consecutive failures, and returns to WS once the HTTP
// fallback loop recovers (or ctx is cancelled).
func (c *Client) runLoop(ctx context.Context) {
	defer close(c.done)

	state := transportWS
	failures := 0

	dial := c.connectWS
	if c.connectWSFn != nil {
		dial = c.connectWSFn
	}
	fallback := c.runHTTPFallback
	if c.runHTTPFallbackFn != nil {
		fallback = c.runHTTPFallbackFn
	}

	for {
		if ctx.Err() != nil {
			c.setMode(ModeDisconnected)
			return
		}

		switch state {
		case transportWS:
			err := dial(ctx)
			if err == nil {
				c.wsConsecutiveFailures.Store(0)
				failures = 0
				c.setMode(ModeWSConnected)
				c.promMetrics.wsConnections.WithLabelValues("success").Inc()
				c.promMetrics.wsConnected.Set(1)
				c.mu.RLock()
				reconnectCB := c.onReconnect
				c.mu.RUnlock()
				if reconnectCB != nil {
					reconnectCB()
				}
				c.runWSSession(ctx)
				c.promMetrics.wsConnected.Set(0)
				c.mu.RLock()
				disconnectCB := c.onDisconnect
				c.mu.RUnlock()
				if disconnectCB != nil {
					disconnectCB()
				}
				c.waitBackoff(ctx, failures)
				continue
			}
			c.promMetrics.wsConnections.WithLabelValues("failure").Inc()
			failures++
			c.wsConsecutiveFailures.Store(int32(failures))
			// Demote the very first failure to DEBUG: when all 4 services boot
			// in parallel, each Thing fails its first WS dial because Hub is
			// also still starting. The next backoff cycle succeeds. WARN here
			// trains users to ignore the line; reserve WARN for failures ≥ 2.
			if failures == 1 {
				c.logger.Debug("WebSocket connection failed (first attempt — boot race expected)",
					slog.String("event", "ws_connect_failed"),
					slog.Int("consecutive_failures", failures),
					slog.String("error", err.Error()),
				)
			} else {
				c.logger.Warn("WebSocket connection failed",
					slog.String("event", "ws_connect_failed"),
					slog.Int("consecutive_failures", failures),
					slog.String("error", err.Error()),
				)
			}
			if failures >= c.cfg.WSFailureThreshold {
				c.logger.Info("Switching to HTTP fallback mode",
					slog.String("event", "mode_switch"),
					slog.String("mode", "http_fallback"),
				)
				state = transportHTTP
				continue
			}
			c.waitBackoff(ctx, failures)

		case transportHTTP:
			fallback(ctx)
			// runHTTPFallback only returns when WS recovered or ctx cancelled.
			state = transportWS
			failures = 0
		}
	}
}

// waitBackoff blocks for the exponential-with-jitter backoff corresponding to
// the given failure count, or returns early if ctx is cancelled.
func (c *Client) waitBackoff(ctx context.Context, failures int) {
	backoff := c.calculateBackoffFor(failures)
	c.promMetrics.reconnectTotal.Inc()
	c.logger.Info("Reconnecting after backoff",
		slog.String("event", "reconnecting"),
		slog.Duration("backoff", backoff),
	)
	select {
	case <-ctx.Done():
	case <-time.After(backoff):
	}
}

// connectWS dials the Hub WebSocket endpoint and processes the "connected" message.
func (c *Client) connectWS(ctx context.Context) error {
	c.setMode(ModeWSConnecting)

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.cfg.Token)

	// Hub's /ws authenticator reads thing identity from query params (service
	// token path requires both id and type; device token path requires id).
	// Append them here while preserving any pre-existing query params on HubURL.
	dialURL, err := c.buildWSDialURL()
	if err != nil {
		c.setMode(ModeDisconnected)
		return fmt.Errorf("build ws url: %w", err)
	}

	conn, resp, err := websocket.Dial(dialCtx, dialURL, &websocket.DialOptions{
		HTTPHeader:   headers,
		Subprotocols: []string{"nexus.bearer", c.cfg.Token},
		// HTTPClient lets us inject a transport with the agent's
		// SO_MARK control attached on Linux. websocket.Dial uses
		// HTTPClient.Transport for the underlying TCP+TLS+upgrade,
		// so a transport built via nexushttp.New automatically
		// picks up the process-wide DialControl.
		HTTPClient: dialHTTPClient(),
	})
	if err != nil {
		c.setMode(ModeDisconnected)
		return fmt.Errorf("dial hub: %w", err)
	}
	// websocket.Dial returns the underlying HTTP handshake response; the
	// body is empty on success (101 Switching Protocols) but must still
	// be closed so the linter and the HTTP transport agree it is done.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readCancel()

	_, data, err := conn.Read(readCtx)
	if err != nil {
		_ = conn.Close(websocket.StatusAbnormalClosure, "failed to read connected message")
		c.setMode(ModeDisconnected)
		return fmt.Errorf("read connected message: %w", err)
	}

	var msg hubMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		_ = conn.Close(websocket.StatusAbnormalClosure, "invalid connected message")
		c.setMode(ModeDisconnected)
		return fmt.Errorf("unmarshal connected message: %w", err)
	}

	if msg.Type != "connected" {
		_ = conn.Close(websocket.StatusAbnormalClosure, "unexpected first message type")
		c.setMode(ModeDisconnected)
		return fmt.Errorf("expected 'connected' message, got %q", msg.Type)
	}

	c.mu.Lock()
	c.wsConn = conn
	c.mu.Unlock()

	// Flip to connected before applyConfig runs: the callback applies the
	// desired state and then calls sendShadowReport, which drops the report
	// if mode is still ws_connecting. runLoop / http-recovery call setMode
	// again on success; the duplicate is idempotent.
	c.setMode(ModeWSConnected)

	c.logger.Info("Connected to Hub",
		slog.String("event", "connected"),
		slog.String("thing_id", msg.ThingID),
		slog.Int64("desired_ver", msg.DesiredVer),
	)

	if len(msg.Desired) > 0 {
		c.mu.Lock()
		c.desiredCache = make(map[string]ConfigState, len(msg.Desired))
		for k, v := range msg.Desired {
			c.desiredCache[k] = v
		}
		c.mu.Unlock()
		c.recordKeyVersions(msg.Desired)
	}
	if msg.DesiredVer > c.reportedVer.Load() {
		c.desiredVer.Store(msg.DesiredVer)
		c.applyConfig(msg.Desired, msg.DesiredVer)
	}

	return nil
}

// buildWSDialURL returns cfg.HubURL with the full register payload set as
// query params, preserving any pre-existing params.
//
// The HTTP register fallback (httpRegister, registerRequest) sends the same
// fields as a JSON body. Mirroring them on the WS handshake prevents the
// online thing rows from landing with NULL metrics_url / version / role
// (which breaks the runtime-bridge introspection endpoint and the Nodes UI
// display fields). Empty values are omitted so optional fields stay NULL
// in the DB rather than being overwritten with "".
func (c *Client) buildWSDialURL() (string, error) {
	u, err := url.Parse(c.cfg.HubURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("id", c.cfg.ThingID)
	q.Set("type", c.cfg.ThingType)
	if c.cfg.ThingName != "" {
		q.Set("name", c.cfg.ThingName)
	}
	if c.cfg.ThingVersion != "" {
		q.Set("version", c.cfg.ThingVersion)
	}
	if c.cfg.ListenAddress != "" {
		q.Set("address", c.cfg.ListenAddress)
	}
	if c.cfg.MetricsURL != "" {
		q.Set("metricsUrl", c.cfg.MetricsURL)
	}
	if c.cfg.ManagementURL != "" {
		q.Set("managementUrl", c.cfg.ManagementURL)
	}
	if c.cfg.Role != "" {
		q.Set("role", c.cfg.Role)
	}
	if c.cfg.RuntimeAPIURL != "" {
		q.Set("runtimeApiUrl", c.cfg.RuntimeAPIURL)
	}
	if c.cfg.PhysicalID != "" {
		q.Set("physicalId", c.cfg.PhysicalID)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// recordKeyVersions stores the per-key ConfigState.Version values so that
// KeyVersion() can answer lookups without walking the desiredCache. Called
// whenever a full or partial desired map is received from Hub.
//
// Each entry's Version is the global shadow version at the time that key
// was last updated; Hub uses a single monotonic counter for all keys, so
// per-key version == global desired version by design. If Hub ever splits
// into per-key counters, both this helper and the WS per-key-delta path
// (handleHubMessage → c.perKeyVersion.Store(msg.ConfigKey, msg.DesiredVer))
// must switch together.
func (c *Client) recordKeyVersions(desired map[string]ConfigState) {
	for k, cs := range desired {
		c.perKeyVersion.Store(k, cs.Version)
	}
}

// runWSSession runs the read and write pumps for an active WebSocket connection.
// conn is snapshotted once at entry; if a subsequent connect races and replaces
// c.wsConn, this session still operates on the conn it owns and the cleanup
// won't close a newer connection owned by another session.
func (c *Client) runWSSession(ctx context.Context) {
	c.mu.RLock()
	conn := c.wsConn
	c.mu.RUnlock()
	if conn == nil {
		return
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writePump(sessionCtx, conn)
	}()

	c.readPump(sessionCtx, conn)
	sessionCancel()

	wg.Wait()

	c.mu.Lock()
	if c.wsConn == conn {
		_ = conn.Close(websocket.StatusNormalClosure, "session ended")
		c.wsConn = nil
	}
	c.mu.Unlock()
}

// readPump reads messages from the WebSocket until error or context cancellation.
func (c *Client) readPump(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logger.Warn("WebSocket read error",
				slog.String("event", "ws_read_error"),
				slog.String("error", err.Error()),
			)
			return
		}

		var msg hubMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.logger.Warn("Invalid message from Hub",
				slog.String("event", "invalid_message"),
				slog.String("error", err.Error()),
			)
			continue
		}

		c.handleHubMessage(msg)
	}
}

// handleHubMessage dispatches a decoded Hub message.
// Exposed on *Client (not an inner closure) so unit tests can drive it directly.
func (c *Client) handleHubMessage(msg hubMessage) {
	switch msg.Type {
	case "config_changed":
		c.logger.Info("Config changed",
			slog.String("event", "config_changed"),
			slog.String("config_key", msg.ConfigKey),
			slog.Int64("desired_ver", msg.DesiredVer),
			slog.Bool("force", msg.Force),
		)
		// Admin-triggered re-sync pushes the same DesiredVer on purpose and
		// must still flow through OnConfigChanged + shadow_report. Skip the
		// entry-level version gate when Force is set; applyConfig honours
		// the same flag internally so the version check there is bypassed
		// too.
		if !msg.Force && msg.DesiredVer <= c.reportedVer.Load() {
			// Hub can replay older template versions (or drift repair can race);
			// we skip apply+shadow_report here, so log at INFO — otherwise operators
			// only see "Config changed" with no follow-up success/failure lines.
			c.logger.Info("Config change skipped (local reported version already ahead)",
				slog.String("event", "config_changed_skipped_stale"),
				slog.String("config_key", msg.ConfigKey),
				slog.Int64("desired_ver", msg.DesiredVer),
				slog.Int64("reported_ver", c.reportedVer.Load()),
				slog.Bool("force", msg.Force),
			)
			return
		}
		switch {
		case msg.ConfigKey != "":
			// Per-key delta: merge into desiredCache and apply the merged map.
			merged := c.mergeDelta(msg.ConfigKey, msg.State, msg.DesiredVer)
			// Track the per-key version so KeyVersion() can surface it.
			c.perKeyVersion.Store(msg.ConfigKey, msg.DesiredVer)
			c.desiredVer.Store(msg.DesiredVer)
			if msg.Force {
				c.applyConfigForce(merged, msg.DesiredVer)
			} else {
				c.applyConfig(merged, msg.DesiredVer)
			}
		case len(msg.Desired) > 0:
			// Legacy full-snapshot config_changed path — delete once Task 1.2 (Hub
			// broadcast alignment) lands; it exists only to keep pre-1.2 tests green.
			c.mu.Lock()
			c.desiredCache = make(map[string]ConfigState, len(msg.Desired))
			for k, v := range msg.Desired {
				c.desiredCache[k] = v
			}
			c.mu.Unlock()
			c.recordKeyVersions(msg.Desired)
			c.desiredVer.Store(msg.DesiredVer)
			if msg.Force {
				c.applyConfigForce(msg.Desired, msg.DesiredVer)
			} else {
				c.applyConfig(msg.Desired, msg.DesiredVer)
			}
		default:
			c.logger.Warn("config_changed missing configKey and desired map; dropping",
				slog.String("event", "config_changed_invalid"))
		}
	default:
		c.logger.Debug("Unknown message type from Hub",
			slog.String("event", "unknown_message"),
			slog.String("msg_type", msg.Type),
		)
	}
}

// mergeDelta updates desiredCache with the new key/state/version and returns a
// copy of the post-merge map safe to hand to the OnConfigChanged callback.
func (c *Client) mergeDelta(key string, state json.RawMessage, ver int64) map[string]ConfigState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.desiredCache == nil {
		c.desiredCache = make(map[string]ConfigState)
	}
	c.desiredCache[key] = ConfigState{State: state, Version: ver}
	out := make(map[string]ConfigState, len(c.desiredCache))
	for k, v := range c.desiredCache {
		out[k] = v
	}
	return out
}

// writePump writes outgoing messages to the WebSocket. Control messages
// (shadow_report family + static_info) are drained preferentially over
// metrics (metrics_sample / diag_event) so a heartbeat-tick flood cannot
// starve a config-apply report when the Hub is briefly back-pressured.
func (c *Client) writePump(ctx context.Context, conn *websocket.Conn) {
	for {
		// Phase 1: non-blocking peek at the control queue. If anything is
		// waiting we send it before touching metrics.
		select {
		case <-ctx.Done():
			return
		case data := <-c.outChControl:
			if !c.writeFrame(ctx, conn, data) {
				return
			}
			continue
		default:
		}
		// Phase 2: nothing in control queue. Block on either; ctx.Done()
		// stays first so shutdown is observed promptly.
		select {
		case <-ctx.Done():
			return
		case data := <-c.outChControl:
			if !c.writeFrame(ctx, conn, data) {
				return
			}
		case data := <-c.outChMetrics:
			if !c.writeFrame(ctx, conn, data) {
				return
			}
		}
	}
}

// writeFrame writes one WebSocket frame with a 5s deadline. Returns false
// when the connection or context is gone and the writePump must exit.
func (c *Client) writeFrame(ctx context.Context, conn *websocket.Conn, data []byte) bool {
	// Honor an already-cancelled ctx before initiating I/O. The websocket
	// library's Write can sometimes complete before its internal ctx check
	// fires (small payloads, no cooperative cancellation point), which
	// would leak a successful write past a caller-side abort — surprising
	// for anyone using ctx as their "stop now" signal.
	if ctx.Err() != nil {
		return false
	}
	writeCtx, writeCancel := context.WithTimeout(ctx, 5*time.Second)
	err := conn.Write(writeCtx, websocket.MessageText, data)
	writeCancel()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		c.logger.Warn("WebSocket write error",
			slog.String("event", "ws_write_error"),
			slog.String("error", err.Error()),
		)
		return false
	}
	return true
}

// sendMessage serializes a thingMessage and queues it for the writePump.
// shadow_report and shadow_report_break_glass are the only Thing→Hub message
// types using this typed envelope; both wait up to 5s for queue space so a
// momentarily stalled writePump doesn't silently lose applied-config
// reports. On timeout we drop and increment outbox_dropped_total.
func (c *Client) sendMessage(msg thingMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		c.logger.Error("Failed to marshal outgoing message",
			slog.String("event", "marshal_error"),
			slog.String("msg_type", msg.Type),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("marshal outgoing message: %w", err)
	}
	return c.sendBytes(data, msg.Type)
}

// sendBytes queues a pre-serialized JSON payload for the writePump. msgType
// is used to route the message to the control vs metrics outbound channel
// (so a heartbeat-tick flood cannot starve a shadow_report) and as the
// outbox_dropped_total label (must match the JSON object's "type" field).
// Used by sendMessage and by the opsmetrics publishers (PushMetricsSample
// / PushDiagEvent) which emit flat envelopes that don't fit the
// thingMessage shape.
// routeOutboundChannel maps a Thing→Hub message type onto the right
// outbound queue. Routing is intentionally deny-list-like — anything not
// in the metrics set lands on outChControl. That preserves "must-deliver"
// semantics for new message types we add in the future without forcing a
// caller to remember the priority bucket.
func (c *Client) routeOutboundChannel(msgType string) chan []byte {
	switch msgType {
	case msgTypeMetricsSample, msgTypeDiagEvent:
		return c.outChMetrics
	default:
		return c.outChControl
	}
}

func (c *Client) sendBytes(data []byte, msgType string) error {
	ch := c.routeOutboundChannel(msgType)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case ch <- data:
		return nil
	case <-ctx.Done():
		c.logger.Warn("outgoing message blocked >5s; dropping",
			slog.String("event", "outbox_blocked"),
			slog.String("msg_type", msgType))
		c.promMetrics.outboxDropped.WithLabelValues(msgType).Inc()
		return fmt.Errorf("outgoing message queue stalled for msg_type=%s", msgType)
	}
}

// calculateBackoff returns the next backoff duration using exponential
// backoff with jitter, based on the current consecutive-failure counter.
// Sequence: 1s, 2s, 4s, 8s, 16s, 30s (cap).
func (c *Client) calculateBackoff() time.Duration {
	return c.calculateBackoffFor(int(c.wsConsecutiveFailures.Load()))
}

// calculateBackoffFor returns the exponential-with-jitter backoff for a given
// failure count. Exported to callers that track their own retry counter (e.g.
// the HTTP fallback loop's WS recovery retries) so they can use the same
// backoff curve without mutating wsConsecutiveFailures.
// Sequence: 1s, 2s, 4s, 8s, 16s, 30s (cap) at defaults.
func (c *Client) calculateBackoffFor(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}

	backoff := float64(c.cfg.ReconnectInitialBackoff) * math.Pow(2, float64(failures-1))
	maxBackoff := float64(c.cfg.ReconnectMaxBackoff)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}

	jitter := backoff * 0.25 * rand.Float64()
	return time.Duration(backoff + jitter)
}

// setMode updates the current operating mode and the Prometheus gauge.
func (c *Client) setMode(m Mode) {
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()

	for _, label := range []string{"ws_connected", "http_fallback", "disconnected", "ws_connecting"} {
		c.promMetrics.mode.WithLabelValues(label).Set(0)
	}
	c.promMetrics.mode.WithLabelValues(m.String()).Set(1)
}

// Mode returns the current operating mode.
func (c *Client) Mode() Mode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mode
}

// OnDisconnect registers a callback invoked when the WebSocket connection is lost.
// Convention is "call before Start()", but we still take c.mu so a runtime swap
// (hot-reload tests, runtime introspection) is race-free.
func (c *Client) OnDisconnect(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDisconnect = fn
}

// OnReconnect registers a callback invoked every time the WebSocket
// connection is established, including the initial connect. Callers use
// this to push static_info, drain reconnect buffers, and replay alert
// envelopes — all of which need fresh-connect coverage as well as
// post-disconnect coverage. Convention is "call before Start()"; we take
// c.mu so a runtime swap is race-free.
func (c *Client) OnReconnect(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onReconnect = fn
}

// OnHeartbeatTick registers a callback invoked after every successful
// heartbeat tick (metrics_sample push completing without error in either
// WS or HTTP-fallback mode). Use this to refresh status fields like
// "lastHeartbeat" that should reflect live liveness rather than the
// most recent WS reconnect. Convention is "call before Start()"; we
// take c.mu so a runtime swap is race-free.
func (c *Client) OnHeartbeatTick(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onHeartbeatTick = fn
}

// Close gracefully shuts down the Thing Client.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	cancel := c.cancel
	conn := c.wsConn
	mode := c.mode
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if conn != nil && mode == ModeWSConnected {
		closeCtx, closeCancel := context.WithTimeout(ctx, 2*time.Second)
		_ = conn.Close(websocket.StatusNormalClosure, "shutting down")
		closeCancel()
		_ = closeCtx
	}

	if mode == ModeHTTPFallback {
		c.httpDeregister(ctx)
	}

	if c.mqBuffer != nil {
		c.flushMQBuffer(ctx)
	}

	select {
	case <-c.done:
	case <-ctx.Done():
		return fmt.Errorf("thingclient: shutdown timed out")
	}

	c.setMode(ModeDisconnected)
	c.logger.Info("Thing Client shut down",
		slog.String("event", "shutdown"),
	)
	return nil
}
