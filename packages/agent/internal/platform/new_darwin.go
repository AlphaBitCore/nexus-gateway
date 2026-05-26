//go:build darwin

package platform

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/bundles"
)

// DarwinPlatform is re-exported so platformshim callers can do type assertions
// without importing platform/darwin directly.
type DarwinPlatform = darwin.DarwinPlatform

// NewPlatform creates the macOS platform shim.
func NewPlatform(addr string, relayClient *relay.Client) Platform {
	return darwin.NewPlatform(addr, relayClient)
}

// InspectBundles returns the macOS bundle version stamps for the host app
// and system extension. Re-exported from platform/darwin so platformshim
// callers can use a single "platform" import.
func InspectBundles() bundles.BundleVersions {
	return darwin.InspectBundles()
}
