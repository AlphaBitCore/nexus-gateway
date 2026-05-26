//go:build windows

// wfp_flowtable.go — in-memory port → original-destination lookup.
//
// SKELETON. See wfp_windows.go header for build-tag context.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §5.1
// SDD: docs/developers/specs/e59-s2-usermode-go-integration.md §T2
//
// The driver also holds a copy of this mapping (it's authoritative
// when the kernel callout stamps a flow). The user-mode cache here
// is a hot path optimization for GetOriginalDestination calls — the
// proxy hits this every time it accepts a redirected connection,
// 1000s of times per second under load.

package windows

import (
	"net/netip"
	"sync"
	"time"
)

const wfpFlowTableTTL = 5 * time.Minute

type wfpFlowKey struct {
	localPort uint16
	isUDP     bool
}

type wfpFlowEntry struct {
	origDst   netip.AddrPort
	processID uint32
	createdAt time.Time
}

type wfpFlowTable struct {
	mu      sync.RWMutex
	entries map[wfpFlowKey]*wfpFlowEntry
}

func newWfpFlowTable() *wfpFlowTable {
	return &wfpFlowTable{
		entries: make(map[wfpFlowKey]*wfpFlowEntry),
	}
}

// Insert is called by the audit pump when a Decision=Redirect event
// arrives — the original destination is now known and can be served
// out of the in-memory cache without an extra IOCTL.
func (f *wfpFlowTable) Insert(localPort uint16, isUDP bool, origDst netip.AddrPort, pid uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[wfpFlowKey{localPort, isUDP}] = &wfpFlowEntry{
		origDst:   origDst,
		processID: pid,
		createdAt: time.Now(),
	}
}

// Lookup returns (origDst, pid, true) on hit or zeros + false on miss.
// Caller should fall back to IOCTL_NEXUS_WFP_GET_ORIG_DST on miss —
// the driver might have a flow we haven't yet seen audit-pump for
// (e.g. agent restart, audit channel back-pressure).
func (f *wfpFlowTable) Lookup(localPort uint16, isUDP bool) (netip.AddrPort, uint32, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.entries[wfpFlowKey{localPort, isUDP}]
	if !ok {
		return netip.AddrPort{}, 0, false
	}
	if time.Since(e.createdAt) > wfpFlowTableTTL {
		// Expired but not yet swept — treat as miss so the IOCTL
		// fallback fires with fresh data.
		return netip.AddrPort{}, 0, false
	}
	return e.origDst, e.processID, true
}

// Sweep evicts entries older than wfpFlowTableTTL. Called periodically
// (every 30s) by the auditPump goroutine.
func (f *wfpFlowTable) Sweep() (evicted int) {
	cutoff := time.Now().Add(-wfpFlowTableTTL)
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, e := range f.entries {
		if e.createdAt.Before(cutoff) {
			delete(f.entries, k)
			evicted++
		}
	}
	return evicted
}

// Size is for observability (metric).
func (f *wfpFlowTable) Size() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.entries)
}
