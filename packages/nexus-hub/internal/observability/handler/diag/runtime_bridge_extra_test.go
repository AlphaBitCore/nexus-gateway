package diag

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// TestRuntime_ThingReturnsNon200 verifies that when the proxied /debug/runtime
// endpoint returns a non-200 status code, the handler returns 502 BadGateway.
func TestRuntime_ThingReturnsNon200(t *testing.T) {
	// Backend returns 500.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer backend.Close()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	desiredJSON, _ := json.Marshal(map[string]any{})
	reportedJSON, _ := json.Marshal(map[string]any{})
	metricsURL := backend.URL + "/metrics"

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("svc-500").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver",
			"last_seen_at", "desired", "reported", "coalesce",
		}).AddRow("svc-500", "ai-gateway", "online", int64(1), int64(1),
			&now, desiredJSON, reportedJSON, metricsURL))

	a := &RuntimeBridgeAPI{
		DB:           mock,
		HubID:        "hub-1",
		ServiceToken: "svc-token",
		HTTPClient:   backend.Client(),
	}
	c, rec := makeRuntimeCtx(t, "svc-500")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d; want 502 when thing returns non-200", rec.Code)
	}
}

// TestRuntime_FetchSnapshotError_502 verifies that a connection error to the
// thing's /debug/runtime endpoint produces a 502.
func TestRuntime_FetchSnapshotError_502(t *testing.T) {
	// Backend that closes immediately to simulate connection refused.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	metricsURL := backend.URL + "/metrics"
	backend.Close() // closed before the request — triggers connection error

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	desiredJSON, _ := json.Marshal(map[string]any{})
	reportedJSON, _ := json.Marshal(map[string]any{})

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("svc-connrefused").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver",
			"last_seen_at", "desired", "reported", "coalesce",
		}).AddRow("svc-connrefused", "ai-gateway", "online", int64(1), int64(1),
			&now, desiredJSON, reportedJSON, metricsURL))

	a := &RuntimeBridgeAPI{
		DB:           mock,
		HubID:        "hub-1",
		ServiceToken: "svc-token",
		HTTPClient:   backend.Client(),
	}
	c, rec := makeRuntimeCtx(t, "svc-connrefused")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status %d; want 502 when proxy call fails", rec.Code)
	}
}

// TestInsertDiagDrainEvent_DBExecError verifies that an error from pool.Exec
// is propagated back to the caller.
func TestInsertDiagDrainEvent_DBExecError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	expectInsertError(mock, errDiagDB)

	evt := diagEvtSimple("err-test")
	err = insertDiagDrainEvent(t.Context(), mock, "t-1", "agent", evt)
	if err == nil {
		t.Error("expected error from insertDiagDrainEvent on DB Exec failure")
	}
}

func diagEvtSimple(id string) DiagDrainEvent {
	return DiagDrainEvent{ID: id}
}
