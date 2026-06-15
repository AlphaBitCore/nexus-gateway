package routing

// F-0099 regression: routing-rule writes must fail loud (HTTP 502) when the
// Category B invalidation push to Hub fails, so the data plane does not keep
// routing on stale rules while the UI reports success. Asserts the CP DB write
// committed (truth preserved), the response is 502 with the propagation_error
// envelope, and NO success audit row was enqueued.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func assertRoutingPropagationEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d; want 502; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v; raw=%s", err, rec.Body.String())
	}
	if body.Error.Type != "propagation_error" || body.Error.Code != "HUB_PROPAGATION_FAILED" {
		t.Errorf("envelope = %+v; want propagation_error/HUB_PROPAGATION_FAILED", body.Error)
	}
}

func TestCreateRoutingRule_HubFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.invalidateErr = errors.New("hub unreachable")
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`INSERT INTO "RoutingRule"`).
		WithArgs(anyN(10)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-new", "n", now)...))

	body := `{"name":"n","strategyType":"single","config":{"providerId":"p","modelId":"m"}}`
	c, rec := makeJSONReq(t, http.MethodPost, "/api/admin/routing-rules", body)
	if err := h.CreateRoutingRule(c); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	assertRoutingPropagationEnvelope(t, rec)
	if len(hub.invalidateCalls) != 1 || hub.invalidateCalls[0].ConfigKey != "routing_rules" {
		t.Errorf("hub invalidate = %+v; want 1× routing_rules", hub.invalidateCalls)
	}
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0 (must not log success on push failure)", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB insert did not commit before push: %v", err)
	}
}

func TestUpdateRoutingRule_HubFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.invalidateErr = errors.New("hub down")
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectQuery(`UPDATE "RoutingRule"`).
		WithArgs(anyN(12)...).
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "new", now)...))

	body := `{"name":"new","strategyType":"single","config":{"providerId":"p2"}}`
	c, rec := makeJSONReq(t, http.MethodPut, "/api/admin/routing-rules/rule-1", body)
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.UpdateRoutingRule(c); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	assertRoutingPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB update did not commit before push: %v", err)
	}
}

func TestDeleteRoutingRule_HubFailure502(t *testing.T) {
	h, mock, hub, aud := newHandlerWithMockDB(t)
	hub.invalidateErr = errors.New("hub down")
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM "RoutingRule" WHERE id = \$1`).
		WithArgs("rule-1").
		WillReturnRows(pgxmock.NewRows(routingRuleCols).AddRow(makeRRRow("rule-1", "old", now)...))
	mock.ExpectExec(`DELETE FROM "RoutingRule"`).
		WithArgs("rule-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	c, rec := makeJSONReq(t, http.MethodDelete, "/api/admin/routing-rules/rule-1", "")
	c.SetParamNames("id")
	c.SetParamValues("rule-1")
	if err := h.DeleteRoutingRule(c); err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}
	assertRoutingPropagationEnvelope(t, rec)
	if aud.count() != 0 {
		t.Errorf("audit count=%d; want 0", aud.count())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB delete did not commit before push: %v", err)
	}
}
