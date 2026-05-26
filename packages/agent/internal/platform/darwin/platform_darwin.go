//go:build darwin

package darwin

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/bridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/bundles"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/flow"
	nepkg "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/ne"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/proc"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// DarwinPlatform implements api.Platform for macOS via Network Extension IPC.
// The Swift NE intercepts flows and sends metadata over a Unix socket. This
// Go side evaluates policy and returns decisions.
type DarwinPlatform struct {
	handler  api.ConnectionHandler
	listener net.Listener
	wg       sync.WaitGroup
	done     chan struct{}
	stopOnce sync.Once

	// MITM bridge: present when SetMITMDeps + StartBridge wire up the
	// loopback listener that runs proxy.MITMRelay against Swift NE
	// redirected inspect flows. Both nil = bridge disabled.
	tlsEngine   *agentTLS.Engine
	relayClient *relay.Client

	// bridgeDeps: when non-nil, handleBridgeFlow routes inspect flows
	// through shared/tlsbump.BumpConnection via proxy.BumpFlow. When nil
	// the legacy proxy.MITMRelay path runs. Set via SetBridgeDeps from
	// main.go after thingclient settles its initial config pull so the
	// resolver / domain snapshot / adapter registry are populated.
	bridgeDeps *proxy.BridgeDeps

	// backpressure: when non-nil + IsThrottled() returns true,
	// handleNewFlow short-circuits to passthrough so the agent sheds
	// load while the audit upload pipeline catches up. Wired from a
	// backpressure.Store fed by a goroutine polling
	// audit.Queue.UnsyncedCount() every 2 s. Pure atomic-load on the
	// hot path; nil store = no throttling.
	backpressure interface{ IsThrottled() bool }

	// Track active flows for audit on close.
	mu          sync.RWMutex
	activeFlows map[string]*flow.State

	// InterceptionHealthReporter state. All written from accept /
	// flow-handler goroutines; read concurrently by statusapi via
	// InterceptionHealth(). Use atomics so we don't take p.mu on the
	// read path.
	startedAtNS      atomic.Int64 // unix nanos; zero before Start
	connectionsTotal atomic.Int64 // cumulative NE attaches
	activeSessions   atomic.Int32 // currently-attached NE conns
	lastFlowAtNS     atomic.Int64 // unix nanos of last flow_new
}

// NewPlatform creates a new Darwin platform shim. relayClient is stored
// for use by the MITM bridge listener; when the bridge isn't configured
// the relayClient is dormant.
func NewPlatform(_ string, relayClient *relay.Client) api.Platform {
	return &DarwinPlatform{
		done:        make(chan struct{}),
		activeFlows: make(map[string]*flow.State),
		relayClient: relayClient,
	}
}

// InspectBundles returns the version stamps for the host app and system
// extension artifacts. Thin delegation to bundles.InspectBundles so
// platformshim callers can stay on the platform package import.
func InspectBundles() bundles.BundleVersions {
	return bundles.InspectBundles()
}

func (p *DarwinPlatform) Start(ctx context.Context, handler api.ConnectionHandler) error {
	p.handler = handler

	// Log a full version inventory of every macOS-side artifact before
	// we even open the socket. Mismatch between extension-on-disk and
	// extension-live is the smoking gun for "system extension never
	// reloaded after upgrade" — the class of bug caught by bundle version inventory.
	bv := bundles.InspectBundles()
	slog.Info("NE bundle inventory: host app",
		"path", bv.HostApp.Path,
		"short_version", bv.HostApp.CFBundleShortVersion,
		"build_number", bv.HostApp.CFBundleVersion,
		"mtime", bv.HostApp.Mtime,
		"note", bv.HostApp.Note,
	)
	slog.Info("NE bundle inventory: extension on disk",
		"path", bv.ExtensionDisk.Path,
		"short_version", bv.ExtensionDisk.CFBundleShortVersion,
		"build_number", bv.ExtensionDisk.CFBundleVersion,
		"mtime", bv.ExtensionDisk.Mtime,
		"note", bv.ExtensionDisk.Note,
	)
	slog.Info("NE bundle inventory: extension loaded by macOS",
		"path", bv.ExtensionLive.Path,
		"short_version", bv.ExtensionLive.CFBundleShortVersion,
		"build_number", bv.ExtensionLive.CFBundleVersion,
		"mtime", bv.ExtensionLive.Mtime,
		"note", bv.ExtensionLive.Note,
	)
	if bv.ExtensionDisk.CFBundleVersion != bv.ExtensionLive.CFBundleVersion &&
		bv.ExtensionDisk.CFBundleVersion != "<missing>" &&
		bv.ExtensionLive.CFBundleVersion != "<missing>" {
		slog.Warn("NE bundle inventory: VERSION MISMATCH — extension on disk differs from the copy macOS loaded; macOS likely skipped systemextensionsctl replacement (CFBundleVersion did not strictly increase). Provider process is running stale code.",
			"disk_build_number", bv.ExtensionDisk.CFBundleVersion,
			"live_build_number", bv.ExtensionLive.CFBundleVersion,
		)
	}

	socketPath := nepkg.SocketPath()
	socketDir := filepath.Dir(socketPath)
	slog.Info("NE platform starting",
		"socket_path", socketPath,
		"socket_dir", socketDir,
		"uid", os.Getuid(),
	)
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		slog.Error("NE create socket dir failed",
			"path", socketDir, "error", err,
		)
		return fmt.Errorf("create NE socket dir: %w", err)
	}

	if _, err := os.Stat(socketPath); err == nil {
		slog.Info("NE removing stale socket", "path", socketPath)
	}
	_ = os.Remove(socketPath) // remove stale socket

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socketPath)
	if err != nil {
		slog.Error("NE listen failed",
			"path", socketPath, "error", err,
		)
		return fmt.Errorf("listen NE socket %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = ln.Close()
		slog.Error("NE chmod socket failed",
			"path", socketPath, "error", err,
		)
		return fmt.Errorf("chmod NE socket: %w", err)
	}
	p.listener = ln
	p.startedAtNS.Store(time.Now().UnixNano())
	slog.Info("NE IPC listening — extension provider should now connect within ~1s of macOS spawning it",
		"path", socketPath, "perms", "0600",
	)

	go func() {
		<-ctx.Done()
		slog.Info("NE listener: ctx cancelled, closing socket", "path", socketPath)
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				slog.Info("NE listener: shutting down after ctx cancel; waiting for in-flight handlers")
				p.wg.Wait()
				slog.Info("NE listener: stopped cleanly")
				return nil
			default:
				slog.Error("NE accept failed (will retry)", "error", err)
				continue
			}
		}
		p.connectionsTotal.Add(1)
		p.activeSessions.Add(1)
		slog.Info("NE provider connected — extension process attached to daemon socket",
			"connections_total", p.connectionsTotal.Load(),
			"active_sessions", p.activeSessions.Load(),
			"remote", conn.RemoteAddr().String(),
		)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.activeSessions.Add(-1)
			p.handleNEConn(conn)
		}()
	}
}

// InterceptionHealth implements api.InterceptionHealthReporter.
// Returns a snapshot of the NE attach state — populated atomically so
// the read path does not contend with NE accept / flow handlers.
func (p *DarwinPlatform) InterceptionHealth() api.InterceptionHealth {
	h := api.InterceptionHealth{
		ConnectionsTotal: p.connectionsTotal.Load(),
		ActiveSessions:   int(p.activeSessions.Load()),
	}
	if ns := p.startedAtNS.Load(); ns > 0 {
		h.StartedAt = time.Unix(0, ns)
	}
	if ns := p.lastFlowAtNS.Load(); ns > 0 {
		h.LastFlowAt = time.Unix(0, ns)
	}
	h.Connected = h.ConnectionsTotal > 0
	return h
}

// InterceptionMode satisfies api.InterceptionModeReporter — macOS always uses
// the bundled NETransparentProxyProvider system extension.
func (p *DarwinPlatform) InterceptionMode() api.InterceptionMode {
	return api.ModeNETransparentProxy
}

func (p *DarwinPlatform) Stop() error {
	p.stopOnce.Do(func() {
		close(p.done)
	})
	if p.listener != nil {
		_ = p.listener.Close()
	}
	p.wg.Wait()
	_ = os.Remove(nepkg.SocketPath())
	return nil
}

func (p *DarwinPlatform) handleNEConn(conn net.Conn) {
	connStart := time.Now()
	var framesByType = map[string]int{}
	defer func() {
		_ = conn.Close()
		slog.Info("NE provider disconnected — extension closed IPC socket",
			"duration_s", int(time.Since(connStart).Seconds()),
			"frames_by_type", framesByType,
			"active_sessions_after", p.activeSessions.Load()-1,
		)
	}()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), nepkg.ScannerMaxBytes)

	for scanner.Scan() {
		select {
		case <-p.done:
			slog.Info("NE handleNEConn: stopping due to p.done")
			return
		default:
		}

		var msg nepkg.FlowMsg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			// Log a snippet of the offending input so we can pinpoint
			// the malformed frame later. Cap to keep logs sane.
			snippet := scanner.Bytes()
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			slog.Warn("NE IPC parse error",
				"error", err,
				"bytes", len(scanner.Bytes()),
				"snippet", string(snippet),
			)
			continue
		}

		framesByType[msg.Type]++
		switch msg.Type {
		case "flow_new":
			p.handleNewFlow(conn, msg)
		case "flow_closed":
			p.handleFlowClosed(msg)
		case "flow_inspect":
			p.handleFlowInspect(conn, msg)
		case "flow_inspect_response":
			p.handleFlowInspectResponse(conn, msg)
		case "flow_update_host":
			p.handleFlowUpdateHost(msg)
		default:
			slog.Warn("unknown NE message type",
				"type", msg.Type, "flow_id", msg.FlowID,
			)
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("NE IPC read error — connection terminating",
			"error", err,
			"duration_s", int(time.Since(connStart).Seconds()),
			"frames_by_type", framesByType,
		)
	}
}

// KillSwitchGater is implemented by handlers that can short-circuit
// flow tracking when protection is paused or the kill switch is
// engaged. Without this gate the daemon would still write audit
// rows for every flow during a user-paused session, which
// contradicts user expectation that "paused = invisible".
type KillSwitchGater interface {
	IsKillSwitchEngaged() bool
}

func (p *DarwinPlatform) handleNewFlow(conn net.Conn, msg nepkg.FlowMsg) {
	if p.handler == nil {
		slog.Warn("NE flow_new dropped — handler not yet wired",
			"flow_id", msg.FlowID, "remote", msg.RemoteHost,
		)
		return
	}

	p.lastFlowAtNS.Store(time.Now().UnixNano())

	// Pause / kill-switch gate: when protection is off we still need
	// to send a passthrough decision back to the provider (so the
	// flow runs natively), but we MUST NOT track the flow in
	// activeFlows or write an audit row. Otherwise paused state
	// silently fills the audit DB and leaks user activity that the
	// user explicitly opted out of recording.
	if g, ok := p.handler.(KillSwitchGater); ok && g.IsKillSwitchEngaged() {
		resp := nepkg.DecisionMsg{FlowID: msg.FlowID, Decision: "passthrough"}
		data, _ := json.Marshal(resp)
		data = append(data, '\n')
		if _, err := conn.Write(data); err != nil {
			slog.Warn("NE IPC write failed (paused passthrough)",
				"flow_id", msg.FlowID, "error", err,
			)
		}
		return
	}

	// Backpressure gate: when the local audit queue is over-full, shed
	// load by passing flows through unmodified — same IPC reply as
	// kill-switch but for a different reason. Same "do not write to
	// activeFlows" rationale: an audit row for a flow we deliberately
	// skipped would just deepen the backlog. Logged separately so the
	// spike is visible in agent.log.
	if p.backpressure != nil && p.backpressure.IsThrottled() {
		slog.Info("NE flow_new shed via backpressure (audit queue over high-water)",
			"flow_id", msg.FlowID, "remote", msg.RemoteHost,
		)
		resp := nepkg.DecisionMsg{FlowID: msg.FlowID, Decision: "passthrough"}
		data, _ := json.Marshal(resp)
		data = append(data, '\n')
		if _, err := conn.Write(data); err != nil {
			slog.Warn("NE IPC write failed (backpressure passthrough)",
				"flow_id", msg.FlowID, "error", err,
			)
		}
		return
	}

	var procMeta api.ProcessMeta
	var procErr error
	if msg.PID > 0 {
		m, err := proc.ProcessInfo(msg.PID)
		procErr = err
		if err == nil {
			procMeta = api.ProcessMeta{
				PID:      m.PID,
				Path:     m.Path,
				Name:     m.Name,
				BundleID: m.BundleID,
				User:     m.User,
			}
		}
	}
	if procErr != nil {
		slog.Debug("NE flow_new: ProcessInfo failed (non-fatal)",
			"flow_id", msg.FlowID, "pid", msg.PID, "error", procErr,
		)
	}

	intercepted := api.InterceptedConn{
		FlowID:  msg.FlowID,
		DstHost: msg.RemoteHost,
		DstIP:   msg.RemoteIP,
		DstPort: msg.RemotePort,
		SrcPort: msg.LocalPort,
		Process: procMeta,
	}

	decision := p.handler.HandleConnection(intercepted)

	// Track for audit on flow_closed
	p.mu.Lock()
	fs := &flow.State{
		FlowID:      msg.FlowID,
		DstHost:     intercepted.DstHost,
		DstIP:       intercepted.DstIP,
		DstPort:     intercepted.DstPort,
		SrcIP:       intercepted.SrcIP,
		SrcPort:     intercepted.SrcPort,
		ProcPID:     procMeta.PID,
		ProcPath:    procMeta.Path,
		ProcName:    procMeta.Name,
		ProcBundleID: procMeta.BundleID,
		ProcUser:    procMeta.User,
		DecisionInt: int(decision),
		StartedAt:   time.Now(),
	}
	p.activeFlows[msg.FlowID] = fs
	activeCount := len(p.activeFlows)
	p.mu.Unlock()

	decStr := "passthrough"
	switch decision {
	case api.DecisionInspect:
		decStr = "inspect"
	case api.DecisionDeny:
		decStr = "deny"
	}

	// INFO so per-flow decisions are visible without raising the global
	// log level. Without this, the daemon log shows "connection decision"
	// from the wiring layer but skips the IPC ingress side — RCA for
	// "extension claimed flow X, what did daemon do with it?" requires
	// matching flow_id across both lines.
	slog.Info("NE flow_new",
		"flow_id", msg.FlowID,
		"remote_host", msg.RemoteHost,
		"remote_ip", msg.RemoteIP,
		"remote_port", msg.RemotePort,
		"local_port", msg.LocalPort,
		"pid", msg.PID,
		"proc_name", procMeta.Name,
		"proc_bundle", procMeta.BundleID,
		"decision", decStr,
		"active_flows_after", activeCount,
	)

	resp := nepkg.DecisionMsg{FlowID: msg.FlowID, Decision: decStr}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		slog.Warn("NE IPC write failed (decision response)",
			"flow_id", msg.FlowID,
			"decision", decStr,
			"error", err,
		)
	}
}

// handleFlowUpdateHost rewrites the destination hostname of an
// in-flight flow when the Swift extension has just extracted a
// real hostname from the TLS ClientHello SNI. This recovers the
// hostname for browsers / Electron apps / DoH clients that
// pre-resolve DNS and leave NEAppProxyFlow.remoteHostname nil.
// No-op when the flow is unknown (race with flow_closed) or the
// hostname is empty.
func (p *DarwinPlatform) handleFlowUpdateHost(msg nepkg.FlowMsg) {
	if msg.Hostname == "" {
		return
	}
	p.mu.Lock()
	fs, ok := p.activeFlows[msg.FlowID]
	if ok {
		fs.DstHost = msg.Hostname
	}
	p.mu.Unlock()
	if ok {
		slog.Debug("NE flow_update_host applied",
			"flow_id", msg.FlowID, "hostname", msg.Hostname,
		)
	}
}

// Inspect flows write per-HTTP-request rows directly from
// tlsbump.AuditEmitter into the agent's SQLite Queue; no flow-level
// summary row is needed and handleFlowClosed has an early-return
// for inspect decisions.

// LookupFlowDestination returns the destination host:port the daemon
// recorded for flowID at flow_new time, plus the source process
// metadata. Bridge handlers call this to recover the real upstream
// destination because the BRIDGE header host can be IP-literal when
// the calling app pre-resolved DNS — the daemon may already have
// the SNI-derived hostname from a flow_update_host IPC. ok=false
// when flowID is unknown (race / synthetic).
func (p *DarwinPlatform) LookupFlowDestination(flowID string) (host string, port int, srcIP string, procMeta api.ProcessMeta, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	fs, exists := p.activeFlows[flowID]
	if !exists {
		return "", 0, "", api.ProcessMeta{}, false
	}
	pm := api.ProcessMeta{
		PID:      fs.ProcPID,
		Path:     fs.ProcPath,
		Name:     fs.ProcName,
		BundleID: fs.ProcBundleID,
		User:     fs.ProcUser,
	}
	return fs.DstHost, fs.DstPort, fs.SrcIP, pm, true
}

// StartBridge starts the macOS NE → Go MITM bridge listener on addr
// (typically 127.0.0.1:9443). The listener accepts Swift NE redirected
// connections and runs proxy.MITMRelay. Returns the listener so the
// caller can close it on shutdown.
//
// Requires:
//   - p.handler (the connectionBridge) must implement RequestInspector,
//     ResponseInspector, ResponseUsageDetector, BodyReadCapper.
//   - SetMITMDeps must have been called with non-nil tlsEngine and
//     relayClient before this method is invoked.
//
// Returns nil + nil error when the bridge is not configured (addr == "")
// — caller should treat that as "feature disabled" and continue.
func (p *DarwinPlatform) StartBridge(ctx context.Context, addr string) (io.Closer, error) {
	if addr == "" {
		return nil, nil
	}
	if p.handler == nil {
		return nil, fmt.Errorf("StartBridge: ConnectionHandler not set — call Start() first")
	}
	if p.tlsEngine == nil || p.relayClient == nil {
		return nil, fmt.Errorf("StartBridge: SetMITMDeps must supply tlsEngine and relayClient before bridge can run")
	}
	ln, err := bridge.New(bridge.Config{
		Addr:   addr,
		Logger: slog.Default(),
		Handle: p.handleBridgeFlow,
	})
	if err != nil {
		return nil, fmt.Errorf("StartBridge: %w", err)
	}
	go ln.Run(ctx)
	slog.Info("bridge listener attached to MITMRelay pipeline", "addr", ln.Addr())
	return ln, nil
}

// SetMITMDeps wires the TLS engine + outbound relay client the bridge
// listener will use to MITM-bump inspect flows. Called from main.go
// before StartBridge. Both fields are non-nil required; passing nil
// disables the bridge gracefully.
func (p *DarwinPlatform) SetMITMDeps(tlsEngine *agentTLS.Engine, relayClient *relay.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tlsEngine = tlsEngine
	p.relayClient = relayClient
}

// SetBackpressure wires the audit-queue backpressure store. When the
// store reports IsThrottled() == true (queue depth >= HighWatermark),
// handleNewFlow short-circuits incoming flows to passthrough so the
// agent sheds load until the audit upload pipeline drains the queue
// below LowWatermark. Pass nil to disable.
func (p *DarwinPlatform) SetBackpressure(bp interface{ IsThrottled() bool }) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backpressure = bp
}

// TLSEngine returns the device-CA TLS engine wired by SetMITMDeps /
// LoadTLSEngineFromDisk. Nil before either has run; main.go reads this
// to populate BridgeDeps.TLSEngine after the disk-load step.
func (p *DarwinPlatform) TLSEngine() *agentTLS.Engine {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tlsEngine
}

// SetBridgeDeps wires the agent's BridgeDeps so handleBridgeFlow can
// route inspect flows through shared/tlsbump.BumpConnection via
// proxy.BumpFlow. When non-nil, the bridge handler prefers BumpFlow
// over the legacy proxy.MITMRelay path. Setting nil reverts to the
// legacy path.
func (p *DarwinPlatform) SetBridgeDeps(deps *proxy.BridgeDeps) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bridgeDeps = deps
}

// LoadTLSEngineFromDisk reads the agent's device CA from the standard
// CA file paths and constructs the proxy.MITMRelay engine in one shot.
// Mirrors linux.go's NewEngine call. caCertPath / caKeyPath usually
// come from platform.DefaultPaths(). Idempotent — replaces any prior
// engine. relayClient must already be set by NewPlatform; this is a
// helper that takes the boilerplate out of main.go.
func (p *DarwinPlatform) LoadTLSEngineFromDisk(caCertPath, caKeyPath string) error {
	caCert, caKey, generated, err := agentTLS.LoadOrGenerateCA(caCertPath, caKeyPath)
	if err != nil {
		return fmt.Errorf("LoadTLSEngineFromDisk: load CA from %s/%s: %w", caCertPath, caKeyPath, err)
	}
	engine, err := agentTLS.NewEngine(caCert, caKey, 2000, time.Hour)
	if err != nil {
		return fmt.Errorf("LoadTLSEngineFromDisk: new engine: %w", err)
	}
	p.mu.Lock()
	p.tlsEngine = engine
	p.mu.Unlock()
	slog.Info("TLS engine ready", "ca_cert", caCertPath, "ca_generated_now", generated)
	return nil
}

// handleBridgeFlow is the bridge.HandleFunc closure registered in
// StartBridge. Mirrors the linux.go inspect path exactly so macOS
// gains TLS-bump parity: build inspector callbacks from p.handler,
// run proxy.MITMRelay, stamp the result back onto the per-flow
// state so handleFlowClosed picks it up when Swift sends flow_closed.
func (p *DarwinPlatform) handleBridgeFlow(ctx context.Context, clientConn net.Conn, peeked []byte, dstHost string, dstPort int, flowID string) {
	defer clientConn.Close() //nolint:errcheck
	bridgeStart := time.Now()
	defer func() {
		slog.Info("bridge: flow handler returned",
			"flow_id", flowID, "elapsed_ms", time.Since(bridgeStart).Milliseconds())
	}()
	slog.Info("bridge: flow received",
		"flow_id", flowID, "dst_host_from_header", dstHost, "dst_port", dstPort, "peeked_bytes", len(peeked))
	// Recover the daemon-side destination — Swift may have sent the
	// IP literal in the BRIDGE header (caller pre-resolved DNS) and
	// the SNI-derived hostname is on flowState by now. Use it for
	// MITMRelay's leaf-cert SAN matching.
	originalHost := dstHost
	if recordedHost, _, _, _, ok := p.LookupFlowDestination(flowID); ok && recordedHost != "" && !looksLikeIPLiteral(recordedHost) {
		dstHost = recordedHost
		if originalHost != recordedHost {
			slog.Info("bridge: dst_host overridden by daemon-recorded SNI",
				"flow_id", flowID, "header_host", originalHost, "sni_host", recordedHost)
		}
	}

	// Build the inspector callbacks the same way linux.go does. The
	// connectionBridge in main.go implements all three interfaces, so
	// these type assertions succeed in production. Method/path are
	// captured into outer-scope vars because proxy.InspectionResult
	// doesn't surface them — they're inputs to the inspector — and
	// the audit row needs them.
	var capturedMethod, capturedPath string
	var inspector proxy.RequestInspector
	if ri, ok := p.handler.(api.RequestInspector); ok {
		inspector = func(ctx context.Context, host, method, path string, headers http.Header, body []byte) proxy.InspectionResult {
			capturedMethod, capturedPath = method, path
			r := ri.InspectRequest(ctx, host, method, path, headers, body)
			return proxy.InspectionResult{
				Decision:          r.Decision,
				Reason:            r.Reason,
				ReasonCode:        r.ReasonCode,
				ComplianceTags:    r.ComplianceTags,
				HookOutcome:       r.HookOutcome,
				Provider:          r.Provider,
				Model:             r.Model,
				ApiKeyClass:       r.ApiKeyClass,
				ApiKeyFingerprint: r.ApiKeyFingerprint,
				PayloadRequest:    r.PayloadRequest,
				PayloadResponse:   r.PayloadResponse,
			}
		}
	}
	var respInspector proxy.ResponseInspector
	if ri, ok := p.handler.(api.ResponseInspector); ok {
		respInspector = func(ctx context.Context, host, method, path string, body []byte, usage *traffic.UsageMeta) proxy.InspectionResult {
			r := ri.InspectResponse(ctx, host, method, path, body, usage)
			return proxy.InspectionResult{
				Decision:              r.Decision,
				Reason:                r.Reason,
				ReasonCode:            r.ReasonCode,
				ComplianceTags:        r.ComplianceTags,
				HookOutcome:           r.HookOutcome,
				PromptTokens:          r.PromptTokens,
				CompletionTokens:      r.CompletionTokens,
				UsageExtractionStatus: r.UsageExtractionStatus,
				PayloadRequest:        r.PayloadRequest,
				PayloadResponse:       r.PayloadResponse,
			}
		}
	}
	var usageDetector proxy.ResponseUsageDetector
	if d, ok := p.handler.(api.ResponseUsageDetector); ok {
		usageDetector = d
	}
	var maxBodyBytes int64
	if capper, ok := p.handler.(api.BodyReadCapper); ok {
		maxBodyBytes = capper.ReadBodyCap()
	}

	// handleBridgeFlow is a thin dispatcher into proxy.BumpFlow.
	// Per-HTTP-request audit rows are emitted from inside
	// tlsbump.AuditEmitter directly into the agent's SQLite queue;
	// no flow-level summary is computed here.
	// handleFlowClosed will NOT write a separate row for this
	// inspect flow — see the early-return there.
	phaseSink := traffic.NewPhaseSink()
	ctx = traffic.WithPhaseSink(ctx, phaseSink)
	_, _, _ = inspector, respInspector, usageDetector
	_ = maxBodyBytes
	_ = capturedMethod
	_ = capturedPath

	p.mu.RLock()
	deps := p.bridgeDeps
	var fp proxy.FlowProcess
	if fs, ok := p.activeFlows[flowID]; ok {
		fp = proxy.FlowProcess{
			Name:   fs.ProcName,
			Bundle: fs.ProcBundleID,
			User:   fs.ProcUser,
		}
	}
	p.mu.RUnlock()

	if deps == nil {
		slog.Warn("bridge: deps not wired — flow dropped",
			"flow_id", flowID, "host", dstHost, "port", dstPort)
		return
	}
	// Pre-flight log so agent.log can be grepped for flow lifecycle
	// even when BumpFlow takes seconds (SSE) or fails silently.
	slog.Info("bridge: BumpFlow start",
		"flow_id", flowID,
		"host", dstHost,
		"port", dstPort,
		"src_proc_name", fp.Name,
		"src_proc_bundle", fp.Bundle,
		"src_proc_user", fp.User,
		"peeked_bytes", len(peeked))
	bumpStart := time.Now()
	if err := proxy.BumpFlow(ctx, clientConn, peeked, dstHost, dstPort, flowID, fp, *deps); err != nil {
		// Errors are mostly benign (certificate pin checks rejecting
		// our minted leaf). INFO so they surface without debug logging.
		slog.Info("bridge: BumpFlow returned error",
			"flow_id", flowID, "host", dstHost, "port", dstPort,
			"elapsed_ms", time.Since(bumpStart).Milliseconds(),
			"error", err)
		return
	}
	slog.Info("bridge: BumpFlow completed",
		"flow_id", flowID, "host", dstHost, "port", dstPort,
		"elapsed_ms", time.Since(bumpStart).Milliseconds())
}

// looksLikeIPLiteral matches the Swift-side isLikelyIPLiteral helper.
// Used by handleBridgeFlow to decide whether the BRIDGE header host
// should be replaced with the daemon's recorded SNI-derived hostname.
func looksLikeIPLiteral(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == ':' {
			return true // IPv6 literal
		}
	}
	hasDot := false
	for _, r := range s {
		if r == '.' {
			hasDot = true
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return hasDot
}

func (p *DarwinPlatform) handleFlowClosed(msg nepkg.FlowMsg) {
	p.mu.Lock()
	fs, ok := p.activeFlows[msg.FlowID]
	delete(p.activeFlows, msg.FlowID)
	remaining := len(p.activeFlows)
	p.mu.Unlock()

	if !ok {
		slog.Warn("NE flow_closed for unknown flow — possible double-close or out-of-order frame",
			"flow_id", msg.FlowID,
			"bytes_in", msg.BytesIn,
			"bytes_out", msg.BytesOut,
		)
		return
	}

	slog.Debug("NE flow_closed",
		"flow_id", msg.FlowID,
		"remote_host", fs.DstHost,
		"decision", fs.DecisionInt,
		"bytes_in", msg.BytesIn,
		"bytes_out", msg.BytesOut,
		"duration_ms", msg.DurationMs,
		"active_flows_remaining", remaining,
	)

	// Inspect flows have already been recorded as N per-HTTP-request
	// rows by tlsbump.AuditEmitter inside BumpFlow. DO NOT write an
	// additional flow-level row here. The transport-level metrics
	// (bytes/duration) from Swift's flow_closed message are not
	// currently merged into the per-request rows.
	if api.Decision(fs.DecisionInt) == api.DecisionInspect {
		return
	}

	// Passthrough/deny flows DID NOT run through the bridge → no
	// per-request rows exist. Write one transport-level row here so
	// the agent UI can show "this flow was decided X without
	// inspection". Fields are limited to what Swift's flow_closed
	// surfaces (no method/path/hookDecision — Swift can't see
	// decrypted HTTP for non-bumped flows).
	if auditor, ok := p.handler.(api.FlowAuditor); ok {
		var breakdown map[string]int
		if msg.InterceptMs != nil && *msg.InterceptMs > 0 {
			breakdown = map[string]int{"intercept_ms": *msg.InterceptMs}
		}
		pm := api.ProcessMeta{
			PID:      fs.ProcPID,
			Path:     fs.ProcPath,
			Name:     fs.ProcName,
			BundleID: fs.ProcBundleID,
			User:     fs.ProcUser,
		}
		auditor.OnFlowComplete(api.FlowResult{
			FlowID:           msg.FlowID,
			SrcIP:            fs.SrcIP,
			DstHost:          fs.DstHost,
			DstIP:            fs.DstIP,
			DstPort:          fs.DstPort,
			Process:          pm,
			Decision:         api.Decision(fs.DecisionInt),
			BytesIn:          msg.BytesIn,
			BytesOut:         msg.BytesOut,
			DurationMs:       msg.DurationMs,
			BumpStatus:       msg.BumpStatus,
			StartedAt:        fs.StartedAt,
			UpstreamTtfbMs:   msg.UpstreamTtfbMs,
			UpstreamTotalMs:  msg.UpstreamTotalMs,
			LatencyBreakdown: breakdown,
		})
	}
}

func (p *DarwinPlatform) handleFlowInspect(conn net.Conn, msg nepkg.FlowMsg) {
	result := api.InspectionResult{Decision: "approve"}

	if inspector, ok := p.handler.(api.RequestInspector); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		body, _ := base64.StdEncoding.DecodeString(msg.Body)
		headers := http.Header(msg.Headers)
		result = inspector.InspectRequest(ctx, msg.RemoteHost, msg.Method, msg.Path, headers, body)
	}

	// Stash detected signals on the flow state so handleFlowClosed can
	// stamp them onto the outgoing audit event. No-op when the flow is
	// unknown (race with flow_closed or a synthetic inspect).
	p.mu.Lock()
	if fs, ok := p.activeFlows[msg.FlowID]; ok {
		fs.Provider = result.Provider
		fs.Model = result.Model
		fs.ApiKeyClass = result.ApiKeyClass
		fs.ApiKeyFingerprint = result.ApiKeyFingerprint
		fs.PayloadRequest = result.PayloadRequest
		// Capture method + path from the inspect message so the audit
		// row carries them even when Swift's flow_closed message
		// doesn't (it currently only carries flowId / bytes /
		// duration / phase metrics, not HTTP-level fields).
		if msg.Method != "" {
			fs.Method = msg.Method
		}
		if msg.Path != "" {
			fs.Path = msg.Path
		}
	}
	p.mu.Unlock()

	resp := nepkg.InspectResult{
		FlowID:         msg.FlowID,
		Decision:       result.Decision,
		Reason:         result.Reason,
		ReasonCode:     result.ReasonCode,
		ComplianceTags: result.ComplianceTags,
	}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	_, _ = conn.Write(data) // best-effort: GUI client may have detached
}

func (p *DarwinPlatform) handleFlowInspectResponse(conn net.Conn, msg nepkg.FlowMsg) {
	result := api.InspectionResult{Decision: "approve"}

	if inspector, ok := p.handler.(api.ResponseInspector); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		body, _ := base64.StdEncoding.DecodeString(msg.Body)
		// On darwin the Network Extension delivers the decoded body after its
		// own buffering — there is no streaming accumulator on the Go side yet.
		// Pass nil for usage; the ConnectionHandler's InspectResponse is
		// expected to compute usage locally via ExtractResponseUsage when it
		// needs to.
		result = inspector.InspectResponse(ctx, msg.RemoteHost, msg.Method, msg.Path, body, nil)
	}

	// Stash response-side usage on the flow state so handleFlowClosed
	// stamps it onto the outgoing audit event. No-op when the flow is
	// unknown (race with flow_closed).
	p.mu.Lock()
	if fs, ok := p.activeFlows[msg.FlowID]; ok {
		fs.PromptTokens = result.PromptTokens
		fs.CompletionTokens = result.CompletionTokens
		fs.UsageExtractionStatus = result.UsageExtractionStatus
		fs.PayloadResponse = result.PayloadResponse
	}
	p.mu.Unlock()

	resp := nepkg.InspectResult{
		FlowID:         msg.FlowID,
		Decision:       result.Decision,
		Reason:         result.Reason,
		ReasonCode:     result.ReasonCode,
		ComplianceTags: result.ComplianceTags,
	}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	_, _ = conn.Write(data) // best-effort: GUI client may have detached
}

// ProcessInfo resolves process metadata for a given PID using macOS libproc APIs.
func (p *DarwinPlatform) ProcessInfo(pid int) (api.ProcessMeta, error) {
	m, err := proc.ProcessInfo(pid)
	if err != nil {
		return api.ProcessMeta{}, err
	}
	return api.ProcessMeta{
		PID:      m.PID,
		Path:     m.Path,
		Name:     m.Name,
		BundleID: m.BundleID,
		User:     m.User,
	}, nil
}

