// Package wiring — Agent-side normalize.Registry construction.
// Delegates to shared.normalize.BuildRegistry so agent, compliance-proxy,
// ai-gateway, and Hub agent_audit all run the exact same Tier 1+2+3
// chain. See packages/shared/transport/normalize/buildregistry.go.
package wiring

import (
	sharednormalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// InitNormalizeRegistry returns the shared Tier 1+2+3 Registry.
func InitNormalizeRegistry() *normalizecore.Registry {
	return sharednormalize.BuildRegistry()
}
