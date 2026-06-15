//go:build linux

package linux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIPPortToHex_V4(t *testing.T) {
	// 127.0.0.1:443 → little-endian addr 0100007F, port 01BB.
	if got := ipPortToHex("127.0.0.1", 443); got != "0100007F:01BB" {
		t.Fatalf("v4 = %q, want 0100007F:01BB", got)
	}
	if got := ipPortToHex("not-an-ip", 80); got != "" {
		t.Fatalf("invalid IP should yield empty, got %q", got)
	}
}

func TestIPPortToHex_V6(t *testing.T) {
	// ::1 (loopback) → 32 hex digits, last word little-endian = 01000000.
	got := ipPortToHex("::1", 443)
	if len(got) != 32+1+4 { // 32 addr + ':' + 4 port
		t.Fatalf("v6 hex wrong length: %q", got)
	}
	if got[33:] != "01BB" {
		t.Fatalf("v6 port wrong: %q", got)
	}
	// Final word of ::1 is 0x00000001 → little-endian bytes → "01000000".
	if got[24:32] != "01000000" {
		t.Fatalf("v6 low word = %q, want 01000000 (full=%q)", got[24:32], got)
	}
}

func TestAddrHexMatch(t *testing.T) {
	v4 := "0100007F:01BB"
	// Exact v4 match.
	if !addrHexMatch("0100007F:01BB", v4) {
		t.Error("exact v4 should match")
	}
	// Port mismatch.
	if addrHexMatch("0100007F:0050", v4) {
		t.Error("port mismatch must not match")
	}
	// v4-mapped in a tcp6 column (32-hex addr, v4 octets in the low word).
	mapped := "0000000000000000FFFF00000100007F:01BB"
	if !addrHexMatch(mapped, v4) {
		t.Error("v4-mapped tcp6 column should suffix-match v4")
	}
	// Different low word → no suffix match.
	if addrHexMatch("0000000000000000FFFF0000DEADBEEF:01BB", v4) {
		t.Error("different v4-mapped addr must not match")
	}
	// Malformed columns.
	if addrHexMatch("nocolon", v4) || addrHexMatch("0100007F:01BB", "nocolon") {
		t.Error("malformed input must not match")
	}
}

func TestScanProcNetTCP_MatchesLocalAndRemote(t *testing.T) {
	dir := t.TempDir()
	procFile := filepath.Join(dir, "tcp")
	// header + one non-matching + one matching row. Column indices mirror
	// /proc/net/tcp: [1]=local [2]=remote [9]=inode.
	content := "" +
		"  sl  local_address rem_address   st ... uid timeout inode\n" +
		"   0: 0100007F:0050 00000000:0000 0A 0 0 0 0 0 11111\n" +
		"   1: 0100007F:01BB DEADBEEF:1F90 01 0 0 0 0 0 22222\n"
	if err := os.WriteFile(procFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Match local only.
	if inode := scanProcNetTCP(procFile, "0100007F:01BB", ""); inode != "22222" {
		t.Fatalf("local-only scan inode=%q want 22222", inode)
	}
	// Match local + remote.
	if inode := scanProcNetTCP(procFile, "0100007F:01BB", "DEADBEEF:1F90"); inode != "22222" {
		t.Fatalf("local+remote scan inode=%q want 22222", inode)
	}
	// Remote mismatch → no match.
	if inode := scanProcNetTCP(procFile, "0100007F:01BB", "11111111:0001"); inode != "" {
		t.Fatalf("remote-mismatch scan inode=%q want empty", inode)
	}
	// Missing file → empty, no panic.
	if inode := scanProcNetTCP(filepath.Join(dir, "absent"), "0100007F:01BB", ""); inode != "" {
		t.Fatalf("missing file inode=%q want empty", inode)
	}
}

func TestFindSocketInode_EmptyOnUnparseableLocal(t *testing.T) {
	if inode := findSocketInode("not-an-ip", 0, "", 0); inode != "" {
		t.Fatalf("unparseable local IP must yield empty inode, got %q", inode)
	}
}
