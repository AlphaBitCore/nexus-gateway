package vendorbill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func anthropicServer(t *testing.T, handler http.HandlerFunc) *anthropicBillSource {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return newAnthropicBillSource("sk-ant-admin01-test", "", srv.URL, srv.Client())
}

func TestAnthropicBillSource_SumsCostTypes_AndNarrowsToWorkspace(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-ant-admin01-test" {
			t.Errorf("wrong admin key header: %q", got)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version header")
		}
		// One day, two cost_type results in the same workspace — must sum.
		// Wire amounts are CENTS (see anthropicAmountIsCents): 1000 + 250 = 1250c
		// must surface as 12.50 USD.
		w.Write([]byte(`{
		  "data": [
		    {"starting_at": "2026-07-01T00:00:00Z", "results": [
		      {"amount": "1000.00", "currency": "USD", "workspace_id": "wrkspc_gw"},
		      {"amount": "250.00",  "currency": "USD", "workspace_id": "wrkspc_gw"}
		    ]}
		  ],
		  "has_more": false, "next_page": null
		}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(bills) != 1 || bills[0].AmountUSD != 12.50 {
		t.Fatalf("want one day summing cost_types to 12.50, got %+v", bills)
	}
	if bills[0].ScopeKind != "workspace" || bills[0].ScopeID != "wrkspc_gw" {
		t.Fatalf("single workspace must be scoped: kind=%q id=%q", bills[0].ScopeKind, bills[0].ScopeID)
	}
}

func TestAnthropicBillSource_MultiWorkspaceIsOrg(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Cents on the wire: 400 + 600 = 1000c must surface as 10.00 USD.
		w.Write([]byte(`{
		  "data": [
		    {"starting_at": "2026-07-01T00:00:00Z", "results": [
		      {"amount": "400.00", "currency": "USD", "workspace_id": "wrkspc_a"},
		      {"amount": "600.00", "currency": "USD", "workspace_id": "wrkspc_b"}
		    ]}
		  ],
		  "has_more": false
		}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(bills) != 1 || bills[0].AmountUSD != 10.00 {
		t.Fatalf("want 10.00 summed across workspaces, got %+v", bills)
	}
	if bills[0].ScopeKind != "org" {
		t.Fatalf("multiple workspaces must collapse to org, got %q", bills[0].ScopeKind)
	}
}

func TestAnthropicBillSource_AuthFailErrors(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err == nil {
		t.Fatal("401 must return an error so the job records fetch_failed")
	}
}

func TestAnthropicBillSource_NonUSDRejected(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"starting_at": "2026-07-01T00:00:00Z", "results": [{"amount": "1.00", "currency": "GBP", "workspace_id": "wrkspc_gw"}]}], "has_more": false}`))
	})
	_, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err == nil {
		t.Fatal("non-USD currency must be rejected")
	}
}

func TestAnthropicBillSource_ProviderKey(t *testing.T) {
	if k := newAnthropicBillSource("k", "", "", nil).ProviderKey(); k != "anthropic" {
		t.Fatalf("ProviderKey = %q, want anthropic", k)
	}
}

// cost_report accepts no filter parameters, so a pinned workspace must be
// enforced client-side: other workspaces' costs are dropped, not summed.
func TestAnthropicBillSource_PinnedWorkspace_FiltersClientSide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "data": [
		    {"starting_at": "2026-07-01T00:00:00Z", "results": [
		      {"amount": "500.00",  "currency": "USD", "workspace_id": "wrkspc_gw"},
		      {"amount": "9000.00", "currency": "USD", "workspace_id": "wrkspc_other"}
		    ]}
		  ],
		  "has_more": false
		}`))
	}))
	t.Cleanup(srv.Close)
	src := newAnthropicBillSource("sk-ant-admin01-test", "wrkspc_gw", srv.URL, srv.Client())

	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	// 500c = 5.00 USD; the other workspace's 9000c must not appear.
	if len(bills) != 1 || bills[0].AmountUSD != 5.00 {
		t.Fatalf("pinned workspace must exclude other workspaces: got %+v", bills)
	}
	if bills[0].ScopeKind != "workspace" || bills[0].ScopeID != "wrkspc_gw" {
		t.Fatalf("pinned workspace must scope exactly: kind=%q id=%q", bills[0].ScopeKind, bills[0].ScopeID)
	}
}

// A non-USD result in an UNPINNED workspace must not fail a pinned fetch — the
// filter runs before the currency check precisely so an unrelated workspace
// cannot break reconciliation for the gateway's own.
func TestAnthropicBillSource_PinnedWorkspace_IgnoresOtherWorkspaceCurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "data": [
		    {"starting_at": "2026-07-01T00:00:00Z", "results": [
		      {"amount": "500.00", "currency": "USD", "workspace_id": "wrkspc_gw"},
		      {"amount": "100.00", "currency": "GBP", "workspace_id": "wrkspc_other"}
		    ]}
		  ],
		  "has_more": false
		}`))
	}))
	t.Cleanup(srv.Close)
	src := newAnthropicBillSource("k", "wrkspc_gw", srv.URL, srv.Client())

	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err != nil {
		t.Fatalf("another workspace's currency must not fail a pinned fetch: %v", err)
	}
	if len(bills) != 1 || bills[0].AmountUSD != 5.00 {
		t.Fatalf("got %+v", bills)
	}
}

// The default workspace is reported with a NULL/empty workspace_id, so an
// account that never created a named workspace can never be inferred as scoped.
// This is why the operator runbook requires creating one (or pinning).
func TestAnthropicBillSource_DefaultWorkspaceIsOrgNotScoped(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "data": [
		    {"starting_at": "2026-07-01T00:00:00Z", "results": [
		      {"amount": "1000.00", "currency": "USD", "workspace_id": null}
		    ]}
		  ],
		  "has_more": false
		}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(bills) != 1 || bills[0].AmountUSD != 10.00 {
		t.Fatalf("got %+v", bills)
	}
	if bills[0].ScopeKind != "org" || bills[0].ScopeID != "" {
		t.Fatalf("null workspace_id must resolve to org (not a bogus scoped row): kind=%q id=%q",
			bills[0].ScopeKind, bills[0].ScopeID)
	}
}
