//go:build windows

// wfp_audit_pump.go — inverted-call IRP pump goroutine.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §6
// SDD: docs/developers/specs/e59-s2-usermode-go-integration.md §5, §T3
//
// Pattern:
//   1. Bind device handle to a private IO completion port.
//   2. Post NEXUS_AUDIT_IRP_DEPTH (=8) overlapped IOCTLs, each carrying
//      a 4 KB output buffer.
//   3. A goroutine loops on GetQueuedCompletionStatus, parses the
//      completed buffer into FlowAuditEvent records, pushes them
//      into the FlowTable (so subsequent GetOriginalDestination
//      hits the in-memory cache) and into the auditCh channel
//      (non-blocking — drop with counter if the channel is full).
//   4. Re-post the same slot immediately. Sustained at 8 outstanding.
//   5. On Stop: CancelIoEx cancels every IRP; the loop drains them
//      and exits; we Close the IOCP handle.

package windows

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"net/netip"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

const auditIRPDepth = 8
const auditBufferSize = 4096

type auditPumpSlot struct {
	buf        []byte
	overlapped windows.Overlapped
}

type auditPump struct {
	log    *slog.Logger
	handle windows.Handle
	iocp   windows.Handle

	auditCh chan<- FlowAuditEvent
	flowTbl *wfpFlowTable

	slots [auditIRPDepth]auditPumpSlot

	droppedCount atomic.Uint64
	stopCh       chan struct{}
	stopped      chan struct{}
}

func newAuditPump(log *slog.Logger, handle windows.Handle, auditCh chan<- FlowAuditEvent, flowTbl *wfpFlowTable) *auditPump {
	return &auditPump{
		log:     log,
		handle:  handle,
		auditCh: auditCh,
		flowTbl: flowTbl,
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Start binds the device handle to a new IOCP, allocates the per-slot
// buffers, posts the initial NEXUS_AUDIT_IRP_DEPTH IRPs, and launches
// the worker goroutine.
func (p *auditPump) Start() error {
	// CreateIoCompletionPort(handle, NULL, 0, 0) — creates a new IOCP
	// bound to this device handle. We use the slot index as the
	// completion key so the worker can re-post the right slot.
	iocp, err := windows.CreateIoCompletionPort(p.handle, 0, 0, 0)
	if err != nil {
		return err
	}
	p.iocp = iocp

	// Allocate per-slot buffers + bind by completion key.
	for i := 0; i < auditIRPDepth; i++ {
		p.slots[i].buf = make([]byte, auditBufferSize)
	}

	// Post initial IRPs.
	for i := 0; i < auditIRPDepth; i++ {
		if err := p.postSlot(i); err != nil {
			// On any post failure, tear down what we started.
			_ = windows.CancelIoEx(p.handle, nil)
			windows.CloseHandle(p.iocp)
			p.iocp = 0
			return err
		}
	}

	go p.loop()
	return nil
}

func (p *auditPump) postSlot(idx int) error {
	// Reset overlapped — Internal/InternalHigh are I/O status; the
	// kernel writes them. Other fields zero.
	p.slots[idx].overlapped = windows.Overlapped{}
	// HEvent left zero — completion lands on the IOCP, not an event.

	// Stash slot index in OffsetHigh so the IOCP completion knows
	// which slot to re-post. OVERLAPPED has Offset/OffsetHigh that
	// the kernel ignores for DeviceIoControl (used only for file
	// offsets in ReadFile/WriteFile); we co-opt OffsetHigh as our
	// slot-tag.
	p.slots[idx].overlapped.OffsetHigh = uint32(idx)

	return ioctlPostAuditPump(p.handle, p.slots[idx].buf, &p.slots[idx].overlapped)
}

func (p *auditPump) Stop() error {
	// Cancel all outstanding IRPs on the handle.
	_ = windows.CancelIoEx(p.handle, nil)
	close(p.stopCh)
	<-p.stopped
	if p.iocp != 0 {
		windows.CloseHandle(p.iocp)
		p.iocp = 0
	}
	return nil
}

// loop blocks on GetQueuedCompletionStatus, drains completed IRPs into
// the audit channel + flow table, and re-posts the slot for the next
// batch. Exits when stopCh is closed AND every IRP has been drained.
func (p *auditPump) loop() {
	defer close(p.stopped)

	remaining := auditIRPDepth // outstanding IRPs we need to drain on Stop

	for {
		var bytes uint32
		var key uintptr
		var pOv *windows.Overlapped

		// 1-second timeout so we can check stopCh periodically.
		err := windows.GetQueuedCompletionStatus(p.iocp, &bytes, &key, &pOv, 1000)
		if err != nil {
			if errors.Is(err, windows.WAIT_TIMEOUT) {
				if p.isStopping() && remaining == 0 {
					return
				}
				continue
			}
			// Completed with error (e.g. cancelled). pOv may still be
			// non-nil — that's a slot completion we should account for.
			if pOv != nil {
				remaining--
				if p.isStopping() && remaining == 0 {
					return
				}
				// Re-post unless stopping.
				if !p.isStopping() {
					slot := int(pOv.OffsetHigh)
					if slot >= 0 && slot < auditIRPDepth {
						_ = p.postSlot(slot)
						remaining++
					}
				}
				continue
			}
			// No overlapped — IOCP closed or unrecoverable.
			return
		}

		if pOv == nil {
			continue
		}
		slot := int(pOv.OffsetHigh)
		if slot < 0 || slot >= auditIRPDepth {
			continue
		}

		// Parse the records.
		if bytes > 0 {
			events := parseFlowAuditEntries(p.slots[slot].buf[:bytes])
			for _, evt := range events {
				if p.flowTbl != nil && evt.Decision == DecisionRedirect {
					p.flowTbl.Insert(
						evt.SrcAddr.Port(),
						evt.Protocol == protoUDP,
						evt.OrigDstAddr,
						evt.ProcessID)
				}
				select {
				case p.auditCh <- evt:
				default:
					p.droppedCount.Add(1)
				}
			}
		}

		// Re-post unless stopping. If stopping, just account for the
		// completed IRP and exit when all are drained.
		if p.isStopping() {
			remaining--
			if remaining == 0 {
				return
			}
		} else {
			if err := p.postSlot(slot); err != nil {
				p.log.Warn("wfp: audit pump re-post failed", "slot", slot, "err", err)
				remaining--
				if remaining == 0 {
					return
				}
			}
		}
	}
}

func (p *auditPump) isStopping() bool {
	select {
	case <-p.stopCh:
		return true
	default:
		return false
	}
}

func (p *auditPump) DroppedCount() uint64 {
	return p.droppedCount.Load()
}

const (
	protoTCP uint8 = 6
	protoUDP uint8 = 17
)

// parseFlowAuditEntries unpacks NexusFlowAuditEntry records (Common.h
// in the driver). 64-byte densely-packed structs.
const flowAuditEntrySize = 64

func parseFlowAuditEntries(buf []byte) []FlowAuditEvent {
	count := len(buf) / flowAuditEntrySize
	if count == 0 {
		return nil
	}
	out := make([]FlowAuditEvent, 0, count)
	for i := 0; i < count; i++ {
		off := i * flowAuditEntrySize
		evt := FlowAuditEvent{
			TimestampUs: binary.LittleEndian.Uint64(buf[off+0:]),
			ProcessID:   binary.LittleEndian.Uint32(buf[off+8:]),
			ParentPID:   binary.LittleEndian.Uint32(buf[off+12:]),
			Protocol:    buf[off+17],
			Decision:    Decision(buf[off+18]),
		}
		evt.SrcAddr = decodeAddrPort(buf[off+16], buf[off+20:off+36],
			binary.LittleEndian.Uint16(buf[off+36:]))
		evt.OrigDstAddr = decodeAddrPort(buf[off+16], buf[off+40:off+56],
			binary.LittleEndian.Uint16(buf[off+56:]))
		out = append(out, evt)
	}
	return out
}

func decodeAddrPort(family uint8, addr16 []byte, port uint16) netip.AddrPort {
	switch family {
	case afInet:
		var a4 [4]byte
		copy(a4[:], addr16[:4])
		return netip.AddrPortFrom(netip.AddrFrom4(a4), port)
	case afInet6:
		var a16 [16]byte
		copy(a16[:], addr16[:16])
		return netip.AddrPortFrom(netip.AddrFrom16(a16), port)
	default:
		return netip.AddrPort{}
	}
}
