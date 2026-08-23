package wiring

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/costing"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// TestInitRouter_nilDeps verifies InitRouter with nil optional deps does not
// panic and returns non-nil strategy registry, health ranker, and capability
// cache.
func TestInitRouter_nilDeps(t *testing.T) {
	stratReg, healthRanker, resolver, capCache := InitRouter(context.Background(), nil, nil, nil, nil, discardLogger(), false)
	if stratReg == nil {
		t.Error("expected non-nil strategy registry")
	}
	if healthRanker == nil {
		t.Error("expected non-nil health ranker")
	}
	if resolver == nil {
		t.Error("expected non-nil routing resolver")
	}
	if capCache == nil {
		t.Error("expected non-nil capability cache")
	}
}

// routerPriceStub is a minimal AIGuardModelLookup returning one catalog row.
type routerPriceStub struct {
	model *store.Model
	err   error
	gotID string
}

func (s *routerPriceStub) GetModel(_ context.Context, id string) (*store.Model, error) {
	s.gotID = id
	return s.model, s.err
}

// TestNewRouterDecider_PriceLookupReturnsCatalogPrices verifies the router
// decider's price lookup reads the SAME catalog fields the AI Guard wiring uses,
// so the router call's cost is priced from the model's per-million rates.
func TestNewRouterDecider_PriceLookupReturnsCatalogPrices(t *testing.T) {
	in, out, cachedRead := 2.50, 10.00, 0.625
	models := &routerPriceStub{model: &store.Model{
		InputPricePM: &in, OutputPricePM: &out, CachedInputReadPricePM: &cachedRead,
	}}

	d := newRouterDecider(context.Background(), nil, nil, models, discardLogger())
	if d.PriceLookup == nil {
		t.Fatal("PriceLookup must be wired when a model lookup is available")
	}
	got, priced := d.PriceLookup("model-gpt-4o")
	if !priced {
		t.Fatal("a model with an input price must report as priced")
	}
	if got.InputUSDPerM != 2.50 || got.OutputUSDPerM != 10.00 {
		t.Errorf("input/output = (%v,%v), want (2.5,10)", got.InputUSDPerM, got.OutputUSDPerM)
	}
	// The cache rates are the point of the widened signature: without them the
	// router bills its heavily-cached prompt at the full input rate.
	if got.CacheReadUSDPerM != 0.625 {
		t.Errorf("CacheReadUSDPerM = %v, want 0.625 from the catalog column", got.CacheReadUSDPerM)
	}
	// Cache-write is NULL on this row — "no surcharge configured" falls back to
	// the input rate, the same rule the customer request path applies.
	if got.CacheWriteUSDPerM != 2.50 {
		t.Errorf("CacheWriteUSDPerM = %v, want the input rate 2.5 on a NULL column", got.CacheWriteUSDPerM)
	}
	if models.gotID != "model-gpt-4o" {
		t.Errorf("looked up %q, want the router model id", models.gotID)
	}
}

// TestNewRouterDecider_PriceLookupFailuresYieldZero pins the fail-soft
// contract: a lookup error, a nil row, or a row with no prices must report
// unpriced so an unpriced router model leaves the cost zero instead of failing
// the request.
func TestNewRouterDecider_PriceLookupFailuresYieldZero(t *testing.T) {
	cases := map[string]*routerPriceStub{
		"lookup error": {err: errors.New("catalog cold")},
		"nil row":      {},
		"no prices":    {model: &store.Model{}},
	}
	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			d := newRouterDecider(context.Background(), nil, nil, models, discardLogger())
			got, priced := d.PriceLookup("model-x")
			if priced {
				t.Errorf("priced = true, want false — an unpriced router model must not be billed")
			}
			if got != (costing.Rates{}) {
				t.Errorf("rates = %+v, want the zero value", got)
			}
		})
	}
}

// TestNewRouterDecider_PriceLookupFailureLogsWarn pins finding 4 from the
// 2026-08-04 vendor-spend reconciliation review: a router model with no
// catalog price must no longer fail silently. Both a failed lookup and a
// zero-priced row must produce one Warn naming the router model id, so a
// pricing-row gap on the router model is visible instead of silently
// reproducing the original under-report (the day still looks reconciled
// because emitVendorSpend drops the zero component).
func TestNewRouterDecider_PriceLookupFailureLogsWarn(t *testing.T) {
	cases := map[string]*routerPriceStub{
		"lookup error": {err: errors.New("catalog cold")},
		"no prices":    {model: &store.Model{}},
	}
	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			d := newRouterDecider(context.Background(), nil, nil, models, logger)
			d.PriceLookup("model-unpriced-router")

			out := buf.String()
			if !strings.Contains(out, "level=WARN") {
				t.Fatalf("expected a Warn log for an unpriced router model, got: %q", out)
			}
			if !strings.Contains(out, "model-unpriced-router") {
				t.Fatalf("expected the log to name the router model id, got: %q", out)
			}
		})
	}
}

// TestNewRouterDecider_NoModelLookup_LeavesPriceLookupNil verifies that
// without a catalog the decider carries no price source at all — the router
// call is then attributed but unpriced, never priced from a fabricated table.
func TestNewRouterDecider_NoModelLookup_LeavesPriceLookupNil(t *testing.T) {
	d := newRouterDecider(context.Background(), nil, nil, nil, discardLogger())
	if d.PriceLookup != nil {
		t.Error("PriceLookup must stay nil without a model catalog")
	}
}
