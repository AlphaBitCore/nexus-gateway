package vendorbill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// applyAmountScale both branches — the cents branch is otherwise unreachable
// because anthropicAmountIsCents is pinned off.
func TestApplyAmountScale_BothInterpretations(t *testing.T) {
	if got := applyAmountScale(100, false); got != 100 {
		t.Errorf("dollars: got %v want 100", got)
	}
	if got := applyAmountScale(100, true); got != 1 {
		t.Errorf("cents: got %v want 1", got)
	}
}

// parseAnthropicDay accepts a bare date and rejects garbage.
func TestParseAnthropicDay(t *testing.T) {
	d, err := parseAnthropicDay("2026-07-01")
	if err != nil || !d.Equal(utcDay(2026, 7, 1)) {
		t.Fatalf("bare date: got %v err=%v", d, err)
	}
	if _, err := parseAnthropicDay("nonsense"); err == nil {
		t.Fatal("garbage date must error")
	}
}

func TestAnthropicBillSource_BadDayErrors(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"starting_at": "nonsense", "results": [{"amount": "1.00", "currency": "USD", "workspace_id": "w"}]}], "has_more": false}`))
	})
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("unparseable starting_at must error")
	}
}

func TestAnthropicBillSource_BadAmountErrors(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"starting_at": "2026-07-01T00:00:00Z", "results": [{"amount": "abc", "currency": "USD", "workspace_id": "w"}]}], "has_more": false}`))
	})
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("unparseable amount must error")
	}
}

func TestAnthropicBillSource_Pagination(t *testing.T) {
	calls := 0
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("page") == "" {
			w.Write([]byte(`{"data": [{"starting_at": "2026-07-01T00:00:00Z", "results": [{"amount": "1.00", "currency": "USD", "workspace_id": "w"}]}], "has_more": true, "next_page": "P2"}`))
			return
		}
		w.Write([]byte(`{"data": [{"starting_at": "2026-07-02T00:00:00Z", "results": [{"amount": "2.00", "currency": "USD", "workspace_id": "w"}]}], "has_more": false}`))
	})
	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 2))
	if err != nil || calls != 2 || len(bills) != 2 {
		t.Fatalf("anthropic pagination: calls=%d bills=%+v err=%v", calls, bills, err)
	}
}

// maxPages guard: a server that always says has_more must be bounded, not loop
// forever.
func TestOpenAIBillSource_MaxPagesGuard(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"start_time": ` + itoa(utcDay(2026, 7, 1).Unix()) + `, "results": [{"amount": {"value": 1.0, "currency": "usd"}, "project_id": "p"}]}], "has_more": true, "next_page": "loop"}`))
	})
	src.maxPages = 2
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("runaway pagination must be bounded by maxPages")
	}
}

func TestAnthropicBillSource_MaxPagesGuard(t *testing.T) {
	src := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": [{"starting_at": "2026-07-01T00:00:00Z", "results": [{"amount": "1.00", "currency": "USD", "workspace_id": "w"}]}], "has_more": true, "next_page": "loop"}`))
	})
	src.maxPages = 2
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("runaway pagination must be bounded by maxPages")
	}
}

// getJSON transport error (server closed) and decode error (invalid JSON body).
func TestGetJSON_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // immediately closed → connection refused
	src := newOpenAIBillSource("k", "", srv.URL, srv.Client())
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("transport failure must surface as an error")
	}
}

func TestGetJSON_DecodeError(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not valid json`))
	})
	if _, err := src.FetchDailyBill(context.Background(), utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("invalid JSON body must surface as a decode error")
	}
}

// Guard: a cancelled context must not hang.
func TestFetchDailyBill_ContextCancelled(t *testing.T) {
	src := openaiServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte(`{"data": [], "has_more": false}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.FetchDailyBill(ctx, utcDay(2026, 7, 1), utcDay(2026, 7, 1)); err == nil {
		t.Fatal("cancelled context must error")
	}
}
