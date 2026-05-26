package access

import (
	"net"
	"testing"
)

func TestIPAllowlist_Allow(t *testing.T) {
	al, err := NewIPAllowlist([]string{"10.0.0.0/8", "192.168.1.0/24"})
	if err != nil {
		t.Fatalf("NewIPAllowlist: %v", err)
	}

	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"in first range", "10.1.2.3", true},
		{"in second range", "192.168.1.100", true},
		{"out of range", "8.8.8.8", false},
		{"boundary start", "10.0.0.0", true},
		{"boundary end", "10.255.255.255", true},
		{"adjacent miss", "192.168.2.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("invalid test IP: %s", tt.ip)
			}
			if got := al.Allow(ip); got != tt.want {
				t.Errorf("Allow(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIPAllowlist_IPv6(t *testing.T) {
	al, err := NewIPAllowlist([]string{"2001:db8::/32"})
	if err != nil {
		t.Fatalf("NewIPAllowlist: %v", err)
	}

	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"in range", "2001:db8::1", true},
		{"in range deep", "2001:db8:abcd:1234::1", true},
		{"out of range", "2001:db9::1", false},
		{"IPv4 against IPv6 list", "10.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("invalid test IP: %s", tt.ip)
			}
			if got := al.Allow(ip); got != tt.want {
				t.Errorf("Allow(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIPAllowlist_Empty(t *testing.T) {
	al, err := NewIPAllowlist(nil)
	if err != nil {
		t.Fatalf("NewIPAllowlist: %v", err)
	}
	if al.Allow(net.ParseIP("10.0.0.1")) {
		t.Error("empty allowlist should deny all")
	}
	if al.Allow(net.ParseIP("8.8.8.8")) {
		t.Error("empty allowlist should deny all")
	}
}

func TestIPAllowlist_InvalidCIDR(t *testing.T) {
	_, err := NewIPAllowlist([]string{"not-a-cidr"})
	if err == nil {
		t.Error("expected error for invalid CIDR")
	}

	_, err = NewIPAllowlist([]string{"10.0.0.0/8", "bad"})
	if err == nil {
		t.Error("expected error for invalid CIDR in list")
	}
}
