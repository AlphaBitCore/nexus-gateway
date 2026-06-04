//go:build linux

// Package linux implements transparent proxy interception for Linux using iptables
// REDIRECT (set up by the installer) and SO_ORIGINAL_DST for original
// destination resolution. Process identification uses the /proc filesystem.
package linux

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

const (
	soOriginalDst  = 80 // linux/netfilter_ipv4.h SO_ORIGINAL_DST
	defaultLinAddr = "127.0.0.1:19080"
)

// LinuxPlatform implements api.Platform for Linux via iptables REDIRECT +
// transparent proxy + /proc process resolution.
const maxConcurrentConns = 512

type LinuxPlatform struct {
	handler        api.ConnectionHandler
	listener       net.Listener
	wg             sync.WaitGroup
	done           chan struct{}
	stopOnce       sync.Once
	sem            chan struct{} // bounds concurrent connection handlers
	tlsEngine      *agentTLS.Engine
	addr           string
	reconciler     *Reconciler
	upstreamDialer *net.Dialer

	// bridgeDeps routes inspect flows through shared/tlsbump.BumpConnection
	// (via proxy.BumpFlow) — the same engine macOS, the compliance proxy,
	// and the AI gateway use. Set once at boot via SetBridgeDeps before
	// Start launches the accept loop; read by the per-connection handlers.
	bridgeDeps *proxy.BridgeDeps
}

// SetBridgeDeps wires the shared/tlsbump bridge dependencies so the inspect
// path bumps each flow through proxy.BumpFlow. Satisfies
// api.BridgeDepsReceiver. Called once at boot before Start.
func (p *LinuxPlatform) SetBridgeDeps(deps *proxy.BridgeDeps) {
	p.bridgeDeps = deps
}

// InterceptionMode satisfies api.InterceptionModeReporter — Linux uses
// the iptables REDIRECT + SO_ORIGINAL_DST path the Reconciler keeps
// alive.
func (p *LinuxPlatform) InterceptionMode() api.InterceptionMode {
	return api.ModeIPTables
}

// NewPlatform creates a new Linux platform shim.
func NewPlatform(addr string) api.Platform {
	if addr == "" {
		addr = defaultLinAddr
	}
	return &LinuxPlatform{
		done:           make(chan struct{}),
		sem:            make(chan struct{}, maxConcurrentConns),
		addr:           addr,
		upstreamDialer: MarkedDialer(),
	}
}

func (p *LinuxPlatform) Start(ctx context.Context, handler api.ConnectionHandler) error {
	p.handler = handler

	// Install SO_MARK on every transport built via shared/httpclient
	// (hubhttp, relay, enrollment, updater, thingclient HTTP fallback)
	// and the proxy's MITM upstream dialer — they all consult
	// nexushttp.GlobalDialControl(). The Linux agent is the only
	// place this is set; macOS/Windows builds leave it nil.
	nexushttp.SetGlobalDialControl(markControl)

	// Initialise TLS engine for MITM inspection.
	//
	// Production model (deb / rpm install): postinstall.sh runs
	// `nexus-agent install-ca` as root, which generates the device
	// CA and persists it to /var/lib/nexus-agent/device-ca.{pem,key}
	// + installs the cert to the OS trust store via
	// update-ca-certificates. The runtime daemon then loads it from
	// disk here.
	//
	// Dev / unprivileged model: when the on-disk path is unreadable
	// or unwritable (e.g. an engineer running `go run` directly
	// without sudo, or a misconfigured upgrade where the postinstall
	// chown didn't fire), we fall back to an in-memory CA.
	// Intercepted TLS won't be trusted by host clients (no cert in
	// OS trust store), but the daemon still functions for development
	// workflows.
	caCertPath := filepath.Join(paths.DefaultPaths().StateDir, "device-ca.pem")
	caKeyPath := filepath.Join(paths.DefaultPaths().StateDir, "device-ca.key")
	caCert, caKey, generated, caErr := agentTLS.LoadOrGenerateCA(caCertPath, caKeyPath)
	var err error
	switch caErr {
	case nil:
		if generated {
			slog.Warn("device CA was regenerated at daemon runtime",
				"hint", "the postinstall script's `nexus-agent install-ca` step did not run as root; intercepted TLS may not be trusted by host clients until `nexus-agent install-ca` is invoked as root",
				"cert_path", caCertPath)
		}
		p.tlsEngine, err = agentTLS.NewEngine(caCert, caKey, 2000, time.Hour)
	default:
		slog.Warn("device CA disk persistence unavailable; using ephemeral CA",
			"cert_path", caCertPath, "error", caErr,
			"hint", "production deb/rpm installs run install-ca as root via postinstall.sh; dev workflows should `sudo mkdir -p /var/lib/nexus-agent && sudo chown $USER /var/lib/nexus-agent` or run the agent as root")
		// Pass nil for cert+key so NewEngine generates a fresh
		// in-memory CA; intercepted TLS will require the dev to
		// manually trust the cert exposed via the Dashboard's
		// Diagnostics page.
		p.tlsEngine, err = agentTLS.NewEngine(nil, nil, 2000, time.Hour)
	}
	if err != nil {
		return fmt.Errorf("init TLS engine: %w", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", p.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.addr, err)
	}
	p.listener = ln
	slog.Info("transparent proxy listening", "addr", p.addr)

	// Start the iptables reconciler — installs the NEXUS_AGENT
	// chain immediately and keeps it healed against firewalld /
	// ufw / manual flushes. Failure to install on this first
	// pass is fatal: without the chain, no traffic reaches us.
	port, err := portFromAddr(p.addr)
	if err != nil {
		return fmt.Errorf("derive proxy port from addr %q: %w", p.addr, err)
	}
	p.reconciler = NewReconciler(slog.Default(), port)
	if err := p.reconciler.Start(ctx); err != nil {
		// Critical: clear the field so the caller's deferred
		// Stop() does NOT then call reconciler.Stop(), which
		// would block forever on doneCh — the loop goroutine
		// never started so it never closes that channel.
		p.reconciler = nil
		_ = ln.Close()
		return fmt.Errorf("start iptables reconciler: %w", err)
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				p.wg.Wait()
				return nil
			default:
				slog.Error("accept failed", "error", err)
				continue
			}
		}
		p.sem <- struct{}{} // backpressure: block accept when at capacity
		p.wg.Add(1)
		go func() {
			defer func() { <-p.sem }()
			defer p.wg.Done()
			p.handleConn(ctx, conn)
		}()
	}
}

func (p *LinuxPlatform) Stop() error {
	p.stopOnce.Do(func() { close(p.done) })

	// Tear down the iptables chain BEFORE closing the listener so
	// in-flight connections still get serviced by the existing
	// listener while the kernel stops redirecting new ones. This
	// avoids the brief window where the kernel sends connections to
	// a closing listener.
	if p.reconciler != nil {
		if err := p.reconciler.Stop(); err != nil {
			slog.Warn("reconciler stop returned error", "error", err)
		}
	}

	if p.listener != nil {
		_ = p.listener.Close()
	}
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		slog.Warn("proxy drain timeout")
	}
	return nil
}

// portFromAddr extracts the TCP port from a "host:port" address
// string. The reconciler needs the bare port for its
// `-j REDIRECT --to-ports N` rule.
func portFromAddr(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("parse port %q: %w", portStr, err)
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("port out of range: %d", port)
	}
	return port, nil
}

func (p *LinuxPlatform) handleConn(ctx context.Context, clientConn net.Conn) {
	defer func() { _ = clientConn.Close() }()
	startedAt := time.Now()

	tcpConn, ok := clientConn.(*net.TCPConn)
	if !ok {
		return
	}

	// Resolve original destination via SO_ORIGINAL_DST
	dstIP, dstPort, err := getOriginalDst(tcpConn)
	if err != nil {
		slog.Warn("SO_ORIGINAL_DST failed", "error", err)
		return
	}

	// Peek at TLS ClientHello for SNI (hostname)
	sni, peeked, peekErr := proxy.PeekSNI(clientConn, 5*time.Second)
	dstHost := sni
	if dstHost == "" {
		dstHost = dstIP // fallback to IP when no SNI
	}

	// Resolve source PID
	srcAddr := clientConn.RemoteAddr().(*net.TCPAddr)
	pid := findPIDBySocket(srcAddr.IP.String(), srcAddr.Port, dstIP, dstPort)
	var procMeta api.ProcessMeta
	if pid > 0 {
		procMeta, _ = p.ProcessInfo(pid)
	}

	intercepted := api.InterceptedConn{
		FlowID:  fmt.Sprintf("%s:%d-%s:%d-%d", srcAddr.IP, srcAddr.Port, dstIP, dstPort, startedAt.UnixMilli()),
		SrcIP:   srcAddr.IP.String(),
		SrcPort: srcAddr.Port,
		DstIP:   dstIP,
		DstPort: dstPort,
		DstHost: dstHost,
		Process: procMeta,
	}

	if p.handler == nil {
		return
	}
	decision := p.handler.HandleConnection(intercepted)

	var bytesIn, bytesOut int64
	bumpStatus := ""
	// intercept_ms = time the agent spent on its own intercept work
	// (SO_ORIGINAL_DST + SNI peek + PID resolve + decision) before handing
	// off to upstream. Stamped just before the first upstream operation
	// in each branch; zero-value means we never reached upstream (e.g.
	// deny) and we report nothing.
	var interceptDoneAt time.Time
	// bumpedViaTLSBump is set when the inspect path ran through
	// proxy.BumpFlow (shared/tlsbump), which emits its own per-HTTP-request
	// audit rows. When true, the flow-level OnFlowComplete row below is
	// skipped to avoid double-auditing — mirrors the macOS NE bridge.
	var bumpedViaTLSBump bool

	switch decision {
	case api.DecisionDeny:
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}

	case api.DecisionPassthrough:
		serverAddr := net.JoinHostPort(dstIP, strconv.Itoa(dstPort))
		// SO_MARK-stamped dialer so this upstream connection is
		// excluded from our own REDIRECT rule (FR-L4).
		interceptDoneAt = time.Now()
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		serverConn, err := p.upstreamDialer.DialContext(dialCtx, "tcp", serverAddr)
		cancel()
		if err != nil {
			slog.Warn("connect to server failed", "addr", serverAddr, "error", err)
			break
		}
		defer func() { _ = serverConn.Close() }()
		// Replay peeked bytes
		if len(peeked) > 0 {
			if _, err := serverConn.Write(peeked); err != nil {
				slog.Warn("replay peeked bytes failed", "error", err)
				break
			}
		}
		bytesOut, bytesIn = proxy.Relay(clientConn, serverConn)

	case api.DecisionInspect:
		if p.bridgeDeps == nil || peekErr != nil {
			// Cannot inspect — bridge deps unwired (device CA load failed
			// at boot) or the TLS ClientHello peek failed (non-TLS /
			// server-speaks-first protocol). Fail open to passthrough so
			// the user's flow still works.
			serverAddr := net.JoinHostPort(dstIP, strconv.Itoa(dstPort))
			// SO_MARK-stamped (FR-L4) — same reasoning as the
			// passthrough path above.
			interceptDoneAt = time.Now()
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			serverConn, derr := p.upstreamDialer.DialContext(dialCtx, "tcp", serverAddr)
			cancel()
			if derr != nil {
				break
			}
			defer func() { _ = serverConn.Close() }()
			if len(peeked) > 0 {
				if _, werr := serverConn.Write(peeked); werr != nil {
					slog.Warn("replay peeked bytes failed (inspect fallback)", "error", werr)
					break
				}
			}
			bytesOut, bytesIn = proxy.Relay(clientConn, serverConn)
			bumpStatus = "BUMP_FAILED_PASSTHROUGH"
		} else {
			// Inspect via shared/tlsbump.BumpConnection (the same engine the
			// macOS NE bridge, the compliance proxy, and the AI gateway use).
			// BumpFlow terminates TLS, runs the hook pipeline, and emits
			// per-HTTP-request audit rows directly via its AuditEmitter — so
			// the flow-level OnFlowComplete row below is skipped. Any bump
			// failure (cert-pin client, non-TLS upstream) falls open to an
			// opaque relay inside BumpFlow, preserving the user's flow.
			interceptDoneAt = time.Now()
			fp := proxy.FlowProcess{Name: procMeta.Name, Bundle: procMeta.BundleID, User: procMeta.User}
			if err := proxy.BumpFlow(ctx, clientConn, peeked, dstHost, dstPort, intercepted.FlowID, fp, *p.bridgeDeps); err != nil {
				slog.Debug("bump flow ended with error", "host", dstHost, "error", err)
			}
			bumpedViaTLSBump = true
		}
	}

	// Audit callback. Skipped for inspect flows bumped via tlsbump —
	// BumpFlow already emitted per-HTTP-request rows (mirrors the macOS
	// NE bridge); writing a flow-level row here would double-audit.
	if auditor, ok := p.handler.(api.FlowAuditor); ok && !bumpedViaTLSBump {
		auditor.OnFlowComplete(api.FlowResult{
			FlowID:     intercepted.FlowID,
			SrcIP:      intercepted.SrcIP,
			DstHost:    dstHost,
			DstIP:      dstIP,
			DstPort:    dstPort,
			Process:    procMeta,
			Decision:   decision,
			BytesIn:    bytesIn,
			BytesOut:   bytesOut,
			DurationMs: int(time.Since(startedAt).Milliseconds()),
			BumpStatus: bumpStatus,
			StartedAt:  startedAt,
			// A Linux raw relay has no distinct upstream call to time, so
			// UpstreamTtfbMs/TotalMs stay nil; LatencyBreakdown carries the
			// agent's own intercept overhead (intercept_ms).
			LatencyBreakdown: mergeInterceptMs(nil, startedAt, interceptDoneAt),
		})
	}
}

// mergeInterceptMs stamps intercept_ms (the agent's own intercept overhead —
// SO_ORIGINAL_DST + SNI peek + PID resolve + decision) into the latency
// breakdown map. interceptDoneAt is zero when no upstream branch ran (e.g.
// DecisionDeny), in which case we don't add the key. Creates the map on
// demand when the phase sink produced nothing else.
func mergeInterceptMs(breakdown map[string]int, startedAt, interceptDoneAt time.Time) map[string]int {
	if interceptDoneAt.IsZero() {
		return breakdown
	}
	ms := int(interceptDoneAt.Sub(startedAt).Milliseconds())
	if ms < 0 {
		ms = 0
	}
	if breakdown == nil {
		breakdown = make(map[string]int, 1)
	}
	breakdown["intercept_ms"] = ms
	return breakdown
}

// getOriginalDst retrieves the original destination address from a connection
// that was redirected via iptables REDIRECT.
func getOriginalDst(conn *net.TCPConn) (string, int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return "", 0, fmt.Errorf("syscall conn: %w", err)
	}

	var addr unix.RawSockaddrInet4
	var sysErr error

	err = raw.Control(func(fd uintptr) {
		addrLen := uint32(unix.SizeofSockaddrInet4)
		_, _, errno := unix.Syscall6(
			unix.SYS_GETSOCKOPT,
			fd,
			uintptr(unix.SOL_IP),
			uintptr(soOriginalDst),
			uintptr(unsafe.Pointer(&addr)),
			uintptr(unsafe.Pointer(&addrLen)),
			0,
		)
		if errno != 0 {
			sysErr = errno
		}
	})
	if err != nil {
		return "", 0, err
	}
	if sysErr != nil {
		return "", 0, fmt.Errorf("getsockopt SO_ORIGINAL_DST: %w", sysErr)
	}

	ip := net.IPv4(addr.Addr[0], addr.Addr[1], addr.Addr[2], addr.Addr[3])
	// Port is in network byte order (big-endian) in memory.
	portBuf := (*[2]byte)(unsafe.Pointer(&addr.Port))
	port := int(binary.BigEndian.Uint16(portBuf[:]))

	return ip.String(), port, nil
}
