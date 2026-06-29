package wiring

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/audit"
	sharednormalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// WireNormalizer builds the normalize registry (Tier 1 AI builtins +
// Tier 1 per-host adapters + Tier 2 pattern probe + Tier 3 verbatim)
// for the forward-handler enforcement path and stamps the proxy's Thing
// identity onto the audit writer (when it is an *audit.MQBatchWriter).
// Returns the registry so the caller can inject it into the forward
// handler via tlsbump.WithNormalizeRegistry.
func WireNormalizer(writer audit.Writer, proxyID, hostname string) *normcore.Registry {
	reg := sharednormalize.BuildRegistry()
	if w, ok := writer.(*audit.MQBatchWriter); ok {
		w.WithThingIdentity(proxyID, hostname)
	}
	return reg
}
