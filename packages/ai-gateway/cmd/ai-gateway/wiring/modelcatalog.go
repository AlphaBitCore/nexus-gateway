package wiring

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/models"
)

// selectModelCatalog picks the backing store for GET /v1/models and
// GET /v1/models/{model}.
//
// The config snapshot wins over the raw table. It is the same Model data
// the data plane already routes, prices, and capability-checks on, so the
// catalog these endpoints advertise can no longer disagree with what the
// router will accept — reading the table directly meant a model could be
// advertised before the snapshot picked it up. It also removes one
// Postgres round-trip and the full row-scan per catalog request. Freshness
// is bounded by the Hub `models` push that drives ReloadModels, which is
// the same event that makes a model routable in the first place.
//
// Returning an interface (rather than passing a concrete pointer straight
// into the handler) is load-bearing: a nil *cachelayer.Layer assigned to
// an interface produces a NON-nil interface holding a nil pointer, which
// slips past the handlers' `models == nil` guard and panics on first use.
func selectModelCatalog(deps RouteDeps) models.ModelLookup {
	switch {
	case deps.CacheLayer != nil:
		return deps.CacheLayer
	case deps.DB != nil:
		return deps.DB
	default:
		return nil
	}
}
