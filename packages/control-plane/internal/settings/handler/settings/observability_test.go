// Observability handler unit tests. The fan-out across ai-gateway,
// compliance-proxy, control-plane, and nexus-hub is load-bearing — a
// missed Thing type means the corresponding receiver never sees the
// admin write and silently runs stale. The receivers themselves are
// tested in their own packages; this test only locks the CP-side
// fan-out shape.
package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// fanoutSpy is the test-only HubInvalidator that records every
// InvalidateConfig call so the test can assert on the (thingType,
// configKey) tuples emitted by UpdateObservability.
type fanoutSpy struct {
	mu    sync.Mutex
	calls []fanoutCall
}

type fanoutCall struct {
	ThingType string
	ConfigKey string
}

func (f *fanoutSpy) InvalidateConfig(_ context.Context, thingType, configKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fanoutCall{ThingType: thingType, ConfigKey: configKey})
}

func (f *fanoutSpy) InvalidateConfigE(ctx context.Context, thingType, configKey string) error {
	f.InvalidateConfig(ctx, thingType, configKey)
	return nil
}

func (f *fanoutSpy) snapshot() []fanoutCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fanoutCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestUpdateObservability_FanoutCoversAllFourThingTypes locks the fan-out
// shape: every Thing type that has an observability receiver MUST get an
// InvalidateConfig call. The four legs are ai-gateway, compliance-proxy,
// control-plane, and nexus-hub. Agent is intentionally excluded (no
// observability config key registered for agent).
func TestUpdateObservability_FanoutCoversAllFourThingTypes(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	spy := &fanoutSpy{}
	h.hub = spy

	// GetSystemMetadata returns ErrNoRows so the merge starts from empty
	// (the merge path is exercised by separate tests below).
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("observability.config").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("observability.config", pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	body, _ := json.Marshal(map[string]any{"otelEnabled": true, "samplingRate": 0.5})
	req := jsonReq(http.MethodPut, "/settings/observability", string(body))
	rec := httptest.NewRecorder()
	c := adminCtx(req, rec, "user-1", "alice")

	if err := h.UpdateObservability(c); err != nil {
		t.Fatalf("UpdateObservability: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}

	got := spy.snapshot()
	if len(got) != 4 {
		t.Fatalf("fan-out call count = %d; want 4 (ai-gateway, compliance-proxy, control-plane, nexus-hub); got = %+v", len(got), got)
	}

	gotTypes := make([]string, 0, len(got))
	for _, c := range got {
		if c.ConfigKey != configkey.Observability {
			t.Errorf("unexpected configKey %q (want %q) in call %+v", c.ConfigKey, configkey.Observability, c)
		}
		gotTypes = append(gotTypes, c.ThingType)
	}
	sort.Strings(gotTypes)

	want := []string{"ai-gateway", "compliance-proxy", "control-plane", "nexus-hub"}
	for i := range want {
		if gotTypes[i] != want[i] {
			t.Errorf("fan-out target #%d = %q; want %q (full set: %v)", i, gotTypes[i], want[i], gotTypes)
		}
	}

	// Agent must NOT appear — no observability config key is registered for agent.
	for _, c := range got {
		if c.ThingType == "agent" {
			t.Errorf("agent must not appear in observability fan-out, got %+v", c)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestUpdateObservability_FanoutSkippedWhenHubNil locks the nil-Hub
// branch: a CP started without Hub wiring must persist the config but
// silently skip the fan-out (admin still receives 200; the receivers
// will pick up the new config at next reconnect).
func TestUpdateObservability_FanoutSkippedWhenHubNil(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	h.hub = nil // explicitly nil — handler must tolerate

	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("observability.config").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO system_metadata`).
		WithArgs("observability.config", pgxmock.AnyArg(), "user-1").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	body, _ := json.Marshal(map[string]any{"otelEnabled": true})
	req := jsonReq(http.MethodPut, "/settings/observability", string(body))
	rec := httptest.NewRecorder()
	c := adminCtx(req, rec, "user-1", "alice")

	if err := h.UpdateObservability(c); err != nil {
		t.Fatalf("UpdateObservability: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
}

// TestGetObservability_EmptyRowReturnsDefault locks the response shape
// for an unseeded row — the admin UI relies on `enabled: false` so the
// initial dashboard renders before any write has been made.
func TestGetObservability_EmptyRowReturnsDefault(t *testing.T) {
	mock, h, _ := newHandlerWithMock(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata`).
		WithArgs("observability.config").
		WillReturnError(pgx.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet, "/settings/observability", nil)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)

	if err := h.GetObservability(c); err != nil {
		t.Fatalf("GetObservability: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	if v, ok := m["enabled"].(bool); !ok || v {
		t.Errorf("default response: enabled = %v (want false); body=%s", m["enabled"], rec.Body.String())
	}
}

// TestUpdateObservability_SamplingRateValidation locks the inclusive
// [0,1] bound on samplingRate. The receiver downstream assumes a valid
// rate and will OTLP-export at that ratio; an out-of-range value lets
// the dashboard silently set the gateway to sample 200% or -50%.
func TestUpdateObservability_SamplingRateValidation(t *testing.T) {
	_, h, _ := newHandlerWithMock(t)
	h.hub = &fanoutSpy{}

	for _, badRate := range []float64{-0.1, 1.5, 2.0} {
		body, _ := json.Marshal(map[string]any{"samplingRate": badRate})
		req := jsonReq(http.MethodPut, "/settings/observability", string(body))
		rec := httptest.NewRecorder()
		c := adminCtx(req, rec, "user-1", "alice")
		_ = h.UpdateObservability(c)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("samplingRate=%v: status = %d; want 400", badRate, rec.Code)
		}
	}
}
