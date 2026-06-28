// Package resource is the operator console's OpenAPI resource cascade: a generic
// browse/read/write view over the Control Plane admin API, driven by the catalog
// in internal/capabilities/resource. It is a leaf dashboard view — the shell holds
// it as a kit.ViewModel and never names its concrete type.
package resource

import (
	"context"
	"github.com/goccy/go-json"
	"net/url"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// AdminClient is the only gateway capability the cascade needs: the generic
// Control-Plane admin API call. *core.Client — and the root tui.Gateway — satisfy
// it structurally, so the cascade depends on this one-method seam rather than the
// full gateway interface.
type AdminClient interface {
	AdminRequest(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, int, error)
}

// NewResource builds the resource-cascade view over the admin client. It returns
// the kit.ViewModel the dashboard shell holds; the concrete view type stays
// package-private (in-package tests construct it via newResource).
func NewResource(gw AdminClient, s ...kit.Session) kit.ViewModel {
	return newResource(gw, s...)
}
