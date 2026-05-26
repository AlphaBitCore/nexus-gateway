package access

import (
	"fmt"
	"net"
)

// IPAllowlist checks if a source IP is in the allowed CIDR ranges.
type IPAllowlist struct {
	nets []*net.IPNet
}

// NewIPAllowlist parses CIDR strings and creates an allowlist.
func NewIPAllowlist(cidrs []string) (*IPAllowlist, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		nets = append(nets, ipNet)
	}
	return &IPAllowlist{nets: nets}, nil
}

// Allow returns true if the IP is in any allowed range.
// Returns false if the allowlist is empty (deny-all when unconfigured).
func (a *IPAllowlist) Allow(ip net.IP) bool {
	if len(a.nets) == 0 {
		return false
	}
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
