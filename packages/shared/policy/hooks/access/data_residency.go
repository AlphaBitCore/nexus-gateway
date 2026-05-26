package access

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// residencyPolicy maps a normalized lowercase severity string to a set of
// allowed provider regions. Severity values correspond to the
// severity:<name> tag emitted by upstream hooks (e.g. pii-detector emits
// "severity:confidential").
type residencyPolicy struct {
	severity       string // "public" | "internal" | "confidential" | "restricted"
	allowedRegions map[string]bool
}

// dataResidencyHook is the data-residency hook implementation.
// Applies to all endpoints and modalities via AnyEndpointAnyModality.
type dataResidencyHook struct {
	core.AnyEndpointAnyModality
	cfg      *core.HookConfig
	policies map[string]*residencyPolicy // key: normalized lowercase severity
}

// NewDataResidency constructs a data-residency hook from declarative config.
// The "classification" field is written in uppercase in stored configs;
// it is normalized to lowercase at load time to match the severity: tag
// convention.
//
//	{
//	  "policies": [
//	    {"classification": "CONFIDENTIAL", "allowedRegions": ["eu-west-1", "eu-central-1"]},
//	    {"classification": "RESTRICTED",   "allowedRegions": ["eu-west-1"]}
//	  ]
//	}
func NewDataResidency(cfg *core.HookConfig) (core.Hook, error) {
	policies := make(map[string]*residencyPolicy)

	rawPolicies, ok := cfg.Config["policies"]
	if !ok {
		return &dataResidencyHook{cfg: cfg, policies: policies}, nil
	}
	policyList, ok := rawPolicies.([]any)
	if !ok {
		return nil, fmt.Errorf("data-residency: 'policies' must be an array")
	}

	for i, raw := range policyList {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("data-residency: policies[%d] must be an object", i)
		}
		classStr, _ := m["classification"].(string)
		if classStr == "" {
			return nil, fmt.Errorf("data-residency: policies[%d] missing 'classification'", i)
		}
		severity := strings.ToLower(classStr)

		rawRegions, ok := m["allowedRegions"]
		if !ok {
			return nil, fmt.Errorf("data-residency: policies[%d] missing 'allowedRegions'", i)
		}
		regionList, ok := rawRegions.([]any)
		if !ok {
			return nil, fmt.Errorf("data-residency: policies[%d] 'allowedRegions' must be an array", i)
		}

		regions := make(map[string]bool, len(regionList))
		for _, r := range regionList {
			if s, ok := r.(string); ok {
				regions[strings.ToLower(s)] = true
			}
		}

		policies[severity] = &residencyPolicy{
			severity:       severity,
			allowedRegions: regions,
		}
	}

	return &dataResidencyHook{cfg: cfg, policies: policies}, nil
}

// highestUpstreamSeverity scans input.UpstreamTags for "severity:<name>"
// tags and returns the highest-ranked severity name in lowercase form (e.g.
// "confidential"), or "" when no severity tag is present. Wraps the
// package-level HighestSeverityTag helper and strips the "severity:" prefix
// so callers can compare directly against the lowercase policy keys.
func highestUpstreamSeverity(upstream []string) string {
	best := core.HighestSeverityTag(upstream)
	if best == "" {
		return ""
	}
	return strings.TrimPrefix(best, "severity:")
}

func (h *dataResidencyHook) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	result := &core.HookResult{
		HookID:           h.cfg.ID,
		ImplementationID: h.cfg.ImplementationID,
		HookName:         h.cfg.Name,
		Decision:         core.Approve,
	}

	severity := highestUpstreamSeverity(input.UpstreamTags)
	if severity == "" || severity == "public" {
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	policy, exists := h.policies[severity]
	if !exists {
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	providerRegion := strings.ToLower(input.ProviderRegion)
	if providerRegion == "" {
		result.Decision = core.RejectHard
		result.Reason = fmt.Sprintf("Data classified as %s cannot be sent to a provider with unknown region", severity)
		result.ReasonCode = "DATA_RESIDENCY_UNKNOWN_REGION"
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	if !policy.allowedRegions[providerRegion] {
		result.Decision = core.RejectHard
		result.Reason = fmt.Sprintf(
			"Data classified as %s cannot be sent to region %q (allowed: %s)",
			severity, providerRegion, joinRegions(policy.allowedRegions),
		)
		result.ReasonCode = "DATA_RESIDENCY_VIOLATION"
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}

func joinRegions(regions map[string]bool) string {
	result := make([]string, 0, len(regions))
	for r := range regions {
		result = append(result, r)
	}
	return strings.Join(result, ", ")
}
