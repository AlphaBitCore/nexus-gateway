package access

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// IPAccessFilter evaluates source IPs against allowlists and blocklists.
// Applies to all endpoints and modalities via AnyEndpointAnyModality.
type IPAccessFilter struct {
	core.AnyEndpointAnyModality
	cfg       *core.HookConfig
	allowlist []*net.IPNet
	blocklist []*net.IPNet
	mode      string // "allowlist", "blocklist", or "both"
}

// NewIPAccessFilter constructs an IPAccessFilter from declarative config.
//
// Expected config shape:
//
//	{
//	  "allowlist": ["10.0.0.0/8"],
//	  "blocklist": ["10.0.0.99/32"],
//	  "mode": "allowlist|blocklist|both"
//	}
func NewIPAccessFilter(cfg *core.HookConfig) (core.Hook, error) {
	mode, _ := cfg.Config["mode"].(string)
	mode = strings.ToLower(mode)
	if mode == "" {
		mode = "blocklist"
	}
	if mode != "allowlist" && mode != "blocklist" && mode != "both" {
		return nil, fmt.Errorf("ip-access-filter: unknown mode %q", mode)
	}

	allowlist, err := parseCIDRs(cfg.Config["allowlist"])
	if err != nil {
		return nil, fmt.Errorf("ip-access-filter: allowlist: %w", err)
	}
	blocklist, err := parseCIDRs(cfg.Config["blocklist"])
	if err != nil {
		return nil, fmt.Errorf("ip-access-filter: blocklist: %w", err)
	}

	return &IPAccessFilter{
		cfg:       cfg,
		allowlist: allowlist,
		blocklist: blocklist,
		mode:      mode,
	}, nil
}

// Execute checks the source IP against configured allowlists and blocklists.
func (iaf *IPAccessFilter) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	result := &core.HookResult{
		HookID:           iaf.cfg.ID,
		ImplementationID: iaf.cfg.ImplementationID,
		HookName:         iaf.cfg.Name,
	}

	ip := net.ParseIP(input.SourceIP)
	if ip == nil {
		result.Decision = core.RejectHard
		result.Reason = fmt.Sprintf("invalid source IP: %s", input.SourceIP)
		result.ReasonCode = "IP_ACCESS_DENIED"
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	switch iaf.mode {
	case "blocklist":
		if matchesAny(ip, iaf.blocklist) {
			result.Decision = core.RejectHard
			result.Reason = fmt.Sprintf("source IP %s is blocklisted", input.SourceIP)
			result.ReasonCode = "IP_ACCESS_DENIED"
		} else {
			result.Decision = core.Approve
		}

	case "allowlist":
		if matchesAny(ip, iaf.allowlist) {
			result.Decision = core.Approve
		} else {
			result.Decision = core.RejectHard
			result.Reason = fmt.Sprintf("source IP %s is not in allowlist", input.SourceIP)
			result.ReasonCode = "IP_ACCESS_DENIED"
		}

	case "both":
		// Blocklist takes precedence in "both" mode.
		switch {
		case matchesAny(ip, iaf.blocklist):
			result.Decision = core.RejectHard
			result.Reason = fmt.Sprintf("source IP %s is blocklisted", input.SourceIP)
			result.ReasonCode = "IP_ACCESS_DENIED"
		case matchesAny(ip, iaf.allowlist):
			result.Decision = core.Approve
		default:
			result.Decision = core.RejectHard
			result.Reason = fmt.Sprintf("source IP %s is not in allowlist", input.SourceIP)
			result.ReasonCode = "IP_ACCESS_DENIED"
		}
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

// parseCIDRs extracts a list of CIDR networks from a config value.
func parseCIDRs(raw any) ([]*net.IPNet, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("expected an array of CIDR strings")
	}
	var nets []*net.IPNet
	for i, item := range list {
		cidr, _ := item.(string)
		if cidr == "" {
			return nil, fmt.Errorf("entry[%d] is empty", i)
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("entry[%d] %q: %w", i, cidr, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

// matchesAny returns true if the IP is contained in any of the given networks.
func matchesAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
