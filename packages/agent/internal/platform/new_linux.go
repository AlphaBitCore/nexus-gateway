//go:build linux

package platform

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/linux"
)

// LinuxPlatform is re-exported for optional type assertions.
type LinuxPlatform = linux.LinuxPlatform

// NewPlatform creates the Linux platform shim.
func NewPlatform(addr string, relayClient *relay.Client) Platform {
	return linux.NewPlatform(addr, relayClient)
}
