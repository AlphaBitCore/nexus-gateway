//go:build windows

package platform

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/network/relay"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/windows"
)

// WindowsPlatform is re-exported for optional type assertions.
type WindowsPlatform = windows.WindowsPlatform

// NewPlatform creates the Windows platform shim.
func NewPlatform(addr string, relayClient *relay.Client) Platform {
	return windows.NewPlatform(addr, relayClient)
}
