// Package domain holds UI-facing product taxonomy that differs from internal
// service names written into the database. The traffic_event.source column
// stores internal binary names (ai-gateway, compliance-proxy, agent) while
// the admin UI and API expose product-level domains (vk, proxy, agent).
// Keep mappings here so handlers, stores, and jobs share one source of truth.
package domain

import "strings"

// TrafficDomain is the UI/API-level taxonomy for data-plane traffic.
// Stable string values — also used as SubDimension tokens in rollup metrics
// (e.g. "source=vk").
type TrafficDomain string

const (
	// DomainVK covers traffic served by ai-gateway (virtual-key-authenticated
	// AI traffic).
	DomainVK TrafficDomain = "vk"
	// DomainProxy covers traffic processed by compliance-proxy (transparent
	// TLS interception + compliance pipeline).
	DomainProxy TrafficDomain = "proxy"
	// DomainAgent covers traffic observed by the desktop Agent.
	DomainAgent TrafficDomain = "agent"
)

// DB source values written into traffic_event.source by each data-plane
// writer. These must stay in sync with:
//   - packages/nexus-hub/internal/observability/consumer/traffic.go (MQ → DB writer)
//   - packages/compliance-proxy/internal/audit/sql.go (fallback writer)
//
// CHECK constraint on traffic_event.source pins the allowed set.
const (
	DBSourceAIGateway       = "ai-gateway"
	DBSourceComplianceProxy = "compliance-proxy"
	DBSourceAgent           = "agent"
)

// ParseTrafficDomain validates and parses a UI-supplied domain string.
// Returns ("", false) for empty input or unknown values.
func ParseTrafficDomain(s string) (TrafficDomain, bool) {
	switch TrafficDomain(strings.TrimSpace(s)) {
	case DomainVK:
		return DomainVK, true
	case DomainProxy:
		return DomainProxy, true
	case DomainAgent:
		return DomainAgent, true
	default:
		return "", false
	}
}

// DBSourcesFor returns the DB source values that belong to a domain.
// Today each domain maps to a single DB source, but the slice shape keeps
// the contract open for future fan-out (e.g. vk splitting into vk-public /
// vk-private without UI churn).
func DBSourcesFor(d TrafficDomain) []string {
	switch d {
	case DomainVK:
		return []string{DBSourceAIGateway}
	case DomainProxy:
		return []string{DBSourceComplianceProxy}
	case DomainAgent:
		return []string{DBSourceAgent}
	default:
		return nil
	}
}

// AllDataPlaneDBSources returns every DB source across every domain.
// Used as the default WHERE filter when the caller wants "all traffic".
func AllDataPlaneDBSources() []string {
	return []string{DBSourceAIGateway, DBSourceComplianceProxy, DBSourceAgent}
}

// AllDomains lists every supported traffic domain in UI order.
func AllDomains() []TrafficDomain {
	return []TrafficDomain{DomainVK, DomainProxy, DomainAgent}
}

// DBSourceToDomain maps a DB source value back to its domain. Returns
// ("", false) for unknown values (e.g. legacy admin / device-lifecycle rows
// that may linger until CHECK constraint cleanup finishes).
func DBSourceToDomain(dbSource string) (TrafficDomain, bool) {
	switch dbSource {
	case DBSourceAIGateway:
		return DomainVK, true
	case DBSourceComplianceProxy:
		return DomainProxy, true
	case DBSourceAgent:
		return DomainAgent, true
	default:
		return "", false
	}
}
