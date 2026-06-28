//go:build windows

// conn_windows.go — per-connection handling for the Windows platform shim.
// Split from windows_windows.go (which keeps platform lifecycle + the
// accept loops) along the same seam as the Linux build's conn_linux.go.

package windows

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/proxy"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
)

func (p *WindowsPlatform) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()
	startedAt := time.Now()

	var (
		dstHost          string
		dstPort          int
		err              error
		transparent      bool   // true → WFP redirect mode (no CONNECT verb)
		preSniffedPeeked []byte // ClientHello bytes consumed during setup (WFP redirect mode only)
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
