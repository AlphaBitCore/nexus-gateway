//go:build windows

// Package windows implements an explicit CONNECT proxy for HTTPS interception on Windows.
// Traffic routing to the proxy is configured by the installer (system proxy
// settings, PAC file, or GPO). Process resolution uses GetExtendedTcpTable
// from iphlpapi.dll, QueryFullProcessImageNameW from kernel32.dll, and
// NtQueryInformationProcess (ntdll) for the parent PID.
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
	listener  net.Listener // IPv4 loopback (127.0.0.1)
	listener6 net.Listener // IPv6 loopback ([::1]); nil if IPv6 unavailable
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

	// QUIC-force-TCP-fallback allowlist (G-2), kept in sync from Hub config
	// via SetForceQUICFallbackImages and pushed to the driver as WFP policy.
	quicMu       sync.Mutex
	quicFallback []string
	policyGen    uint32
}

// SetBridgeDeps wires the shared/tlsbump bridge dependencies so the inspect
// path bumps each flow through proxy.BumpFlow. Satisfies
// api.BridgeDepsReceiver. Called once at boot before Start.
func (p *WindowsPlatform) SetBridgeDeps(deps *proxy.BridgeDeps) {
	p.bridgeDeps = deps
}

// SetForceQUICFallbackImages updates the QUIC-force-TCP-fallback allowlist
// (process image basenames, e.g. "chrome.exe") and pushes it to the driver
// as WFP policy. Satisfies api.ForceQUICFallbackController. Safe to call
// before Start (the list is stored and re-pushed once the driver is up) and
// on every Hub config change. No-op effect in system-proxy fallback mode
// (no kernel driver to push to).
func (p *WindowsPlatform) SetForceQUICFallbackImages(images []string) {
	p.quicMu.Lock()
	p.quicFallback = append([]string(nil), images...)
	p.quicMu.Unlock()
	p.pushWFPPolicy()
}

// pushWFPPolicy builds the current WFP policy and pushes it to the driver.
// No-op when the kernel driver isn't active (fallback mode or pre-Start).
//
// MERGE INVARIANT: the driver replaces its entire policy snapshot on every
// PUSH_POLICY (NexusPolicyApply), so this must send the FULL current policy.
// Today only QUICFallbackImages is populated; when the as-yet-unused
// BypassPIDs / BypassCIDRs / KillSwitch fields gain their own Set*() methods,
// each must store into a WindowsPlatform field and call this single builder,
// NOT push a partial Policy — otherwise concurrent updates erase each other.
//
// ORDERING: quicMu is held across the whole push (including the PushPolicy
// IOCTL), so two concurrent pushes can't interleave or land out of order.
// This matters because the driver can't reject a stale push itself — its
// policy generation is per-agent-process (it resets to 1 when the agent
// restarts, while the driver persists), so an older generation is not a
// reliable kernel-side "stale" signal; ordering has to be guaranteed here.
// Pushes are rare (config changes), so serialising them costs nothing.
func (p *WindowsPlatform) pushWFPPolicy() {
	p.quicMu.Lock()
	defer p.quicMu.Unlock()
	if p.wfp == nil {
		return // driver not active; Start re-pushes once it is.
	}
	p.policyGen++
	pol := Policy{
		Generation: p.policyGen,
		// Safe to reference directly (not copy): quicFallback is stable under
		// quicMu, which we hold across the synchronous PushPolicy below.
		QUICFallbackImages: p.quicFallback,
	}
	if err := p.wfp.PushPolicy(context.Background(), pol); err != nil {
		slog.Warn("push WFP policy failed", "error", err,
			"quicFallbackImages", len(pol.QUICFallbackImages))
		return
	}
	slog.Info("WFP policy pushed", "generation", pol.Generation,
		"quicFallbackImages", len(pol.QUICFallbackImages))
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

	// Also accept IPv6 redirects. The driver rewrites V6 connects to
	// [::1]:proxyPort (Callouts.c NexusConnectRedirectV6), so without a
	// loopback-v6 listener on the SAME port those flows hit a closed port
	// and IPv6 interception silently fails. Non-fatal: if IPv6 is disabled
	// on the host the v4 path still works.
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		v6Addr := net.JoinHostPort("::1", strconv.Itoa(tcpAddr.Port))
		if ln6, err6 := net.Listen("tcp6", v6Addr); err6 != nil {
			slog.Warn("IPv6 loopback listener unavailable; v6 redirects will not be intercepted",
				"addr", v6Addr, "error", err6)
		} else {
			p.listener6 = ln6
			slog.Info("IPv6 transparent proxy listening", "addr", v6Addr)
		}
	}

	// Attempt NexusWFP kernel capture first. Failure here is
	// non-fatal: we degrade to the legacy CONNECT-proxy +
	// system-proxy path, log a warning, and report state=degraded
	// so the tray turns yellow + Dashboard's Diagnostics surfaces
	// the bypass.
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
			// Push any QUIC-fallback allowlist already received from Hub
			// config before the driver came up (SetForceQUICFallbackImages
			// no-ops the push while p.wfp is nil).
			p.pushWFPPolicy()
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

	// Closing the listeners on ctx-cancellation unblocks the serve loops.
	go func() {
		<-ctx.Done()
		ln.Close()
		if p.listener6 != nil {
			p.listener6.Close()
		}
	}()

	// One accept loop per listener (v4 + v6), both feeding the same
	// bounded handler pool. Start blocks until both loops exit (listeners
	// closed on ctx.Done) and all in-flight handlers have drained.
	var serveWg sync.WaitGroup
	serveWg.Add(1)
	go func() { defer serveWg.Done(); p.serve(ctx, ln) }()
	if p.listener6 != nil {
		serveWg.Add(1)
		go func() { defer serveWg.Done(); p.serve(ctx, p.listener6) }()
	}
	serveWg.Wait()
	p.wg.Wait()
	return nil
}

// serve runs the accept loop for one listener, dispatching each accepted
// connection to handleConn under the shared concurrency semaphore. It
// returns when the listener is closed (ctx cancellation).
func (p *WindowsPlatform) serve(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
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
	if p.listener6 != nil {
		p.listener6.Close()
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

// portFromAddrWindows extracts the TCP port from a "host:port" string for
// use as the NexusWFP proxy port (sent via SET_PROXY_PORT). Mirrors linux.go's
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
