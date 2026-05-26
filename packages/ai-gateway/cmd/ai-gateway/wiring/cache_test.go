package wiring

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// TestInitCacheLayer_nilDBReturnsError verifies InitCacheLayer propagates
// the cachelayer.New error when db is nil. The non-nil DB path requires a
// live PostgreSQL pool (cachelayer.New uses db.Pool, not the pgxmock seam)
// and is documented as DB-bound.
func TestInitCacheLayer_nilDBReturnsError(t *testing.T) {
	opsReg := registry.NewRegistry(prometheus.NewRegistry())
	layer, err := InitCacheLayer(context.Background(), nil, discardLogger(), opsReg)
	if err == nil {
		t.Fatal("expected error when db is nil")
	}
	if layer != nil {
		t.Fatal("expected nil layer on error")
	}
}
