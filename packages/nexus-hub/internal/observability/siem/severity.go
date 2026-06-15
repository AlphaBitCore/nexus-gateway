package siem

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// severity.go derives SIEM event severity from the CANONICAL event taxonomy the
// classifier actually emits — "<resource>.<verb>" for admin events (resource
// from iam.Catalog), "traffic.<signal>" for the compliance classifier, and the
// "auth.login_*" identities for login events — rather than from invented
// prefixes.
//
// The previous table branched on "iam." / "proxy." / "config." prefixes that
// ClassifyAdminEvent never produces (it emits "iam-policy.create",
// "kill-switch.toggle", "settings.update", "node.*"), so every IAM / kill-switch
// / settings / node mutation fell through to the lowest default severity — a SOC
// keying alert routing on severity would mis-prioritise a privilege escalation
// as noise. Keying on the resource's canonical IAM service (plus a few
// cross-service security-critical resources) keeps the mapping correct as the
// catalog grows.

// resourceService maps a canonical resource name (the part of an eventType
// before the first ".") to its IAM service, built once from iam.Catalog so the
// severity classifier tracks the single source of truth.
var resourceService = buildResourceServiceMap()

func buildResourceServiceMap() map[string]iam.Service {
	m := make(map[string]iam.Service, len(iam.Catalog))
	for i := range iam.Catalog {
		r := &iam.Catalog[i]
		m[r.Name] = r.Service
	}
	return m
}

// severityTier is an internal, format-agnostic risk level. cefSeverity and
// syslogSeverity each map a tier to their own numeric scale so the two stay in
// lockstep from one classification.
type severityTier int

const (
	tierInfo     severityTier = iota // routine / unknown
	tierLow                          // gateway/agent data-plane events
	tierModerate                     // compliance config, login success, blocked traffic
	tierElevated                     // identity/privilege/secret/platform writes
	tierHigh                         // authentication failure
	tierCritical                     // kill-switch / emergency passthrough toggles
)

// eventTier classifies an emitted eventType into a severity tier from the
// canonical taxonomy.
func eventTier(eventType string) severityTier {
	resource, sub, _ := strings.Cut(eventType, ".")
	switch resource {
	case "auth":
		// auth.login_failure (security-relevant) vs auth.login_success (audit).
		if sub == "login_failure" {
			return tierHigh
		}
		return tierModerate
	case "kill-switch", "passthrough":
		// Safety-critical: a kill-switch toggle or emergency-passthrough enable
		// must never be exported as routine noise.
		return tierCritical
	case "credential":
		// Secret-management resource lives under ServiceGateway in the catalog
		// but is privilege-grade — keep it elevated regardless of service.
		return tierElevated
	case "traffic":
		switch sub {
		case "request_blocked":
			return tierModerate
		case "rate_limited", "budget_exceeded":
			return tierLow
		default:
			return tierInfo // allowed / passthrough
		}
	}

	if svc, ok := resourceService[resource]; ok {
		switch svc {
		case iam.ServiceIAM, iam.ServicePlatform:
			return tierElevated
		case iam.ServiceCompliance:
			return tierModerate
		case iam.ServiceGateway, iam.ServiceAgent:
			return tierLow
		}
	}
	return tierInfo
}

// cefSeverity maps an emitted eventType to a CEF severity integer (0–10),
// derived from the canonical taxonomy.
func cefSeverity(eventType string) int {
	switch eventTier(eventType) {
	case tierCritical:
		return 9
	case tierHigh:
		return 7
	case tierElevated:
		return 6
	case tierModerate:
		return 5
	case tierLow:
		return 4
	default:
		return 3
	}
}

// syslogSeverity maps an emitted eventType to an RFC-5424 severity code (0
// emerg … 7 debug; lower is more severe), derived from the canonical taxonomy.
func syslogSeverity(eventType string) int {
	switch eventTier(eventType) {
	case tierCritical:
		return 2 // critical
	case tierHigh:
		return 4 // warning
	case tierElevated, tierModerate:
		return 5 // notice
	default:
		return 6 // info
	}
}
