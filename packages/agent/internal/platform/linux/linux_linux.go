//go:build linux

// Package linux implements transparent proxy interception for Linux using iptables
// REDIRECT (set up by the installer) and SO_ORIGINAL_DST for original
// destination resolution. Process identification uses the /proc filesystem.
package linux

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	agentTLS "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/tls"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
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
	relayClient    *relay.Client
	reconciler     *Reconciler
	upstreamDialer *net.Dialer
}

// InterceptionMode satisfies api.InterceptionModeReporter — Linux uses
// the iptables REDIRECT + SO_ORIGINAL_DST path the Reconciler keeps
// alive.
func (p *LinuxPlatform) InterceptionMode() api.InterceptionMode {
	return api.ModeIPTables
}

// NewPlatform creates a new Linux platform shim.
func NewPlatform(addr string, relayClient *relay.Client) api.Platform {
	if addr == "" {
		addr = defaultLinAddr
	}
	return &LinuxPlatform{
		done:           make(chan struct{}),
		sem:            make(chan struct{}, maxConcurrentConns),
		addr:           addr,
		relayClient:    relayClient,
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
	switch {
	case caErr == nil:
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

	ln, err := net.Listen("tcp", p.addr)
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
	defer clientConn.Close()
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
	var hookDecision, hookReason, hookReasonCode string
	var complianceTags []string
	var detectedProvider, detectedModel, apiKeyClass, apiKeyFingerprint string
	var promptTokens, completionTokens *int
	var usageExtractionStatus string
	var payloadRequest, payloadResponse []byte
	// Per-flow upstream phase sink. Populated by the relay's tracing
	// transport during MITMRelay; read at OnFlowComplete time. Nil for
	// flows that never reached the relay (passthrough / inspect-fallback).
	var phaseSink *traffic.PhaseSink
	// intercept_ms = time the agent spent on its own intercept work
	// (SO_ORIGINAL_DST + SNI peek + PID resolve + decision) before handing
	// off to upstream. Stamped just before the first upstream operation
	// in each branch; zero-value means we never reached upstream (e.g.
	// deny) and we report nothing.
	var interceptDoneAt time.Time

	// Build inspector callback if the handler supports compliance inspection.
	var inspector proxy.RequestInspector
	if ri, ok := p.handler.(api.RequestInspector); ok {
		inspector = func(ctx context.Context, host, method, path string, headers http.Header, body []byte) proxy.InspectionResult {
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

	// Build response inspector + usage detector when the handler supports them.
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

	switch decision {
	case api.DecisionDeny:
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.SetLinger(0)
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
		defer serverConn.Close()
		// Replay peeked bytes
		if len(peeked) > 0 {
			if _, err := serverConn.Write(peeked); err != nil {
				slog.Warn("replay peeked bytes failed", "error", err)
				break
			}
		}
		bytesOut, bytesIn = proxy.Relay(clientConn, serverConn)

	case api.DecisionInspect:
		if peekErr != nil || p.tlsEngine == nil {
			// Cannot inspect — fallback to passthrough
			serverAddr := net.JoinHostPort(dstIP, strconv.Itoa(dstPort))
			// SO_MARK-stamped (FR-L4) — same reasoning as the
			// passthrough path above.
			interceptDoneAt = time.Now()
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			serverConn, err := p.upstreamDialer.DialContext(dialCtx, "tcp", serverAddr)
			cancel()
			if err != nil {
				break
			}
			defer serverConn.Close()
			if len(peeked) > 0 {
				if _, err := serverConn.Write(peeked); err != nil {
					slog.Warn("replay peeked bytes failed (inspect fallback)", "error", err)
					break
				}
			}
			bytesOut, bytesIn = proxy.Relay(clientConn, serverConn)
			bumpStatus = "BUMP_FAILED_PASSTHROUGH"
		} else {
			var maxBodyBytes int64
			if capper, ok := p.handler.(api.BodyReadCapper); ok {
				maxBodyBytes = capper.ReadBodyCap()
			}
			// Attach a per-flow PhaseSink to ctx so the relay's tracing
			// transport captures upstream TTFB + upstream-total. On a
			// keep-alive flow the sink retains the most recent upstream
			// call's values when MITMRelay returns.
			phaseSink = traffic.NewPhaseSink()
			ctx = traffic.WithPhaseSink(ctx, phaseSink)
			interceptDoneAt = time.Now()
			bi, bo, inspection, err := proxy.MITMRelay(ctx, p.relayClient, clientConn, peeked, dstHost, dstPort, p.tlsEngine, inspector, respInspector, usageDetector, intercepted.FlowID, maxBodyBytes)
			hookDecision = inspection.Decision
			hookReason = inspection.Reason
			hookReasonCode = inspection.ReasonCode
			complianceTags = inspection.ComplianceTags
			detectedProvider = inspection.Provider
			detectedModel = inspection.Model
			apiKeyClass = inspection.ApiKeyClass
			apiKeyFingerprint = inspection.ApiKeyFingerprint
			promptTokens = inspection.PromptTokens
			completionTokens = inspection.CompletionTokens
			usageExtractionStatus = inspection.UsageExtractionStatus
			payloadRequest = inspection.PayloadRequest
			payloadResponse = inspection.PayloadResponse
			if err != nil {
				slog.Debug("MITM relay failed", "host", dstHost, "error", err)
				bumpStatus = "BUMP_FAILED_PASSTHROUGH"
			} else {
				bytesIn, bytesOut = bi, bo
				bumpStatus = "BUMP_SUCCESS"
			}
		}
	}

	// Audit callback
	if auditor, ok := p.handler.(api.FlowAuditor); ok {
		auditor.OnFlowComplete(api.FlowResult{
			FlowID:                intercepted.FlowID,
			SrcIP:                 intercepted.SrcIP,
			DstHost:               dstHost,
			DstIP:                 dstIP,
			DstPort:               dstPort,
			Process:               procMeta,
			Decision:              decision,
			BytesIn:               bytesIn,
			BytesOut:              bytesOut,
			DurationMs:            int(time.Since(startedAt).Milliseconds()),
			BumpStatus:            bumpStatus,
			StartedAt:             startedAt,
			HookDecision:          hookDecision,
			HookReason:            hookReason,
			HookReasonCode:        hookReasonCode,
			ComplianceTags:        complianceTags,
			Provider:              detectedProvider,
			Model:                 detectedModel,
			ApiKeyClass:           apiKeyClass,
			ApiKeyFingerprint:     apiKeyFingerprint,
			PromptTokens:          promptTokens,
			CompletionTokens:      completionTokens,
			UsageExtractionStatus: usageExtractionStatus,
			PayloadRequest:        payloadRequest,
			PayloadResponse:       payloadResponse,
			// Latency phase breakdown. nil pointers when phaseSink is nil
			// (passthrough / inspect-fallback paths that skip the relay)
			// or when the upstream call never produced bytes.
			UpstreamTtfbMs:  phaseSinkTtfb(phaseSink),
			UpstreamTotalMs: phaseSinkTotal(phaseSink),
			// Codec-layer / response-inspector breakdowns.
			ResponseHooksMs:  phaseSinkBreakdownInt(phaseSink, "response_hooks_ms"),
			LatencyBreakdown: mergeInterceptMs(phaseSinkBreakdownAll(phaseSink), startedAt, interceptDoneAt),
		})
	}
}

// phaseSinkBreakdownInt extracts a single named extras key as *int.
func phaseSinkBreakdownInt(ps *traffic.PhaseSink, key string) *int {
	if ps == nil {
		return nil
	}
	m := ps.Breakdown()
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	return &v
}

// phaseSinkBreakdownAll returns the full extras map (everything codec /
// inspector code stamped). nil when empty.
func phaseSinkBreakdownAll(ps *traffic.PhaseSink) map[string]int {
	if ps == nil {
		return nil
	}
	return ps.Breakdown()
}

// phaseSinkTtfb returns ps.TtfbMs() with a nil-receiver guard so the
// FlowResult builder above stays a single expression per field.
func phaseSinkTtfb(ps *traffic.PhaseSink) *int {
	if ps == nil {
		return nil
	}
	return ps.TtfbMs()
}

// phaseSinkTotal returns ps.TotalMs() with a nil-receiver guard.
func phaseSinkTotal(ps *traffic.PhaseSink) *int {
	if ps == nil {
		return nil
	}
	return ps.TotalMs()
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

// ProcessInfo resolves process metadata from /proc on Linux.
func (p *LinuxPlatform) ProcessInfo(pid int) (api.ProcessMeta, error) {
	meta := api.ProcessMeta{PID: pid}
	pidStr := strconv.Itoa(pid)

	// Executable path: /proc/[pid]/exe symlink
	exePath, err := os.Readlink(filepath.Join("/proc", pidStr, "exe"))
	if err == nil {
		meta.Path = exePath
		meta.Name = filepath.Base(exePath)
	}

	// Short name fallback: /proc/[pid]/comm
	if meta.Name == "" {
		if data, err := os.ReadFile(filepath.Join("/proc", pidStr, "comm")); err == nil {
			meta.Name = strings.TrimSpace(string(data))
		}
	}

	// User from UID in /proc/[pid]/status
	if data, err := os.ReadFile(filepath.Join("/proc", pidStr, "status")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if u, err := user.LookupId(fields[1]); err == nil {
						meta.User = u.Username
					}
				}
				break
			}
		}
	}

	if meta.Path == "" {
		return meta, fmt.Errorf("cannot resolve exe for pid %d", pid)
	}
	return meta, nil
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

// inodePIDCache caches inode→PID lookups for 2 seconds to avoid the expensive
// O(processes×fds) /proc scan on every intercepted connection.
var (
	inodeCacheMu  sync.RWMutex
	inodePIDCache = make(map[string]inodeCacheEntry)
	inodeCacheTTL = 2 * time.Second
)

type inodeCacheEntry struct {
	pid       int
	expiresAt time.Time
}

// findPIDBySocket looks up the owning PID of a TCP socket by parsing
// /proc/net/tcp and matching against /proc/[pid]/fd symlinks.
func findPIDBySocket(srcIP string, srcPort int, dstIP string, dstPort int) int {
	inode := findSocketInode(srcIP, srcPort, dstIP, dstPort)
	if inode == "" {
		return 0
	}

	// Check cache first.
	now := time.Now()
	inodeCacheMu.RLock()
	if entry, ok := inodePIDCache[inode]; ok && now.Before(entry.expiresAt) {
		inodeCacheMu.RUnlock()
		return entry.pid
	}
	inodeCacheMu.RUnlock()

	pid := findPIDByInode(inode)

	inodeCacheMu.Lock()
	inodePIDCache[inode] = inodeCacheEntry{pid: pid, expiresAt: now.Add(inodeCacheTTL)}
	// Evict expired entries periodically (when cache grows past 1024).
	if len(inodePIDCache) > 1024 {
		for k, v := range inodePIDCache {
			if now.After(v.expiresAt) {
				delete(inodePIDCache, k)
			}
		}
	}
	inodeCacheMu.Unlock()

	return pid
}

// findSocketInode parses /proc/net/tcp to find the inode of a socket matching
// the given local address.
func findSocketInode(localIP string, localPort int, remoteIP string, remotePort int) string {
	localHex := ipPortToHex(localIP, localPort)
	remoteHex := ipPortToHex(remoteIP, remotePort)

	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		// Match both local and remote addresses to avoid TOCTOU collisions
		// with TIME_WAIT sockets sharing the same local address.
		if strings.EqualFold(fields[1], localHex) &&
			(remoteHex == "" || strings.EqualFold(fields[2], remoteHex)) {
			return fields[9] // inode
		}
	}
	return ""
}

// findPIDByInode searches /proc/[pid]/fd/ for a socket inode.
func findPIDByInode(inode string) int {
	target := "socket:[" + inode + "]"
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && link == target {
				return pid
			}
		}
	}
	return 0
}

// ipPortToHex converts an IP:port pair to the /proc/net/tcp hex format.
// Example: "127.0.0.1:443" → "0100007F:01BB"
func ipPortToHex(ip string, port int) string {
	parsed := net.ParseIP(ip).To4()
	if parsed == nil {
		return ""
	}
	// /proc/net/tcp stores IPs in little-endian uint32 hex on x86
	hexIP := fmt.Sprintf("%02X%02X%02X%02X", parsed[3], parsed[2], parsed[1], parsed[0])
	hexPort := fmt.Sprintf("%04X", port)
	return hexIP + ":" + hexPort
}
