package vendorbill

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func openaiServer(t *testing.T, handler http.HandlerFunc) *openaiBillSource {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return newOpenAIBillSource("sk-admin-test", "", srv.URL, srv.Client())
}

// day helper: unix seconds for UTC midnight of the given date.
func utcDay(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestOpenAIBillSource_ParsesDailyUSD_AndNarrowsToProject(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-admin-test" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		w.Write([]byte(`{
		  "data": [
		    {"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) + `, "results": [
		      {"amount": {"value": 1.25, "currency": "usd"}, "project_id": "proj_gw"}
		    ]},
		    {"start_time": ` + itoa(utcDay(2026, 7, 2).Unix()) + `, "results": [
		      {"amount": {"value": 2.50, "currency": "usd"}, "project_id": "proj_gw"}
		    ]}
		  ],
		  "has_more": false, "next_page": null
		}`))
	})

	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 2))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(bills) != 2 {
		t.Fatalf("want 2 daily bills, got %d", len(bills))
	}
	if bills[0].AmountUSD != 1.25 || bills[1].AmountUSD != 2.50 {
		t.Fatalf("wrong amounts: %+v", bills)
	}
	if !bills[0].Day.Equal(utcDay(2026, 7, 1)) {
		t.Fatalf("wrong day ordering / value: %v", bills[0].Day)
	}
	if bills[0].ScopeKind != "project" || bills[0].ScopeID != "proj_gw" {
		t.Fatalf("single project must be scoped: kind=%q id=%q", bills[0].ScopeKind, bills[0].ScopeID)
	}
}

func TestOpenAIBillSource_MultiProjectIsOrg(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "data": [
		    {"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) + `, "results": [
		      {"amount": {"value": 1.00, "currency": "usd"}, "project_id": "proj_a"},
		      {"amount": {"value": 3.00, "currency": "usd"}, "project_id": "proj_b"}
		    ]}
		  ],
		  "has_more": false
		}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(bills) != 1 || bills[0].AmountUSD != 4.00 {
		t.Fatalf("want one day summing both projects to 4.00, got %+v", bills)
	}
	if bills[0].ScopeKind != "org" || bills[0].ScopeID != "" {
		t.Fatalf("multiple projects must collapse to org: kind=%q id=%q", bills[0].ScopeKind, bills[0].ScopeID)
	}
}

func TestOpenAIBillSource_Pagination(t *testing.T) {
	calls := 0
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("page") == "" {
			w.Write([]byte(`{"data": [{"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) + `, "results": [{"amount": {"value": 1.00, "currency": "usd"}, "project_id": "proj_gw"}]}], "has_more": true, "next_page": "PAGE2"}`))
			return
		}
		w.Write([]byte(`{"data": [{"start_time": ` + itoa(utcDay(2026, 7, 2).Unix()) + `, "results": [{"amount": {"value": 2.00, "currency": "usd"}, "project_id": "proj_gw"}]}], "has_more": false, "next_page": null}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 2))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 page fetches, got %d", calls)
	}
	if len(bills) != 2 || bills[0].AmountUSD != 1.00 || bills[1].AmountUSD != 2.00 {
		t.Fatalf("pages not assembled: %+v", bills)
	}
}

func TestOpenAIBillSource_PartialWindowIsSuccess(t *testing.T) {
	// Request 3 days; vendor only has data for 1 (the rest not yet finalized).
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) + `, "results": [{"amount": {"value": 5.00, "currency": "usd"}, "project_id": "proj_gw"}]}], "has_more": false}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 3))
	if err != nil {
		t.Fatalf("partial window must be a success, got err: %v", err)
	}
	if len(bills) != 1 {
		t.Fatalf("want only the finalized day (absence != zero), got %d rows", len(bills))
	}
}

func TestOpenAIBillSource_AuthFailErrors(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid admin key"}`))
	})
	_, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err == nil {
		t.Fatal("401 must return an error so the job records fetch_failed")
	}
}

func TestOpenAIBillSource_NonUSDRejected(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) + `, "results": [{"amount": {"value": 1.00, "currency": "eur"}, "project_id": "proj_gw"}]}], "has_more": false}`))
	})
	_, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err == nil {
		t.Fatal("non-USD currency must be rejected, not silently summed at face value")
	}
}

func TestOpenAIBillSource_ProviderKey(t *testing.T) {
	if k := newOpenAIBillSource("k", "", "", nil).ProviderKey(); k != "openai" {
		t.Fatalf("ProviderKey = %q, want openai", k)
	}
}

// The live API encodes amount.value as a 20-significant-digit STRING, not the
// bare number the docs show. Every fixture in this file used a number, so a
// float64-only field passed all tests while failing 100% of real fetches with
// "cannot unmarshal string into Go struct field ... of type float64" (observed
// 2026-07-20). This test pins the real wire shape.
func TestOpenAIBillSource_AcceptsStringAmount_RealWireShape(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "data": [
		    {"start_time": ` + itoa(utcDay(2026, 7, 15).Unix()) + `, "results": [
		      {"amount": {"value": "15.46230655000000000000000000", "currency": "usd"}, "project_id": "proj_gw"}
		    ]}
		  ],
		  "has_more": false
		}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 15), utcDay(2026, 7, 15))
	if err != nil {
		t.Fatalf("string-encoded amount must decode, got: %v", err)
	}
	if len(bills) != 1 {
		t.Fatalf("want 1 bill, got %d", len(bills))
	}
	if math.Abs(bills[0].AmountUSD-15.46230655) > 1e-9 {
		t.Fatalf("AmountUSD = %v, want 15.46230655", bills[0].AmountUSD)
	}
}

func TestOpenAIBillSource_RejectsUnparseableAmount(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) +
			`, "results": [{"amount": {"value": "not-money", "currency": "usd"}, "project_id": "p"}]}], "has_more": false}`))
	})
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("a non-numeric amount must fail the fetch (recorded as fetch_failed), never sum as 0")
	}
}

// With an api key pinned, the vendor must be asked to narrow the bill, and the
// resulting scope is known exactly — even though the response still carries
// several project ids, which unpinned would collapse to "org".
func TestOpenAIBillSource_PinnedAPIKey_FiltersServerSideAndScopesExactly(t *testing.T) {
	var gotKeyIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeyIDs = r.URL.Query()["api_key_ids"]
		w.Write([]byte(`{
		  "data": [
		    {"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) + `, "results": [
		      {"amount": {"value": "3.00", "currency": "usd"}, "project_id": "proj_a"},
		      {"amount": {"value": "1.00", "currency": "usd"}, "project_id": "proj_b"}
		    ]}
		  ],
		  "has_more": false
		}`))
	}))
	t.Cleanup(srv.Close)
	src := newOpenAIBillSource("sk-admin-test", "key_gw123", srv.URL, srv.Client())

	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(gotKeyIDs) != 1 || gotKeyIDs[0] != "key_gw123" {
		t.Fatalf("api_key_ids filter not sent to vendor: %v", gotKeyIDs)
	}
	if len(bills) != 1 || bills[0].AmountUSD != 4.00 {
		t.Fatalf("want the filtered day summing to 4.00, got %+v", bills)
	}
	if bills[0].ScopeKind != "api_key" || bills[0].ScopeID != "key_gw123" {
		t.Fatalf("pinned key must scope exactly: kind=%q id=%q (multi-project response must NOT downgrade it to org)",
			bills[0].ScopeKind, bills[0].ScopeID)
	}
}

func TestOpenAIBillSource_UnpinnedSendsNoKeyFilter(t *testing.T) {
	var sawParam bool
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, sawParam = r.URL.Query()["api_key_ids"]
		w.Write([]byte(`{"data": [], "has_more": false}`))
	})
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if sawParam {
		t.Fatal("unpinned source must not send api_key_ids at all")
	}
}

// TestOpenAIFetchDailyBill_EmptyBucketIsAReportedZero: OpenAI answers a day the
// gateway sent no traffic on with a bucket whose results array is empty — the
// vendor stating it charged nothing. That is a fact about the day, not an
// absence of one, and the two are opposite downstream: a reported zero earns a
// $0 reconciliation row (and heals a fetch_failed placeholder left on an idle
// day), while a day the vendor omitted must be left for a later run.
//
// Verified against the live endpoint on 2026-08-21 for the prod key: 08-16
// came back as {"start_time":…,"results":[]} while 08-15/17/18 carried amounts.
func TestOpenAIFetchDailyBill_EmptyBucketIsAReportedZero(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[
		  {"start_time":` + itoa(utcDay(2026, 8, 15).Unix()) + `,"results":[{"amount":{"value":0.55,"currency":"usd"},"project_id":"proj_gw"}]},
		  {"start_time":` + itoa(utcDay(2026, 8, 16).Unix()) + `,"results":[]},
		  {"start_time":` + itoa(utcDay(2026, 8, 17).Unix()) + `,"results":[{"amount":{"value":2.13,"currency":"usd"},"project_id":"proj_gw"}]}
		],"has_more":false,"next_page":null}`))
	})

	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 8, 15), utcDay(2026, 8, 17))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(bills) != 3 {
		t.Fatalf("bills = %+v, want all three reported days including the empty one", bills)
	}
	if !bills[1].Day.Equal(utcDay(2026, 8, 16)) || bills[1].AmountUSD != 0 {
		t.Errorf("bills[1] = %+v, want 2026-08-16 at $0", bills[1])
	}
	if bills[0].AmountUSD != 0.55 || bills[2].AmountUSD != 2.13 {
		t.Errorf("the surrounding days must be untouched, got %+v and %+v", bills[0], bills[2])
	}
}

// TestOpenAIFetchDailyBill_DayAbsentFromResponseStaysAbsent is the other half of
// the rule: only a bucket the vendor actually returned becomes a zero. A day
// missing from the response entirely still means "not finalized", and inventing
// a $0 for it would write a comparison the vendor never made.
func TestOpenAIFetchDailyBill_DayAbsentFromResponseStaysAbsent(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[
		  {"start_time":` + itoa(utcDay(2026, 8, 15).Unix()) + `,"results":[{"amount":{"value":0.55,"currency":"usd"},"project_id":"proj_gw"}]}
		],"has_more":false,"next_page":null}`))
	})

	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 8, 15), utcDay(2026, 8, 17))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if len(bills) != 1 || !bills[0].Day.Equal(utcDay(2026, 8, 15)) {
		t.Fatalf("bills = %+v, want only the day the vendor reported a bucket for", bills)
	}
}

// TestOpenAIBillingHost: the host the cost API is read from is what a Provider
// row's baseUrl is matched against, so it must follow the source's configured
// base URL rather than a constant.
func TestOpenAIBillingHost(t *testing.T) {
	if got := newOpenAIBillSource("k", "", "", nil).BillingHost(); got != "api.openai.com" {
		t.Errorf("default BillingHost = %q, want api.openai.com", got)
	}
	if got := newOpenAIBillSource("k", "", "https://gw.internal:8443/v1", nil).BillingHost(); got != "gw.internal" {
		t.Errorf("overridden BillingHost = %q, want the override's host without its port", got)
	}
}
