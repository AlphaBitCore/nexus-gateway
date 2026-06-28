package diag

import (
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
)

// makeRuntimeCtx creates an Echo context with a path param :id set.
func makeRuntimeCtx(t *testing.T, id string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things/"+id+"/runtime", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	return c, rec
}

func TestResolveIntrospectURL_HubSelf(t *testing.T) {
	got, err := resolveIntrospectURL("hub-1", "hub-1", "http://localhost:3060", "http://host:9100/metrics")
	if err != nil {
		t.Fatalf("resolveIntrospectURL: %v", err)
	}
	want := "http://localhost:3060/debug/runtime"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestResolveIntrospectURL_HubSelf_NoLocalURL(t *testing.T) {
	// If hubLocalURL is empty, fall through to metricsURL path.
	got, err := resolveIntrospectURL("hub-1", "hub-1", "", "http://host:9100/metrics")
	if err != nil {
		t.Fatalf("resolveIntrospectURL: %v", err)
	}
	want := "http://host:9100/debug/runtime"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestResolveIntrospectURL_OtherThing(t *testing.T) {
	got, err := resolveIntrospectURL("svc-1", "hub-1", "http://localhost:3060", "http://host:9100/metrics")
	if err != nil {
		t.Fatalf("resolveIntrospectURL: %v", err)
	}
	want := "http://host:9100/debug/runtime"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestResolveIntrospectURL_MetricsURLNoSuffix(t *testing.T) {
	// metricsURL without the /metrics suffix — function still appends /debug/runtime
	// to the base (with trailing slash stripped).
	got, err := resolveIntrospectURL("svc-1", "hub-1", "", "http://host:9100/")
	if err != nil {
		t.Fatalf("resolveIntrospectURL: %v", err)
	}
	// TrimSuffix won't match "/metrics" so base stays as-is then
	// TrimRight removes trailing slash.
	want := "http://host:9100/debug/runtime"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestResolveIntrospectURL_EmptyMetricsURL(t *testing.T) {
	_, err := resolveIntrospectURL("svc-1", "hub-1", "", "")
	if err == nil {
		t.Error("expected error when metricsURL is empty and not hub self")
	}
}

// RuntimeBridgeAPI.Runtime: routing invariants

func TestRuntime_MissingID_400(t *testing.T) {
	a := &RuntimeBridgeAPI{}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/hub/things//runtime", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// No id param set — c.Param("id") returns "".
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d; want 400 when id missing", rec.Code)
	}
	// canonical {error, code} envelope.
	assertCanonicalErrorEnvelope(t, rec, "INVALID_REQUEST")
}

// assertCanonicalErrorEnvelope checks the response body is the canonical
// {error:{message,type,code}} nested envelope with the expected machine-readable
// code.
func assertCanonicalErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("error body not JSON: %s", rec.Body.String())
	}
	inner, ok := resp["error"].(map[string]any)
	if !ok {
		t.Errorf("error field missing/empty: %v", resp)
		return
	}
	if msg, _ := inner["message"].(string); msg == "" {
		t.Errorf("error.message missing/empty: %v", resp)
	}
	if c, _ := inner["code"].(string); c != wantCode {
		t.Errorf("code=%q, want %q; body=%s", inner["code"], wantCode, rec.Body.String())
	}
}

func TestRuntime_ThingNotFound_404(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("nonexistent").
		WillReturnError(pgx.ErrNoRows)

	a := &RuntimeBridgeAPI{DB: mock}
	c, rec := makeRuntimeCtx(t, "nonexistent")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d; want 404 for ErrNoRows", rec.Code)
	}
	assertCanonicalErrorEnvelope(t, rec, "NOT_FOUND")
}

func TestRuntime_DBError_500(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("thing-1").
		WillReturnError(errDiagDB)

	a := &RuntimeBridgeAPI{DB: mock}
	c, rec := makeRuntimeCtx(t, "thing-1")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d; want 500 on DB error", rec.Code)
	}
}

func TestRuntime_AgentType_501(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	desiredJSON, _ := json.Marshal(map[string]any{"k": "v"})
	reportedJSON, _ := json.Marshal(map[string]any{"k": "v"})

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("agent-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver",
			"last_seen_at", "desired", "reported", "coalesce",
		}).AddRow("agent-1", "agent", "online", int64(1), int64(1),
			&now, desiredJSON, reportedJSON, ""))

	a := &RuntimeBridgeAPI{DB: mock}
	c, rec := makeRuntimeCtx(t, "agent-1")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status %d; want 501 for agent type", rec.Code)
	}
}

func TestRuntime_Offline_503(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	desiredJSON, _ := json.Marshal(map[string]any{})
	reportedJSON, _ := json.Marshal(map[string]any{})

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("svc-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver",
			"last_seen_at", "desired", "reported", "coalesce",
		}).AddRow("svc-1", "control-plane", "offline", int64(1), int64(1),
			&now, desiredJSON, reportedJSON, "http://host:9100/metrics"))

	a := &RuntimeBridgeAPI{DB: mock}
	c, rec := makeRuntimeCtx(t, "svc-1")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d; want 503 for offline thing", rec.Code)
	}
}

func TestRuntime_NoMetricsURL_503(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	now := time.Now()
	desiredJSON, _ := json.Marshal(map[string]any{})
	reportedJSON, _ := json.Marshal(map[string]any{})

	// Online but no metrics_url → resolveIntrospectURL returns error.
	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("svc-nometrics").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver",
			"last_seen_at", "desired", "reported", "coalesce",
		}).AddRow("svc-nometrics", "control-plane", "online", int64(1), int64(1),
			&now, desiredJSON, reportedJSON, ""))

	a := &RuntimeBridgeAPI{DB: mock, HubID: "hub-1"}
	c, rec := makeRuntimeCtx(t, "svc-nometrics")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d; want 503 when no metrics_url and not hub-self", rec.Code)
	}
}

// TestRuntime_Online_ProxyForwards verifies that a non-agent online thing
// proxies the request to the thing's /debug/runtime endpoint. We use an
// httptest.Server to act as the thing endpoint.
func TestRuntime_Online_ProxyForwards(t *testing.T) {
	// Fake /debug/runtime endpoint that returns 200.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	// metrics_url points to backend/metrics so resolveIntrospectURL strips /metrics and appends /debug/runtime.
	metricsURL := backend.URL + "/metrics"

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("svc-online").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver",
			"last_seen_at", "desired", "reported", "coalesce",
		}).AddRow("svc-online", "ai-gateway", "online", int64(3), int64(2),
			&now, desiredJSON, reportedJSON, metricsURL))

	a := &RuntimeBridgeAPI{
		DB:           mock,
		HubID:        "hub-1",
		ServiceToken: "svc-token",
		HTTPClient:   backend.Client(),
	}
	c, rec := makeRuntimeCtx(t, "svc-online")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; want 200 when proxy succeeds", rec.Code)
	}
}

// TestRuntime_HubSelf_LocalURL verifies that when the id matches hubID
// the hub-local URL is used directly.
func TestRuntime_HubSelf_LocalURL(t *testing.T) {
	// Fake local endpoint.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"self":true}`))
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

	mock.ExpectQuery(`SELECT t.id, t.type, t.status`).
		WithArgs("hub-self").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver",
			"last_seen_at", "desired", "reported", "coalesce",
		}).AddRow("hub-self", "nexus-hub", "online", int64(1), int64(1),
			&now, desiredJSON, reportedJSON, ""))

	a := &RuntimeBridgeAPI{
		DB:           mock,
		HubID:        "hub-self",
		HubLocalURL:  backend.URL,
		ServiceToken: "svc-token",
		HTTPClient:   backend.Client(),
	}
	c, rec := makeRuntimeCtx(t, "hub-self")
	if err := a.Runtime(c); err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status %d; want 200 for hub-self local path", rec.Code)
	}
}
