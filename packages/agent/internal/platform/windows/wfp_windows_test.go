//go:build windows

// wfp_windows_test.go — skeleton-level tests covering pieces that
// don't need a loaded driver: policy marshal/unmarshal round-trip,
// flowTable behaviour, audit-event parse boundary checks.
//
// SKELETON. See wfp_windows.go header for build-tag context. The
// integration tests that drive the actual driver live under the
// `wfpintegration` tag and run only on hosts with the driver loaded
// (E59-S6 territory).

package windows

import (
	"net/netip"
	"testing"
	"time"
)

func TestPolicyRoundTrip(t *testing.T) {
	p := Policy{
		Generation: 42,
		KillSwitch: true,
		BypassPIDs: []uint32{1234, 5678, 9000},
		BypassCIDRs: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("192.168.1.0/24"),
			netip.MustParsePrefix("fe80::/10"),
		},
	}
	buf, err := MarshalPolicy(p)
	if err != nil {
		t.Fatalf("MarshalPolicy: %v", err)
	}
	got, err := UnmarshalPolicy(buf)
	if err != nil {
		t.Fatalf("UnmarshalPolicy: %v", err)
	}
	if got.Generation != p.Generation {
		t.Errorf("Generation: got %d want %d", got.Generation, p.Generation)
	}
	if got.KillSwitch != p.KillSwitch {
		t.Errorf("KillSwitch: got %v want %v", got.KillSwitch, p.KillSwitch)
	}
	if len(got.BypassPIDs) != len(p.BypassPIDs) {
		t.Fatalf("PID count: got %d want %d", len(got.BypassPIDs), len(p.BypassPIDs))
	}
	for i := range p.BypassPIDs {
		if got.BypassPIDs[i] != p.BypassPIDs[i] {
			t.Errorf("PID[%d]: got %d want %d", i, got.BypassPIDs[i], p.BypassPIDs[i])
		}
	}
	if len(got.BypassCIDRs) != len(p.BypassCIDRs) {
		t.Fatalf("CIDR count: got %d want %d", len(got.BypassCIDRs), len(p.BypassCIDRs))
	}
	for i := range p.BypassCIDRs {
		if got.BypassCIDRs[i] != p.BypassCIDRs[i] {
			t.Errorf("CIDR[%d]: got %v want %v", i, got.BypassCIDRs[i], p.BypassCIDRs[i])
		}
	}
}

func TestPolicyTooMany(t *testing.T) {
	pids := make([]uint32, maxProcessBypassCount+1)
	_, err := MarshalPolicy(Policy{BypassPIDs: pids})
	if err == nil {
		t.Fatal("expected error for too-many PIDs")
	}
	cidrs := make([]netip.Prefix, maxDestBypassCount+1)
	for i := range cidrs {
		cidrs[i] = netip.MustParsePrefix("10.0.0.0/8")
	}
	_, err = MarshalPolicy(Policy{BypassCIDRs: cidrs})
	if err == nil {
		t.Fatal("expected error for too-many CIDRs")
	}
}

func TestFlowTableInsertLookup(t *testing.T) {
	tbl := newWfpFlowTable()
	orig := netip.MustParseAddrPort("10.0.0.5:443")
	tbl.Insert(54321, false, orig, 1234)

	got, pid, ok := tbl.Lookup(54321, false)
	if !ok {
		t.Fatal("expected hit")
	}
	if got != orig {
		t.Errorf("origDst: got %v want %v", got, orig)
	}
	if pid != 1234 {
		t.Errorf("pid: got %d want 1234", pid)
	}

	if _, _, ok := tbl.Lookup(54321, true); ok {
		t.Errorf("UDP lookup should miss for TCP entry")
	}
	if _, _, ok := tbl.Lookup(12345, false); ok {
		t.Errorf("wrong-port lookup should miss")
	}
}

func TestFlowTableTTL(t *testing.T) {
	tbl := newWfpFlowTable()
	tbl.entries[wfpFlowKey{localPort: 100, isUDP: false}] = &wfpFlowEntry{
		origDst:   netip.MustParseAddrPort("1.1.1.1:1"),
		processID: 1,
		createdAt: time.Now().Add(-wfpFlowTableTTL - time.Second), // expired
	}
	if _, _, ok := tbl.Lookup(100, false); ok {
		t.Fatal("expected expired entry to miss")
	}
	if evicted := tbl.Sweep(); evicted != 1 {
		t.Errorf("Sweep evicted=%d want 1", evicted)
	}
}

func TestCtlCode(t *testing.T) {
	// Lock the IOCTL codes against accidental refactor — these MUST
	// match the C macros in nexus-wfp-driver/Common.h forever (or be
	// bumped with NEXUS_WFP_PROTOCOL_VERSION).
	cases := []struct {
		name string
		got  uint32
		want uint32
	}{
		// CTL_CODE(0x12, 0x800, 0, 0) =
		//   (0x12 << 16) | (0 << 14) | (0x800 << 2) | 0 = 0x122000
		{"HELLO", ioctlNexusWfpHello, 0x00122000},
		// CTL_CODE(0x12, 0x801, 0, 0) = 0x00122004
		{"SET_PROXY_PORT", ioctlNexusWfpSetProxyPort, 0x00122004},
		// CTL_CODE(0x12, 0x802, 0, 0) = 0x00122008
		{"PUSH_POLICY", ioctlNexusWfpPushPolicy, 0x00122008},
		// CTL_CODE(0x12, 0x803, 0, 0) = 0x0012200C
		{"GET_ORIG_DST", ioctlNexusWfpGetOrigDst, 0x0012200C},
		// CTL_CODE(0x12, 0x804, 2 /*OUT_DIRECT*/, 0) = 0x00122012
		{"AUDIT_PUMP", ioctlNexusWfpAuditPump, 0x00122012},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got 0x%X want 0x%X", c.name, c.got, c.want)
		}
	}
}
