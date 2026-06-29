package pipeline

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// matchesIngress checks whether a hook config applies to the given ingress type.
// Semantics: if any entry in ApplicableIngress matches the current ingressType,
// return true.
//
// Named aliases:
//   - "ALL"                   → matches every ingress type
//   - "AI_GATEWAY"        → matches "AI_GATEWAY" only
//   - "COMPLIANCE_PROXY"  → matches "COMPLIANCE_PROXY" only
//   - "AGENT"             → matches "AGENT" only
//
// Any other value is matched case-insensitively against the ingressType.
func (r *PolicyResolver) matchesIngress(cfg *core.HookConfig, ingressType string) bool {
	if len(cfg.ApplicableIngress) == 0 {
		return true
	}

	for _, ing := range cfg.ApplicableIngress {
		upper := strings.ToUpper(ing)
		if upper == "ALL" {
			return true
		}
		if upper == "AI_GATEWAY" && strings.EqualFold(ingressType, "AI_GATEWAY") {
			return true
		}
		if upper == "COMPLIANCE_PROXY" && strings.EqualFold(ingressType, "COMPLIANCE_PROXY") {
			return true
		}
		if upper == "AGENT" && strings.EqualFold(ingressType, "AGENT") {
			return true
		}
		if strings.EqualFold(upper, ingressType) {
			return true
		}
	}
	return false
}
