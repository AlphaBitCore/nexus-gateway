// Package iam implements the IAM policy evaluation engine for the control-plane
// admin API: NRN resource names, condition evaluation, and policy matching.
package iam

import (
	"strings"

	sharediam "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

const nrnPrefix = "nrn:nexus:"

// NRNComponents holds the parsed segments of a Nexus Resource Name.
type NRNComponents struct {
	Service      string // gateway | admin
	Scope        string // org-id, org-id/dept, or * for global
	ResourceType string // provider, model, policy, hook, etc.
	ResourceID   string // specific ID, slug, or * for wildcard
}

// BuildNRN constructs an NRN string from components.
func BuildNRN(service, scope, resourceType, resourceID string) string {
	return nrnPrefix + service + ":" + scope + ":" + resourceType + "/" + resourceID
}

// BuildRequestNRNForAction derives the canonical request-side NRN for the given
// admin action by extracting (service, resourceType) from the catalog. For
// canonical actions of the form "admin:<resource>.<verb>" this returns
// "nrn:nexus:<service>:*:<resource>/*" so that policies scoped to a specific
// resource type match exactly. Non-canonical actions fall back to a
// fully-wildcarded NRN so a literal Resource: "*" still authorises them.
// Used by the iamauth middleware and GetMePermissions.
func BuildRequestNRNForAction(action string) string {
	service := "*"
	resourceType := "*"
	if resource, _, ok := sharediam.ParseAction(action); ok {
		resourceType = resource
		if svc, ok := sharediam.ServiceForAction(action); ok {
			service = string(svc)
		}
	}
	return BuildNRN(service, "*", resourceType, "*")
}

// BuildDeviceCandidateNRNs returns the full set of candidate request
// NRNs that should be evaluated for an action targeting a specific
// agent-device. The list always includes the unscoped resource (so
// fleet-wide grants continue to work), plus one group-scoped resource
// per group the device belongs to (so a policy with Resource
// `agent-device/group:<id>/*` matches just for member devices).
//
// Group-scoped path segment shape:
//
//	nrn:nexus:agent:*:agent-device/group:<group-id>/<device-id>
//
// `group:` is a literal prefix so the resourceID path can be
// distinguished from a bare device id at parse time. MatchNRN's
// matchSegment treats the whole path-component as one segment, so the
// pattern `agent-device/group:abc/*` matches `agent-device/group:abc/<dev>`
// via the existing globMatch fallback — no NRN grammar change needed.
//
// deviceGroupIDs may be empty when the device isn't in any group; in
// that case only the unscoped candidate is returned. The unscoped form
// is unconditionally included so super-admin's `admin:*` and any
// wildcard `agent-device/*` policy continue to work.
func BuildDeviceCandidateNRNs(action, deviceID string, deviceGroupIDs []string) []string {
	service := "*"
	resourceType := "*"
	if resource, _, ok := sharediam.ParseAction(action); ok {
		resourceType = resource
		if svc, ok := sharediam.ServiceForAction(action); ok {
			service = string(svc)
		}
	}
	out := make([]string, 0, len(deviceGroupIDs)+1)
	// Unscoped candidate first — most policies are unscoped and we want
	// the canonical Resource: ["...agent-device/*"] form to short-circuit.
	out = append(out, BuildNRN(service, "*", resourceType, deviceID))
	for _, g := range deviceGroupIDs {
		if g == "" {
			continue
		}
		out = append(out, BuildNRN(service, "*", resourceType, "group:"+g+"/"+deviceID))
	}
	return out
}

// ParseNRN parses an NRN string into components. Returns nil if invalid.
func ParseNRN(nrn string) *NRNComponents {
	if !strings.HasPrefix(nrn, nrnPrefix) {
		return nil
	}
	rest := nrn[len(nrnPrefix):]

	firstColon := strings.Index(rest, ":")
	if firstColon == -1 {
		return nil
	}
	service := rest[:firstColon]
	afterService := rest[firstColon+1:]

	lastColon := strings.Index(afterService, ":")
	if lastColon == -1 {
		return nil
	}
	scope := afterService[:lastColon]
	resourcePart := afterService[lastColon+1:]

	slashIdx := strings.Index(resourcePart, "/")
	if slashIdx == -1 {
		return nil
	}
	resourceType := resourcePart[:slashIdx]
	resourceID := resourcePart[slashIdx+1:]

	if service == "" || resourceType == "" {
		return nil
	}
	return &NRNComponents{
		Service:      service,
		Scope:        scope,
		ResourceType: resourceType,
		ResourceID:   resourceID,
	}
}

// MatchNRN checks if a pattern NRN matches a target NRN.
// Supports wildcards (*) in any segment and hierarchical scope matching.
func MatchNRN(pattern, target string) bool {
	p := ParseNRN(pattern)
	t := ParseNRN(target)
	if p == nil || t == nil {
		return false
	}
	if !matchSegment(p.Service, t.Service) {
		return false
	}
	if !matchScope(p.Scope, t.Scope) {
		return false
	}
	if !matchSegment(p.ResourceType, t.ResourceType) {
		return false
	}
	if !matchSegment(p.ResourceID, t.ResourceID) {
		return false
	}
	return true
}

// matchSegment matches a single segment with glob wildcard support.
// "*" matches anything. "gpt-*" matches "gpt-4o".
func matchSegment(pattern, target string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == target
	}
	return globMatch(pattern, target)
}

// matchScope matches scope with hierarchical inheritance.
// Pattern "org-acme" matches "org-acme" and "org-acme/engineering".
func matchScope(pattern, target string) bool {
	if pattern == "*" {
		return true
	}
	if pattern == target {
		return true
	}
	if strings.HasPrefix(target, pattern+"/") {
		return true
	}
	return false
}

// globMatch implements safe segment-based glob matching (no regex).
func globMatch(pattern, target string) bool {
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		var idx int
		if i == 0 {
			if strings.HasPrefix(target, part) {
				idx = 0
			} else {
				return false
			}
		} else {
			idx = strings.Index(target[pos:], part)
			if idx == -1 {
				return false
			}
			idx += pos
		}
		pos = idx + len(part)
	}
	if last := parts[len(parts)-1]; last != "" {
		return strings.HasSuffix(target, last)
	}
	return true
}
