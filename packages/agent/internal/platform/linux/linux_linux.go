//go:build linux

// Package linux implements transparent proxy interception for Linux using iptables
// REDIRECT (set up by the installer) and SO_ORIGINAL_DST for original
// destination resolution. Process identification uses the /proc filesystem.
package linux

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

const (
	soOriginalDst = 80 // linux/netfilter_ipv4.h SO_ORIGINAL_DST (level SOL_IP)
	// ip6tSOOriginalDst is the IPv6 equivalent: linux/netfilter_ipv6/ip6_tables.h
	// IP6T_SO_ORIGINAL_DST, read at level SOL_IPV6. Same numeric value (80) as
	// the v4 option but a different socket level, so the two are not
	// interchangeable.
	ip6tSOOriginalDst = 80
	defaultLinAddr    = "127.0.0.1:19080"
	// loopbackV6 is the address the IPv6 listener binds. iptables REDIRECT in
	// the OUTPUT chain rewrites locally-generated IPv6 flows to the v6 loopback,
	// so the proxy must accept there in addition to 127.0.0.1.
	loopbackV6 = "::1"
)

// LinuxPlatform implements api.Platform for Linux via iptables REDIRECT +
// transparent proxy + /proc process resolution.
const maxConcurrentConns = 512

type LinuxPlatform struct {
	handler  api.ConnectionHandler
	listener net.Listener
	// listener6 is the IPv6 loopback ([::1]) listener. Separate from the v4
	// listener because Go binds one family per listener and we want
	// loopback-only on both (binding ":port" would expose the proxy on all
	// interfaces). Nil when the host has no IPv6 — the agent then runs v4-only.
	listener6      net.Listener
	wg             sync.WaitGroup
	done           chan struct{}
	stopOnce       sync.Once
	sem            chan struct{} // bounds concurrent connection handlers
	tlsEngine      *agentTLS.Engine
	addr           string
	upstreamDialer *net.Dialer

	// reconciler maintains the NEXUS_AGENT redirect chain. Held as an
	// atomic pointer because InterceptionHealth() reads it from the
	// status-collector goroutine concurrently with Start (which sets it)
	// and Start's failure path (which clears it back to nil).
	reconciler atomic.Pointer[Reconciler]

	// bridgeDeps routes inspect flows through shared/tlsbump.BumpConnection
	// (via proxy.BumpFlow) — the same engine macOS, the compliance proxy,
	// and the AI gateway use. Set once at boot via SetBridgeDeps before
	// Start launches the accept loop; read by the per-connection handlers.
	bridgeDeps *proxy.BridgeDeps

	// InterceptionHealth flow counters. Written from handleConn
	// goroutines, read lock-free by InterceptionHealth(). startedAtNS is
	// set in Start; the others track flow lifecycle for the diagnostics
	// dashboard. The health *verdict* comes from the reconciler, not
	// these counters — an idle Linux host with zero flows is healthy
	// (the redirect chain is live the moment the reconciler installs it).
	startedAtNS      atomic.Int64
	connectionsTotal atomic.Int64
	activeSessions   atomic.Int32
	lastFlowAtNS     atomic.Int64
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

// InterceptionHealth implements api.InterceptionHealthReporter for Linux.
//
// Unlike the macOS NE shim — where zero IPC attaches means the user never
// approved the proxy dialog and the host is capturing nothing — the Linux
// iptables redirect chain is live the instant the reconciler installs it.
// An enrolled host sitting idle with zero flows is therefore HEALTHY, not
// degraded. So the health *verdict* is driven entirely by the reconciler's
// chain-upkeep state (installed + not persistently failing to repair),
// reported via SelfReported=true + DegradedReason. The flow counters are
// surfaced for the diagnostics dashboard only; they never gate the verdict.
//
// This is why we must NOT reuse the generic ConnectionsTotal==0 heuristic:
// it would mis-flag every idle Linux host. See status_health.go.
func (p *LinuxPlatform) InterceptionHealth() api.InterceptionHealth {
	h := api.InterceptionHealth{
		SelfReported:     true,
		ConnectionsTotal: p.connectionsTotal.Load(),
		ActiveSessions:   int(p.activeSessions.Load()),
	}
	if ns := p.startedAtNS.Load(); ns > 0 {
		h.StartedAt = time.Unix(0, ns)
	}
	if ns := p.lastFlowAtNS.Load(); ns > 0 {
		h.LastFlowAt = time.Unix(0, ns)
	}
	if rec := p.reconciler.Load(); rec != nil {
		h.DegradedReason = rec.degradedReason()
	} else {
		// Start has not run (or its reconciler install failed and the
		// pointer was cleared). Either way no redirect chain is being
		// maintained, so nothing is being captured.
		h.DegradedReason = "iptables redirect chain not installed"
	}
	h.Connected = h.DegradedReason == ""
	return h
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

	port, err := portFromAddr(p.addr)
	if err != nil {
		return fmt.Errorf("derive proxy port from addr %q: %w", p.addr, err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", p.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.addr, err)
	}
	p.listener = ln
	p.startedAtNS.Store(time.Now().UnixNano())
	slog.Info("transparent proxy listening", "addr", p.addr)

	// IPv6 loopback listener. The ip6tables REDIRECT rule sends locally-
	// generated IPv6 flows to [::1]:port, so without a listener there every
	// intercepted IPv6 connection would hit a closed port and break. Best-
	// effort: a host with IPv6 disabled simply runs IPv4-only.
	addr6 := net.JoinHostPort(loopbackV6, strconv.Itoa(port))
	if ln6, err6 := lc.Listen(ctx, "tcp6", addr6); err6 != nil {
		slog.Warn("IPv6 transparent proxy listen failed; continuing IPv4-only",
			"addr", addr6, "error", err6)
	} else {
		p.listener6 = ln6
		slog.Info("transparent proxy listening (IPv6)", "addr", addr6)
	}

	// Start the iptables reconciler — installs the NEXUS_AGENT
	// chain immediately and keeps it healed against firewalld /
	// ufw / manual flushes. Failure to install on this first
	// pass is fatal: without the chain, no traffic reaches us.
	rec := NewReconciler(slog.Default(), port)
	p.reconciler.Store(rec)
	if err := rec.Start(ctx); err != nil {
		// Critical: clear the field so the caller's deferred
		// Stop() does NOT then call reconciler.Stop(), which
		// would block forever on doneCh — the loop goroutine
		// never started so it never closes that channel.
		p.reconciler.Store(nil)
		_ = ln.Close()
		if p.listener6 != nil {
			_ = p.listener6.Close()
		}
		return fmt.Errorf("start iptables reconciler: %w", err)
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		if p.listener6 != nil {
			_ = p.listener6.Close()
		}
	}()

	// Serve the IPv6 listener in its own goroutine (tracked by wg) and the
	// IPv4 listener inline. Both feed handleConn and both unwind on ctx
	// cancel; wg.Wait below blocks until both loops and all in-flight
	// handlers have drained.
	if p.listener6 != nil {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.acceptLoop(ctx, p.listener6)
		}()
	}
	p.acceptLoop(ctx, ln)
	p.wg.Wait()
	return nil
}

// acceptLoop accepts connections on ln until ctx is cancelled — cancellation
// closes the listener, which surfaces as an Accept error and unwinds the loop.
// Each accepted connection runs on the bounded worker pool.
func (p *LinuxPlatform) acceptLoop(ctx context.Context, ln net.Listener) {
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

func (p *LinuxPlatform) Stop() error {
	p.stopOnce.Do(func() { close(p.done) })

	// Tear down the iptables chain BEFORE closing the listener so
	// in-flight connections still get serviced by the existing
	// listener while the kernel stops redirecting new ones. This
	// avoids the brief window where the kernel sends connections to
	// a closing listener.
	if rec := p.reconciler.Load(); rec != nil {
		if err := rec.Stop(); err != nil {
			slog.Warn("reconciler stop returned error", "error", err)
		}
	}

	if p.listener != nil {
		_ = p.listener.Close()
	}
	if p.listener6 != nil {
		_ = p.listener6.Close()
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
