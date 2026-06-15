//go:build darwin

package darwin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/bridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/bundles"
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

	// tlsEngine is the device-CA TLS engine the NE bridge uses to mint
	// per-host leaf certs for inspect flows. Wired by LoadTLSEngineFromDisk
	// before StartBridge; nil = bridge disabled.
	tlsEngine *agentTLS.Engine

	// bridgeDeps: when non-nil, handleBridgeFlow routes inspect flows
	// through shared/tlsbump.BumpConnection via proxy.BumpFlow. Set via
	// SetBridgeDeps from main.go after thingclient settles its initial
	// config pull so the resolver / domain snapshot / adapter registry
	// are populated. Nil leaves inspect flows on the Swift raw-relay path.
	bridgeDeps *proxy.BridgeDeps

	// backpressure: when non-nil + IsThrottled() returns true,
	// handleNewFlow short-circuits to passthrough so the agent sheds
	// load while the audit upload pipeline catches up. Wired from a
	// backpressure.Store fed by a goroutine polling
	// audit.Queue.UnsyncedCount() every 2 s. Set ONCE at boot
	// (SetBackpressure, before Start), then only read on the hot path —
	// the read is intentionally unsynchronized because the write strictly
	// precedes any flow. nil store = no throttling.
	backpressure interface{ IsThrottled() bool }

	// mu guards only the low-frequency config fields below
	// (backpressure / tlsEngine / bridgeDeps), which are set once at
	// boot and read off the per-flow hot path. It deliberately does NOT
	// guard activeFlows — those live in a sync.Map so concurrent flow
	// decisions never serialize on a shared lock.
	mu sync.RWMutex

	// activeFlows is keyed by flowID (globally-unique UUIDs), so every
	// flow touches a disjoint key — sync.Map is lock-free for this
	// access pattern. Values are *flow.State; the worker publishes its
	// computed fields via flow.State.Ready (see the State doc).
	activeFlows    sync.Map
	activeFlowsLen atomic.Int64 // maintained on Store/Delete for logging/health

	// flowSem bounds how many flow decisions compute concurrently. The
	// IPC reader registers each flow synchronously then hands the slow
	// part (process lookup + policy + connection-stage hooks) to a
	// worker that acquires a slot here, so a slow hook on one flow no
	// longer stalls every other flow behind the single reader goroutine.
	flowSem chan struct{}

	// InterceptionHealthReporter state. All written from accept /
	// flow-handler goroutines; read concurrently by statusapi via
	// InterceptionHealth(). Use atomics so we don't take p.mu on the
	// read path.
	startedAtNS      atomic.Int64 // unix nanos; zero before Start
	connectionsTotal atomic.Int64 // cumulative NE attaches
	activeSessions   atomic.Int32 // currently-attached NE conns
	lastFlowAtNS     atomic.Int64 // unix nanos of last flow_new
}

// flowDecisionConcurrency caps concurrent flow-decision workers. Sized
// to absorb a browser's connection burst without unbounded goroutine
// growth; a saturated pool applies backpressure on the IPC reader, and
// any flow whose decision is delayed past the NE's 2 s timeout fails
// open to passthrough — never blocks.
const flowDecisionConcurrency = 128

// NewPlatform creates a new Darwin platform shim.
func NewPlatform(_ string) api.Platform {
	return &DarwinPlatform{
		done:    make(chan struct{}),
		flowSem: make(chan struct{}, flowDecisionConcurrency),
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

// neConn pairs the IPC connection with a single-writer response channel.
// Decision workers run concurrently but must not write to the socket
// concurrently (interleaved bytes corrupt the JSON-line framing), so all
// replies funnel through respCh and exactly one goroutine drains it to
// the conn — a lock-free single-writer instead of a write mutex.
type neConn struct {
	conn    net.Conn
	respCh  chan []byte
	workers sync.WaitGroup // in-flight decision workers for this conn
}

// reply queues a framed response for the single writer. Non-blocking
// intent: respCh is buffered; if a wedged writer ever fills it, the send
// blocks the worker (bounded by flowSem), which is acceptable backpressure
// and still fail-open (the NE times out to passthrough).
func (nc *neConn) reply(data []byte) {
	nc.respCh <- data
}

func (p *DarwinPlatform) handleNEConn(conn net.Conn) {
	connStart := time.Now()
	var framesByType = map[string]int{}

	nc := &neConn{conn: conn, respCh: make(chan []byte, 256)}
	// Single writer goroutine: serialises all socket writes without a
	// per-write lock. Closed after the reader loop ends and every
	// in-flight worker has drained.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for data := range nc.respCh {
			if _, err := conn.Write(data); err != nil {
				slog.Warn("NE IPC write failed", "bytes", len(data), "error", err)
			}
		}
	}()

	defer func() {
		// Drain in-flight decision workers so their replies are sent
		// before we close the writer; then close respCh to stop the
		// writer and wait for it.
		nc.workers.Wait()
		close(nc.respCh)
		<-writerDone
		_ = conn.Close()
		slog.Info("NE provider disconnected — extension closed IPC socket",
			"duration_s", int(time.Since(connStart).Seconds()),
			"frames_by_type", framesByType,
			"active_sessions_after", p.activeSessions.Load()-1,
		)
	}()
	// Liveness heartbeat: while this NE IPC connection is alive, emit a
	// periodic line so an operator tailing agent.log can confirm the
	// extension is still attached (and how many flows it carries) WITHOUT
	// waiting for the next flow. A gap in this line while traffic flows is
	// the visible signature of a daemon↔NE split — the zombie state the NE
	// side self-heals by declining flows. Reads only atomics + immutable
	// connStart, so it never races the reader loop's framesByType map.
	hbStop := make(chan struct{})
	defer close(hbStop)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-p.done:
				return
			case <-ticker.C:
				slog.Info("NE liveness heartbeat — extension attached to daemon",
					"active_sessions", p.activeSessions.Load(),
					"uptime_s", int(time.Since(connStart).Seconds()),
				)
			}
		}
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
			p.handleNewFlow(nc, msg)
		case "flow_closed":
			p.handleFlowClosed(msg)
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
	fs, exists := p.loadFlow(flowID)
	if !exists {
		return "", 0, "", api.ProcessMeta{}, false
	}
	// Process metadata is worker-written; only read it once the worker
	// has published via Ready. Host/port are reader-owned identity
	// fields, always safe. A not-yet-ready flow returns empty proc meta
	// (the bridge falls back to the header host) rather than racing.
	var pm api.ProcessMeta
	if fs.Ready.Load() {
		pm = api.ProcessMeta{
			PID:      fs.ProcPID,
			Path:     fs.ProcPath,
			Name:     fs.ProcName,
			BundleID: fs.ProcBundleID,
			User:     fs.ProcUser,
		}
	}
	return fs.DstHost(), fs.DstPort, fs.SrcIP, pm, true
}

// StartBridge starts the macOS NE → Go bridge listener on addr (typically
// 127.0.0.1:9443). The listener accepts Swift NE redirected inspect flows
// and hands each off to proxy.BumpFlow (shared/tlsbump). Returns the
// listener so the caller can close it on shutdown.
//
// Requires p.tlsEngine to be set (via LoadTLSEngineFromDisk) before this
// method is invoked.
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
	if p.tlsEngine == nil {
		return nil, fmt.Errorf("StartBridge: LoadTLSEngineFromDisk must supply tlsEngine before bridge can run")
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
	slog.Info("bridge listener attached to BumpFlow pipeline", "addr", ln.Addr())
	return ln, nil
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

// TLSEngine returns the device-CA TLS engine wired by
// LoadTLSEngineFromDisk. Nil before it has run; main.go reads this to
// populate BridgeDeps.TLSEngine after the disk-load step.
func (p *DarwinPlatform) TLSEngine() *agentTLS.Engine {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.tlsEngine
}

// SetBridgeDeps wires the agent's BridgeDeps so handleBridgeFlow can
// route inspect flows through shared/tlsbump.BumpConnection via
// proxy.BumpFlow. Setting nil leaves inspect flows on the Swift
// raw-relay path.
func (p *DarwinPlatform) SetBridgeDeps(deps *proxy.BridgeDeps) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bridgeDeps = deps
}

// LoadTLSEngineFromDisk reads the agent's device CA from the standard CA
// file paths and constructs the device-CA TLS engine in one shot. Mirrors
// linux.go's NewEngine call. caCertPath / caKeyPath usually come from
// platform.DefaultPaths(). Idempotent — replaces any prior engine.
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
// StartBridge. It is a thin dispatcher into proxy.BumpFlow: inspect flows
// terminate TLS and emit per-HTTP-request audit rows from inside tlsbump,
// so handleFlowClosed skips the flow-level row for them.
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
	// BumpFlow's leaf-cert SAN matching.
	originalHost := dstHost
	if recordedHost, _, _, _, ok := p.LookupFlowDestination(flowID); ok && recordedHost != "" && !looksLikeIPLiteral(recordedHost) {
		dstHost = recordedHost
		if originalHost != recordedHost {
			slog.Info("bridge: dst_host overridden by daemon-recorded SNI",
				"flow_id", flowID, "header_host", originalHost, "sni_host", recordedHost)
		}
	}

	// handleBridgeFlow is a thin dispatcher into proxy.BumpFlow.
	// Per-HTTP-request audit rows are emitted from inside
	// tlsbump.AuditEmitter directly into the agent's SQLite queue;
	// no flow-level summary is computed here. handleFlowClosed will
	// NOT write a separate row for this inspect flow — see the
	// early-return there.
	phaseSink := traffic.NewPhaseSink()
	ctx = traffic.WithPhaseSink(ctx, phaseSink)

	p.mu.RLock()
	deps := p.bridgeDeps
	p.mu.RUnlock()
	// activeFlows is a separate lock-free map; read the process fields
	// only once the decision worker has published them via Ready.
	var fp proxy.FlowProcess
	if fs, ok := p.loadFlow(flowID); ok && fs.Ready.Load() {
		fp = proxy.FlowProcess{
			Name:   fs.ProcName,
			Bundle: fs.ProcBundleID,
			User:   fs.ProcUser,
		}
	}

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
