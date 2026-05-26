package wiring

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/host/openbrowser"
)

// InitOpenBrowser creates the open-browser helper with an initially empty
// allowed-hosts list. The caller populates it via SetAllowedHosts once the
// bootstrap client resolves the Control Plane URL.
func InitOpenBrowser() *openbrowser.Opener {
	return openbrowser.New()
}
