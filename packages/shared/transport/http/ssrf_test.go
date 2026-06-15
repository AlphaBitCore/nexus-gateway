package http

import (
	"net"
	"strings"
	"testing"
)

// TestIsDisallowedIP_RangeMatrix is the SSRF deny-list unit matrix: every
// non-public range the guard must block returns true; representative public
// unicast addresses return false. This is the authoritative contract for the
// shared deny-list consumed by every outbound-to-untrusted-URL call site.
func TestIsDisallowedIP_RangeMatrix(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},             // loopback
		{"127.5.5.5", true},             // loopback /8
		{"::1", true},                   // IPv6 loopback
		{"10.0.0.1", true},              // RFC-1918
		{"10.255.255.255", true},        // RFC-1918
		{"172.16.0.1", true},            // RFC-1918
		{"172.31.255.1", true},          // RFC-1918
		{"192.168.1.1", true},           // RFC-1918
		{"169.254.1.1", true},           // link-local
		{"169.254.169.254", true},       // cloud metadata
		{"fe80::1", true},               // IPv6 link-local
		{"fc00::1", true},               // IPv6 ULA (private)
		{"ff02::1", true},               // IPv6 link-local multicast
		{"0.0.0.0", true},               // unspecified
		{"::", true},                    // IPv6 unspecified
		{"224.0.0.1", true},             // multicast
		{"8.8.8.8", false},              // public
		{"1.1.1.1", false},              // public
		{"172.32.0.1", false},           // just outside 172.16/12
		{"203.0.113.10", false},         // public (TEST-NET-3, not in a blocked range)
		{"2606:4700:4700::1111", false}, // public IPv6 (Cloudflare)
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("test bug: %q is not a valid IP", c.ip)
		}
		if got := IsDisallowedIP(ip); got != c.want {
			t.Errorf("IsDisallowedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// TestBlockPrivateDialControl_PrivateAddresses proves the dial-time hook rejects
// every non-public IP range and admits public ones. Because the hook runs on the
// concrete already-resolved address, it is the authoritative DNS-rebinding guard.
func TestBlockPrivateDialControl_PrivateAddresses(t *testing.T) {
	cases := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"loopback 127.0.0.1", "127.0.0.1:443", true},
		{"loopback 127.5.5.5", "127.5.5.5:443", true},
		{"RFC-1918 10.x", "10.0.0.1:443", true},
		{"RFC-1918 192.168.x", "192.168.1.1:443", true},
		{"link-local cloud-meta", "169.254.169.254:80", true},
		{"IPv6 ULA", "[fc00::1]:443", true},
		{"public 1.1.1.1", "1.1.1.1:443", false},
		{"public 8.8.8.8", "8.8.8.8:53", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := BlockPrivateDialControl("tcp", tc.address, nil)
			if tc.wantErr && err == nil {
				t.Errorf("BlockPrivateDialControl(%q) = nil; want SSRF error", tc.address)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("BlockPrivateDialControl(%q) = %v; want nil (public)", tc.address, err)
			}
		})
	}
}

// TestBlockPrivateDialControl_BadAddress proves a non-parseable address returns
// a parse error — the hook must never silently admit an address it cannot read.
func TestBlockPrivateDialControl_BadAddress(t *testing.T) {
	for _, addr := range []string{"badaddress", "bad:bad:bad"} {
		t.Run(addr, func(t *testing.T) {
			if err := BlockPrivateDialControl("tcp", addr, nil); err == nil {
				t.Errorf("BlockPrivateDialControl(%q) = nil; want parse error", addr)
			}
		})
	}
}

// TestBlockPrivateDialControl_NonIPHost proves that a host:port that parses but
// whose host is not a numeric IP reaches the "is not an IP" error arm (defence
// in depth — Control hooks normally receive resolved addresses).
func TestBlockPrivateDialControl_NonIPHost(t *testing.T) {
	err := BlockPrivateDialControl("tcp", "notanip:443", nil)
	if err == nil {
		t.Fatal("BlockPrivateDialControl with non-IP host must error")
	}
	if !strings.Contains(err.Error(), "not an IP") {
		t.Errorf("error = %q; want it to mention the host is not an IP", err)
	}
}

// TestIsMetadataOrLinkLocalIP_RangeMatrix proves the metadata-only deny-set
// blocks every cloud-metadata / link-local address (the range an admin-URL probe
// must never reach) while ADMITTING RFC-1918 / loopback — the on-prem
// self-hosted-provider use case the provider probe depends on.
func TestIsMetadataOrLinkLocalIP_RangeMatrix(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"169.254.169.254", true},        // AWS/GCP/Azure IMDS
		{"169.254.1.1", true},            // broader link-local /16
		{"fe80::1", true},                // IPv6 link-local
		{"ff02::1", true},                // IPv6 link-local multicast
		{"::ffff:169.254.169.254", true}, // v4-mapped IMDS
		{"0.0.0.0", true},                // unspecified
		{"::", true},                     // IPv6 unspecified
		{"10.0.0.1", false},              // RFC-1918 — on-prem provider OK
		{"127.0.0.1", false},             // loopback — on-prem provider OK
		{"192.168.1.1", false},           // RFC-1918 — on-prem provider OK
		{"172.16.0.1", false},            // RFC-1918 — on-prem provider OK
		{"fd00:ec2::254", true},          // AWS IMDS over IPv6 (ULA, blocked explicitly)
		{"fc00::1", false},               // IPv6 ULA — on-prem provider OK
		{"8.8.8.8", false},               // public
		{"2606:4700:4700::1111", false},  // public IPv6
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("test bug: %q is not a valid IP", c.ip)
		}
		if got := IsMetadataOrLinkLocalIP(ip); got != c.want {
			t.Errorf("IsMetadataOrLinkLocalIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// TestBlockMetadataDialControl proves the provider-probe dial guard refuses the
// metadata / link-local range at dial time but admits RFC-1918 / loopback (so an
// on-prem vLLM/Ollama probe still connects) and public addresses.
func TestBlockMetadataDialControl(t *testing.T) {
	cases := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"cloud-meta 169.254.169.254", "169.254.169.254:80", true},
		{"link-local 169.254.x", "169.254.1.1:443", true},
		{"IPv6 link-local", "[fe80::1]:443", true},
		{"v4-mapped IMDS", "[::ffff:169.254.169.254]:80", true},
		{"on-prem loopback", "127.0.0.1:11434", false}, // Ollama default port
		{"on-prem RFC-1918 10.x", "10.0.0.5:8000", false},
		{"on-prem RFC-1918 192.168.x", "192.168.1.50:8000", false},
		{"public 1.1.1.1", "1.1.1.1:443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := BlockMetadataDialControl("tcp", tc.address, nil)
			if tc.wantErr && err == nil {
				t.Errorf("BlockMetadataDialControl(%q) = nil; want SSRF error", tc.address)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("BlockMetadataDialControl(%q) = %v; want nil", tc.address, err)
			}
		})
	}
}

// TestBlockMetadataDialControl_BadAddress proves the metadata guard shares the
// same parse-fail / non-IP defence as the private guard.
func TestBlockMetadataDialControl_BadAddress(t *testing.T) {
	if err := BlockMetadataDialControl("tcp", "badaddress", nil); err == nil {
		t.Error("BlockMetadataDialControl(bad) = nil; want parse error")
	}
	err := BlockMetadataDialControl("tcp", "notanip:443", nil)
	if err == nil || !strings.Contains(err.Error(), "not an IP") {
		t.Errorf("BlockMetadataDialControl(non-IP) = %v; want not-an-IP error", err)
	}
}

// TestAdminEgressDialControl_PolicyMatrix proves the single chokepoint hands back
// the correct guard per egress class: AdminEgressExternalOnly blocks all
// non-public ranges (loopback/RFC-1918 included), AdminEgressAllowPrivate blocks
// ONLY the metadata range and lets RFC-1918 / loopback through. Both block the
// metadata endpoint — the invariant that must hold on EVERY admin-URL egress.
func TestAdminEgressDialControl_PolicyMatrix(t *testing.T) {
	cases := []struct {
		name    string
		kind    AdminEgressKind
		address string
		wantErr bool
	}{
		// External-only: private + loopback + metadata all blocked.
		{"external blocks loopback", AdminEgressExternalOnly, "127.0.0.1:443", true},
		{"external blocks RFC-1918", AdminEgressExternalOnly, "10.0.0.1:443", true},
		{"external blocks metadata", AdminEgressExternalOnly, "169.254.169.254:80", true},
		{"external allows public", AdminEgressExternalOnly, "8.8.8.8:443", false},
		// Allow-private: metadata blocked, on-prem private allowed.
		{"allowprivate blocks metadata", AdminEgressAllowPrivate, "169.254.169.254:80", true},
		{"allowprivate allows loopback", AdminEgressAllowPrivate, "127.0.0.1:11434", false},
		{"allowprivate allows RFC-1918", AdminEgressAllowPrivate, "10.0.0.5:8000", false},
		{"allowprivate allows public", AdminEgressAllowPrivate, "1.1.1.1:443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := AdminEgressDialControl(tc.kind)
			if guard == nil {
				t.Fatal("AdminEgressDialControl returned nil guard")
			}
			err := guard("tcp", tc.address, nil)
			if tc.wantErr && err == nil {
				t.Errorf("guard(%q) = nil; want SSRF error", tc.address)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("guard(%q) = %v; want nil", tc.address, err)
			}
		})
	}
}
