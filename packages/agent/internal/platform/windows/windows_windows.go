//go:build windows

// Package windows implements an explicit CONNECT proxy for HTTPS interception on Windows.
// Traffic routing to the proxy is configured by the installer (system proxy
// settings, PAC file, or GPO). Process resolution uses GetExtendedTcpTable
// from iphlpapi.dll and QueryFullProcessImageNameW from kernel32.dll.
package windows

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/catrust"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

const defaultWinAddr = "127.0.0.1:19080"

// WindowsPlatform implements api.Platform for Windows via CONNECT proxy +
// iphlpapi PID resolution + Win32 process metadata.
const maxConcurrentConns = 512

type WindowsPlatform struct {
	handler   api.ConnectionHandler
	listener  net.Listener
	wg        sync.WaitGroup
	done      chan struct{}
	stopOnce  sync.Once
	sem       chan struct{} // bounds concurrent connection handlers
	tlsEngine *agentTLS.Engine
	addr      string

	// NexusWFP client when the kernel driver loaded successfully;
	// nil in SystemProxyFallback mode. handleConn branches on this
	// field: when set, the per-connection setup phase looks up the
	// original destination from the driver's flow table; when nil,
	// it parses HTTP CONNECT from the client (legacy behaviour).
	wfp  WFPClient
	mode api.InterceptionMode

	// bridgeDeps routes inspect flows through shared/tlsbump.BumpConnection
	// (via proxy.BumpFlow) — the same engine macOS, the compliance proxy,
	// and the AI gateway use. Set once at boot via SetBridgeDeps before
	// Start launches the accept loop; read by the per-connection handlers.
	bridgeDeps *proxy.BridgeDeps
}

// SetBridgeDeps wires the shared/tlsbump bridge dependencies so the inspect
// path bumps each flow through proxy.BumpFlow. Satisfies
// api.BridgeDepsReceiver. Called once at boot before Start.
func (p *WindowsPlatform) SetBridgeDeps(deps *proxy.BridgeDeps) {
	p.bridgeDeps = deps
}

// InterceptionMode satisfies api.InterceptionModeReporter. Returns
// api.ModeNexusWFP when the kernel driver is up, api.ModeSystemProxyFallback
// otherwise. Set during Start().
func (p *WindowsPlatform) InterceptionMode() api.InterceptionMode {
	if p.mode == "" {
		// Start() not called yet — pessimistic default avoids the
		// Dashboard rendering a falsely-positive "NexusWFP" badge.
		return api.ModeSystemProxyFallback
	}
	return p.mode
}

// NewPlatform creates a new Windows platform shim.
func NewPlatform(addr string) api.Platform {
	if addr == "" {
		addr = defaultWinAddr
	}
	return &WindowsPlatform{
		done: make(chan struct{}),
		sem:  make(chan struct{}, maxConcurrentConns),
		addr: addr,
	}
}

func (p *WindowsPlatform) Start(ctx context.Context, handler api.ConnectionHandler) error {
	p.handler = handler

	// Device CA — load from disk if persisted, otherwise mint + persist.
	// Mirrors the linux.go pattern (see linux.go Start()). Without this,
	// every daemon restart minted a fresh CA in memory and added it to
	// the Windows Root store, polluting the trust store and breaking the
	// MSI's NEXUS_DEVICE_CA_PEM env var (which points at a stable path).
	caCertPath := filepath.Join(paths.DefaultPaths().StateDir, "device-ca.pem")
	caKeyPath := filepath.Join(paths.DefaultPaths().StateDir, "device-ca.key")
	caCert, caKey, generated, caErr := agentTLS.LoadOrGenerateCA(caCertPath, caKeyPath)
	var err error
	switch {
	case caErr == nil:
		if generated {
			slog.Info("device CA minted + persisted",
				"cert_path", caCertPath, "key_path", caKeyPath)
		} else {
			slog.Info("device CA loaded from disk",
				"cert_path", caCertPath, "subject", caCert.Subject.CommonName)
		}
		p.tlsEngine, err = agentTLS.NewEngine(caCert, caKey, 2000, time.Hour)
	default:
		slog.Warn("device CA disk persistence unavailable; using ephemeral CA",
			"cert_path", caCertPath, "error", caErr,
			"hint", "MSI install creates %ProgramData%\\NexusAgent\\ with LocalSystem write; running the daemon manually as a non-elevated user can hit this")
		p.tlsEngine, err = agentTLS.NewEngine(nil, nil, 2000, time.Hour)
	}
	if err != nil {
		return fmt.Errorf("init TLS engine: %w", err)
	}

	// Best-effort: install the device CA into the Windows Root store so
	// intercepted TLS connections are trusted by host clients (browsers,
	// Win32 HTTP clients). Idempotent — certutil -addstore -f no-ops when
	// the cert is already in the store. Failure is non-fatal: clients
	// will see cert-untrusted warnings but the daemon still functions.
	if caPEM := p.tlsEngine.CACertPEM(); len(caPEM) > 0 {
		if installErr := catrust.InstallCACert(caPEM, "nexus-agent-device-ca"); installErr != nil {
			slog.Warn("device CA auto-install failed (non-fatal)", "error", installErr)
		} else {
			slog.Info("device CA installed into OS trust store")
		}
	}

	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.addr, err)
	}
	p.listener = ln

	// Attempt NexusWFP kernel capture first. Failure here is
	// non-fatal: we degrade to the legacy CONNECT-proxy +
	// system-proxy path, log a warning, and report state=degraded
	// so the tray turns yellow + Dashboard's Diagnostics surfaces
	// the bypass (FR-W4).
	proxyPort, perr := portFromAddrWindows(p.addr)
	if perr == nil {
		wfpClient := NewClient(slog.Default())
		startOpts := StartOptions{
			AgentPID:     uint32(os.Getpid()),
			TCPProxyPort: uint16(proxyPort),
			UDPProxyPort: uint16(proxyPort),
		}
		if err := wfpClient.Start(ctx, startOpts); err != nil {
			slog.Warn("NexusWFP capture unavailable — falling back to system-proxy",
				"error", err,
				"impact", "apps that ignore WinINet (Electron/httpx/curl-with-custom-cert) will bypass filtering",
				"resolution", "see https://nexus-gateway.com/docs/agent/nexuswfp-troubleshooting")
			p.mode = api.ModeSystemProxyFallback
		} else {
			p.wfp = wfpClient
			p.mode = api.ModeNexusWFP
			slog.Info("NexusWFP capture active",
				"proxy_port", proxyPort,
				"mode", "kernel transparent proxy")
		}
	} else {
		slog.Warn("NexusWFP disabled: cannot derive proxy port from addr", "addr", p.addr, "error", perr)
		p.mode = api.ModeSystemProxyFallback
	}

	if p.mode == api.ModeNexusWFP {
		slog.Info("transparent proxy listening", "addr", p.addr)
	} else {
		slog.Info("CONNECT proxy listening (degraded mode)", "addr", p.addr)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
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

func (p *WindowsPlatform) Stop() error {
	p.stopOnce.Do(func() { close(p.done) })

	// Close the NexusWFP handle BEFORE closing the listener so the
	// kernel stops handing us packets first; in-flight connections
	// still get serviced by the listener while no new ones land.
	if p.wfp != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := p.wfp.Stop(stopCtx); err != nil {
			slog.Warn("NexusWFP stop returned error", "error", err)
		}
		cancel()
	}

	if p.listener != nil {
		p.listener.Close()
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

// portFromAddrWindows extracts the TCP port from a "host:port" string
// for use as the WinDivert REDIRECT target. Mirrors linux.go's
// portFromAddr but lives under a Windows build tag.
func portFromAddrWindows(addr string) (int, error) {
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

func (p *WindowsPlatform) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()
	startedAt := time.Now()

	var (
		dstHost          string
		dstPort          int
		err              error
		transparent      bool   // true → WinDivert mode (no CONNECT verb)
		preSniffedPeeked []byte // ClientHello bytes consumed during setup (WinDivert mode only)
		preSniffedErr    error
		wfpPID           int // kernel-supplied owning PID (WFP path); 0 on the fallback path
	)
	if p.wfp != nil {
		transparent = true
		// NexusWFP transparent path: connection arrived via kernel
		// redirect. Look up the original destination by client
		// source port via the driver.
		srcAddr, ok := clientConn.RemoteAddr().(*net.TCPAddr)
		if !ok {
			slog.Debug("non-TCP RemoteAddr; dropping")
			return
		}
		origAddrPort, kernelPID, ok := p.wfp.GetOriginalDestination(ctx, uint16(srcAddr.Port), false)
		if !ok {
			// Unknown flow — probably a manual probe (curl
			// to 127.0.0.1:19080). Reject so health-checks
			// don't accumulate as audit events.
			slog.Debug("no flow table entry for connection", "srcPort", srcAddr.Port)
			return
		}
		dstHost = origAddrPort.Addr().String()
		dstPort = int(origAddrPort.Port())
		// The kernel driver already told us the owning PID for this
		// redirected flow — keep it so we don't recompute it the
		// expensive way (a full system TCP-table snapshot per
		// connection) below.
		wfpPID = int(kernelPID)
		// Peek the TLS ClientHello bytes once: gives us the SNI
		// (host upgrade from IP to hostname) AND the buffered
		// bytes the inspect / passthrough branches need to
		// replay upstream. PeekSNI uses a ReplayConn under the
		// hood so the original clientConn still holds the bytes
		// for the byte-level relay path; we keep an explicit
		// copy for the MITM path which needs them.
		var sni string
		sni, preSniffedPeeked, preSniffedErr = proxy.PeekSNI(clientConn, 5*time.Second)
		if sni != "" {
			dstHost = sni
		}
	} else {
		// Fallback: legacy CONNECT-proxy path. ParseCONNECT returns
		// a wrapped connection that replays any buffered bytes
		// (e.g. TLS ClientHello sent in the same TCP segment as
		// the CONNECT header).
		dstHost, dstPort, clientConn, err = proxy.ParseCONNECT(clientConn, 10*time.Second)
		if err != nil {
			slog.Debug("non-CONNECT request", "error", err)
			return
		}
	}

	// Resolve the client PID. On the WFP path the kernel driver already
	// supplied it (wfpPID) — use it directly. Only the legacy
	// CONNECT-proxy fallback (no kernel flow entry) pays the
	// GetExtendedTcpTable full-table scan.
	srcAddr := clientConn.RemoteAddr().(*net.TCPAddr)
	pid := wfpPID
	if pid <= 0 {
		pid = findOwnerPID(srcAddr.IP, srcAddr.Port)
	}
	var procMeta api.ProcessMeta
	if pid > 0 {
		procMeta, _ = p.ProcessInfo(pid)
	}

	intercepted := api.InterceptedConn{
		FlowID:  fmt.Sprintf("%s:%d-%s:%d-%d", srcAddr.IP, srcAddr.Port, dstHost, dstPort, startedAt.UnixMilli()),
		SrcIP:   srcAddr.IP.String(),
		SrcPort: srcAddr.Port,
		DstHost: dstHost,
		DstPort: dstPort,
		Process: procMeta,
	}

	if p.handler == nil {
		return
	}
	decision := p.handler.HandleConnection(intercepted)

	var bytesIn, bytesOut int64
	bumpStatus := ""
	var interceptDoneAt time.Time
	// bumpedViaTLSBump is set when the inspect path ran through
	// proxy.BumpFlow (shared/tlsbump), which emits its own per-HTTP-request
	// audit rows. When true, the flow-level OnFlowComplete row below is
	// skipped to avoid double-auditing — mirrors the macOS NE bridge.
	var bumpedViaTLSBump bool

	switch decision {
	case api.DecisionDeny:
		if transparent {
			// No CONNECT verb to reject; just close hard so the
			// app's SYN-ACK never gets a final ACK.
			if tc, ok := clientConn.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
		} else {
			proxy.RejectCONNECT(clientConn)
		}

	case api.DecisionPassthrough:
		// Transparent: relay without sending CONNECT response.
		// CONNECT mode: 200 OK then relay.
		serverAddr := net.JoinHostPort(dstHost, strconv.Itoa(dstPort))
		interceptDoneAt = time.Now()
		serverConn, err := net.DialTimeout("tcp", serverAddr, 10*time.Second)
		if err != nil {
			if !transparent {
				proxy.RejectCONNECT(clientConn)
			}
			slog.Warn("connect to server failed", "addr", serverAddr, "error", err)
			break
		}
		defer serverConn.Close()
		if !transparent {
			if err := proxy.RespondCONNECT(clientConn); err != nil {
				break
			}
		} else if len(preSniffedPeeked) > 0 {
			// Replay the ClientHello bytes consumed by the
			// SNI peek so upstream sees a complete TLS
			// handshake.
			if _, err := serverConn.Write(preSniffedPeeked); err != nil {
				slog.Warn("replay peeked bytes failed (transparent passthrough)", "error", err)
				break
			}
		}
		bytesOut, bytesIn = proxy.Relay(clientConn, serverConn)

	case api.DecisionInspect:
		// Transparent: peek already done above; CONNECT: peek now.
		var peeked []byte
		var peekErr error
		var sni string
		if transparent {
			peeked = preSniffedPeeked
			peekErr = preSniffedErr
			sni = dstHost // already SNI-promoted in the setup phase
		} else {
			if err := proxy.RespondCONNECT(clientConn); err != nil {
				break
			}
			sni, peeked, peekErr = proxy.PeekSNI(clientConn, 5*time.Second)
		}
		if p.bridgeDeps == nil || peekErr != nil {
			// Cannot inspect — bridge deps unwired (device CA load failed
			// at boot) or the TLS ClientHello peek failed (non-TLS /
			// server-speaks-first). Fail open to passthrough.
			serverAddr := net.JoinHostPort(dstHost, strconv.Itoa(dstPort))
			interceptDoneAt = time.Now()
			serverConn, derr := net.DialTimeout("tcp", serverAddr, 10*time.Second)
			if derr != nil {
				break
			}
			defer serverConn.Close()
			if transparent && len(peeked) > 0 {
				if _, werr := serverConn.Write(peeked); werr != nil {
					break
				}
			}
			bytesOut, bytesIn = proxy.Relay(clientConn, serverConn)
			bumpStatus = "BUMP_FAILED_PASSTHROUGH"
		} else {
			host := sni
			if host == "" {
				host = dstHost
			}
			// Inspect via shared/tlsbump.BumpConnection (the same engine the
			// macOS NE bridge, the compliance proxy, and the AI gateway use).
			// BumpFlow terminates TLS, runs the hook pipeline, and emits
			// per-HTTP-request audit rows directly — so the flow-level
			// OnFlowComplete row below is skipped. Any bump failure falls
			// open to an opaque relay inside BumpFlow.
			interceptDoneAt = time.Now()
			fp := proxy.FlowProcess{Name: procMeta.Name, Bundle: procMeta.BundleID, User: procMeta.User}
			if err := proxy.BumpFlow(ctx, clientConn, peeked, host, dstPort, intercepted.FlowID, fp, *p.bridgeDeps); err != nil {
				slog.Debug("bump flow ended with error", "host", host, "error", err)
			}
			bumpedViaTLSBump = true
		}
	}

	// Skipped for inspect flows bumped via tlsbump — BumpFlow already
	// emitted per-HTTP-request rows (mirrors the macOS NE bridge).
	if auditor, ok := p.handler.(api.FlowAuditor); ok && !bumpedViaTLSBump {
		auditor.OnFlowComplete(api.FlowResult{
			FlowID:     intercepted.FlowID,
			SrcIP:      intercepted.SrcIP,
			DstHost:    dstHost,
			DstPort:    dstPort,
			Process:    procMeta,
			Decision:   decision,
			BytesIn:    bytesIn,
			BytesOut:   bytesOut,
			DurationMs: int(time.Since(startedAt).Milliseconds()),
			BumpStatus: bumpStatus,
			StartedAt:  startedAt,
			// A Windows raw relay has no distinct upstream call to time, so
			// UpstreamTtfbMs/TotalMs stay nil; LatencyBreakdown carries the
			// agent's own intercept overhead (intercept_ms).
			LatencyBreakdown: mergeInterceptMsWindows(nil, startedAt, interceptDoneAt),
		})
	}
}

// mergeInterceptMsWindows stamps intercept_ms onto the breakdown map. See
// linux.go's mergeInterceptMs for the rationale; this is the Windows
// build's symmetric helper (separate definition so each platform file
// compiles independently under its own build tag).
func mergeInterceptMsWindows(breakdown map[string]int, startedAt, interceptDoneAt time.Time) map[string]int {
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
