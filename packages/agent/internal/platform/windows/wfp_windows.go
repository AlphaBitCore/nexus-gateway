//go:build windows

// Package windows — NexusWFP user-mode client.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md
// SDD: docs/developers/specs/e59-s2-usermode-go-integration.md

package windows

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// ─── Public API (per SDD §4) ────────────────────────────────────────

type FlowAuditEvent struct {
	ProcessID   uint32
	ParentPID   uint32
	SrcAddr     netip.AddrPort
	OrigDstAddr netip.AddrPort
	Protocol    uint8
	Decision    Decision
	TimestampUs uint64
}

type Decision uint8

const (
	DecisionRedirect Decision = 1
	DecisionPermit   Decision = 2
	DecisionBlock    Decision = 3
)

type WFPClient interface {
	Start(ctx context.Context, opts StartOptions) error
	Stop(ctx context.Context) error
	PushPolicy(ctx context.Context, p Policy) error
	GetOriginalDestination(ctx context.Context, localPort uint16, isUDP bool) (netip.AddrPort, uint32, bool)
}

type StartOptions struct {
	AgentPID     uint32
	TCPProxyPort uint16
	UDPProxyPort uint16
}

type Policy struct {
	Generation  uint32
	KillSwitch  bool
	BypassPIDs  []uint32
	BypassCIDRs []netip.Prefix
	// QUICFallbackImages is the macOS-parity force-QUIC-TCP-fallback
	// allowlist: process image basenames (e.g. "chrome.exe") whose UDP/443
	// the driver blocks so they fall back to interceptable TCP/443.
	QUICFallbackImages []string
}

var (
	ErrDriverUnavailable = errors.New("wfp: driver service not running")
	ErrVersionMismatch   = errors.New("wfp: driver protocol version mismatch")
	ErrAuditPumpStarved  = errors.New("wfp: audit-pump IRP queue empty for > 30s")
	ErrAlreadyStarted    = errors.New("wfp: client already started")
)

func NewClient(log *slog.Logger) WFPClient {
	if log == nil {
		log = slog.Default()
	}
	return &wfpClient{
		log: log,
	}
}

// ─── Implementation ──────────────────────────────────────────────────

type wfpClient struct {
	log *slog.Logger

	mu      sync.Mutex
	started bool
	handle  windows.Handle
	flowTbl *wfpFlowTable
	pump    *auditPump
}

func (c *wfpClient) Start(ctx context.Context, opts StartOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started {
		return ErrAlreadyStarted
	}
	if opts.AgentPID == 0 {
		opts.AgentPID = uint32(os.Getpid())
	}
	if opts.TCPProxyPort == 0 || opts.UDPProxyPort == 0 || opts.TCPProxyPort != opts.UDPProxyPort {
		return errors.New("wfp: StartOptions.TCPProxyPort and UDPProxyPort must be equal and non-zero")
	}

	handle, err := openDevice()
	if err != nil {
		// Likely SC service not running, or the driver isn't installed.
		return errors.Join(ErrDriverUnavailable, err)
	}

	helloResp, err := ioctlHello(handle, helloRequest{
		ProtocolVersion: protocolVersion,
		AgentPID:        opts.AgentPID,
	})
	if err != nil {
		windows.CloseHandle(handle)
		return err
	}
	if helloResp.DriverProtocolVersion != protocolVersion {
		windows.CloseHandle(handle)
		return ErrVersionMismatch
	}

	if err := ioctlSetProxyPort(handle, opts.TCPProxyPort, opts.UDPProxyPort); err != nil {
		windows.CloseHandle(handle)
		return err
	}

	c.handle = handle
	c.flowTbl = newWfpFlowTable()
	c.pump = newAuditPump(c.log, handle, c.flowTbl)

	if err := c.pump.Start(); err != nil {
		windows.CloseHandle(handle)
		c.handle = 0
		c.flowTbl = nil
		c.pump = nil
		return err
	}

	c.started = true
	c.log.Info("wfp: client started",
		"agentPid", opts.AgentPID,
		"tcpProxyPort", opts.TCPProxyPort,
		"driverVersion", helloResp.DriverProtocolVersion,
		"capabilities", helloResp.Capabilities)
	_ = ctx
	return nil
}

func (c *wfpClient) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.started {
		return nil
	}

	if c.pump != nil {
		_ = c.pump.Stop()
		c.pump = nil
	}
	if c.handle != 0 {
		windows.CloseHandle(c.handle)
		c.handle = 0
	}
	c.flowTbl = nil
	c.started = false
	_ = ctx
	return nil
}

func (c *wfpClient) PushPolicy(ctx context.Context, p Policy) error {
	c.mu.Lock()
	handle := c.handle
	started := c.started
	c.mu.Unlock()

	if !started {
		return ErrDriverUnavailable
	}
	body, err := MarshalPolicy(p)
	if err != nil {
		return err
	}
	_ = ctx
	return ioctlPushPolicy(handle, body)
}

func (c *wfpClient) GetOriginalDestination(ctx context.Context, localPort uint16, isUDP bool) (netip.AddrPort, uint32, bool) {
	c.mu.Lock()
	started := c.started
	handle := c.handle
	tbl := c.flowTbl
	c.mu.Unlock()

	if !started {
		return netip.AddrPort{}, 0, false
	}

	// 1. In-memory cache (hot path; most calls hit here because the
	//    audit pump just inserted the entry).
	if tbl != nil {
		if addr, pid, ok := tbl.Lookup(localPort, isUDP); ok {
			return addr, pid, true
		}
	}

	// 2. Authoritative lookup against the driver. Happens when the
	//    proxy accepts a connection before the audit-pump record has
	//    landed user-side, or when audit pump was back-pressured.
	var isUdpByte uint8
	if isUDP {
		isUdpByte = 1
	}
	resp, ok, err := ioctlGetOrigDst(handle, getOrigDstRequest{
		LocalPort: localPort,
		IsUDP:     isUdpByte,
	})
	if err != nil || !ok {
		_ = ctx
		return netip.AddrPort{}, 0, false
	}

	addr := decodeAddrPort(resp.Family, resp.OrigDstAddr[:], resp.OrigDstPort)
	// Cache for next time.
	if tbl != nil {
		tbl.Insert(localPort, isUDP, addr, resp.ProcessID)
	}
	return addr, resp.ProcessID, true
}
