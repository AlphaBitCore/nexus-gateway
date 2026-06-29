//go:build windows

// wfp_windows_test.go — skeleton-level tests covering pieces that
// don't need a loaded driver: policy marshal/unmarshal round-trip,
// flowTable behaviour, audit-event parse boundary checks.
//
// SKELETON. See wfp_windows.go header for build-tag context. The
// integration tests that drive the actual driver live under the
// `wfpintegration` tag and run only on hosts with the driver loaded.

package windows

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

// encodeAuditEntry builds one 60-byte NexusFlowAuditEntry exactly as the
// kernel driver packs it (Common.h, #pragma pack(1)). Used to lock the Go
// parser's stride + field offsets against the C struct.
func encodeAuditEntry(ts uint64, pid, ppid uint32, family, proto, decision uint8,
	src netip.AddrPort, dst netip.AddrPort) []byte {
	b := make([]byte, flowAuditEntrySize)
	binary.LittleEndian.PutUint64(b[0:], ts)
	binary.LittleEndian.PutUint32(b[8:], pid)
	binary.LittleEndian.PutUint32(b[12:], ppid)
	b[16] = family
	b[17] = proto
	b[18] = decision
	// b[19] reserved
	copyAddr(b[20:36], src.Addr())
	binary.LittleEndian.PutUint16(b[36:], src.Port())
	copyAddr(b[40:56], dst.Addr())
	binary.LittleEndian.PutUint16(b[56:], dst.Port())
	return b
}

// copyAddr writes an IP into a 16-byte field the way the kernel does:
// IPv4 in the first 4 bytes (rest zero), IPv6 across all 16.
func copyAddr(dst []byte, a netip.Addr) {
	if a.Is4() {
		v := a.As4()
		copy(dst, v[:])
		return
	}
	v := a.As16()
	copy(dst, v[:])
}

func TestParseFlowAuditEntries(t *testing.T) {
	if flowAuditEntrySize != 60 {
		t.Fatalf("flowAuditEntrySize = %d, must be 60 to match the kernel "+
			"NexusFlowAuditEntry (Common.h #pragma pack(1))", flowAuditEntrySize)
	}

	r0src := netip.MustParseAddrPort("10.0.0.1:11111")
	r0dst := netip.MustParseAddrPort("93.184.216.34:443")
	r1src := netip.MustParseAddrPort("192.168.1.5:22222")
	r1dst := netip.MustParseAddrPort("1.2.3.4:8080")

	buf := append(
		encodeAuditEntry(0x1122334455667788, 1000, 2000, afInet, protoTCP,
			uint8(DecisionRedirect), r0src, r0dst),
		encodeAuditEntry(0x99AABBCCDDEEFF00, 3000, 4000, afInet, protoUDP,
			uint8(DecisionPermit), r1src, r1dst)...,
	)

	got := parseFlowAuditEntries(buf)
	if len(got) != 2 {
		t.Fatalf("parsed %d records, want 2 (a wrong stride drops the second)", len(got))
	}

	// Record 0.
	if got[0].TimestampUs != 0x1122334455667788 || got[0].ProcessID != 1000 ||
		got[0].ParentPID != 2000 || got[0].Protocol != protoTCP ||
		got[0].Decision != DecisionRedirect ||
		got[0].SrcAddr != r0src || got[0].OrigDstAddr != r0dst {
		t.Errorf("record0 mismatch: %+v", got[0])
	}
	// Record 1 — only parses correctly if the stride is exactly 60.
	if got[1].TimestampUs != 0x99AABBCCDDEEFF00 || got[1].ProcessID != 3000 ||
		got[1].ParentPID != 4000 || got[1].Protocol != protoUDP ||
		got[1].Decision != DecisionPermit ||
		got[1].SrcAddr != r1src || got[1].OrigDstAddr != r1dst {
		t.Errorf("record1 mismatch (stride bug?): %+v", got[1])
	}

	// A trailing partial record (fewer than 60 bytes) must be ignored.
	if n := len(parseFlowAuditEntries(buf[:len(buf)-1])); n != 1 {
		t.Errorf("partial-tail buffer parsed %d records, want 1", n)
	}
}

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
		QUICFallbackImages: []string{"chrome.exe", "msedge.exe"},
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
	if len(got.QUICFallbackImages) != len(p.QUICFallbackImages) {
		t.Fatalf("QUIC image count: got %d want %d", len(got.QUICFallbackImages), len(p.QUICFallbackImages))
	}
	for i := range p.QUICFallbackImages {
		if got.QUICFallbackImages[i] != p.QUICFallbackImages[i] {
			t.Errorf("QUICFallbackImages[%d]: got %q want %q", i, got.QUICFallbackImages[i], p.QUICFallbackImages[i])
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
	images := make([]string, maxQuicFallbackCount+1)
	for i := range images {
		images[i] = "chrome.exe"
	}
	_, err = MarshalPolicy(Policy{QUICFallbackImages: images})
	if err == nil {
		t.Fatal("expected error for too-many QUIC-fallback images")
	}
}

// TestMarshalPolicyQuicImageNormalization verifies that a mixed-case full
// path is serialised as the lowercase basename — the form the kernel's
// ALE_APP_ID basename match expects — and that the v2 header lays the
// quicFallbackCount at the right offset.
func TestMarshalPolicyQuicImageNormalization(t *testing.T) {
	p := Policy{QUICFallbackImages: []string{`C:\Program Files\Google\Chrome\Application\CHROME.EXE`}}
	buf, err := MarshalPolicy(p)
	if err != nil {
		t.Fatalf("MarshalPolicy: %v", err)
	}
	if v := binary.LittleEndian.Uint32(buf[0:]); v != protocolVersion {
		t.Errorf("protocol version on wire: got %d want %d", v, protocolVersion)
	}
	if c := binary.LittleEndian.Uint32(buf[20:]); c != 1 {
		t.Fatalf("quicFallbackCount at offset 20: got %d want 1", c)
	}
	got, err := UnmarshalPolicy(buf)
	if err != nil {
		t.Fatalf("UnmarshalPolicy: %v", err)
	}
	if len(got.QUICFallbackImages) != 1 || got.QUICFallbackImages[0] != "chrome.exe" {
		t.Errorf("normalized image: got %q want [chrome.exe]", got.QUICFallbackImages)
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
