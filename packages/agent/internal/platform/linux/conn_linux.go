//go:build linux

package linux

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
)

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

	// Flow accounting for the diagnostics dashboard / InterceptionHealth.
	// Counted only for real intercepted TCP flows (post type-assertion).
	p.connectionsTotal.Add(1)
	p.activeSessions.Add(1)
	p.lastFlowAtNS.Store(startedAt.UnixNano())
	defer p.activeSessions.Add(-1)

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
// redirected via iptables / ip6tables REDIRECT. It branches on the address
// family of the accepted socket: IPv4 flows land on the 127.0.0.1 listener and
// read SOL_IP/SO_ORIGINAL_DST; IPv6 flows land on the [::1] listener and read
// SOL_IPV6/IP6T_SO_ORIGINAL_DST. The two options share the numeric value 80 but
// live at different socket levels, so the family must be detected first.
func getOriginalDst(conn *net.TCPConn) (string, int, error) {
	isV6 := false
	if la, ok := conn.LocalAddr().(*net.TCPAddr); ok && la.IP.To4() == nil {
		isV6 = true
	}

	raw, err := conn.SyscallConn()
	if err != nil {
		return "", 0, fmt.Errorf("syscall conn: %w", err)
	}

	var (
		ip     net.IP
		port   int
		sysErr error
	)
	cErr := raw.Control(func(fd uintptr) {
		if isV6 {
			var addr unix.RawSockaddrInet6
			addrLen := uint32(unix.SizeofSockaddrInet6)
			_, _, errno := unix.Syscall6(
				unix.SYS_GETSOCKOPT,
				fd,
				uintptr(unix.SOL_IPV6),
				uintptr(ip6tSOOriginalDst),
				uintptr(unsafe.Pointer(&addr)),
				uintptr(unsafe.Pointer(&addrLen)),
				0,
			)
			if errno != 0 {
				sysErr = errno
				return
			}
			// Copy out of the syscall struct so the net.IP doesn't alias it.
			ip = net.IP(append([]byte(nil), addr.Addr[:]...))
			portBuf := (*[2]byte)(unsafe.Pointer(&addr.Port))
			port = int(binary.BigEndian.Uint16(portBuf[:]))
			return
		}
		var addr unix.RawSockaddrInet4
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
			return
		}
		ip = net.IPv4(addr.Addr[0], addr.Addr[1], addr.Addr[2], addr.Addr[3])
		// Port is in network byte order (big-endian) in memory.
		portBuf := (*[2]byte)(unsafe.Pointer(&addr.Port))
		port = int(binary.BigEndian.Uint16(portBuf[:]))
	})
	if cErr != nil {
		return "", 0, cErr
	}
	if sysErr != nil {
		return "", 0, fmt.Errorf("getsockopt SO_ORIGINAL_DST (v6=%v): %w", isV6, sysErr)
	}

	return ip.String(), port, nil
}
