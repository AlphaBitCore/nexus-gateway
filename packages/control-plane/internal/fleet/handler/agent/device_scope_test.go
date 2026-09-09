package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	cpiam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// scopeLoader hands each principal a fixed policy set.
type scopeLoader struct{ policies []cpiam.LoadedPolicy }

func (l *scopeLoader) LoadPolicies(_ context.Context, _, _ string) ([]cpiam.LoadedPolicy, error) {
	return l.policies, nil
}

// groupsStub answers device-group membership. err makes the lookup fail, which
// must fail CLOSED (the device is treated as having no memberships, so a
// group-scoped grant cannot match).
type groupsStub struct {
	byDevice map[string][]string
	err      error
}

func (g *groupsStub) GroupsOfDevice(_ context.Context, deviceID string) ([]string, error) {
	if g.err != nil {
		return nil, g.err
	}
	return g.byDevice[deviceID], nil
}

var _ middleware.DeviceGroupLookup = (*groupsStub)(nil)

func policy(id string, effect string, actions, resources []string) cpiam.LoadedPolicy {
	return cpiam.LoadedPolicy{
		ID: id, Name: id, Source: "direct",
		Document: cpiam.PolicyDocument{
			Version:   cpiam.PolicyVersion,
			Statement: []cpiam.Statement{{Effect: effect, Action: actions, Resource: resources}},
		},
	}
}

// scopedHandler builds a handler wired the way production wires it: same engine
// and same group lookup the device-aware middleware uses.
func scopedHandler(mock pgxmock.PgxPoolIface, policies []cpiam.LoadedPolicy, groups middleware.DeviceGroupLookup) *Handler {
	return New(Deps{
		Pool:         mock,
		Hub:          &fakeHub{},
		Logger:       silentLogger(),
		Audit:        newAuditWriter(&auditSpy{}),
		IAM:          cpiam.NewEngine(&scopeLoader{policies: policies}, silentLogger()),
		DeviceGroups: groups,
	})
}

func listUserDevices(t *testing.T, h *Handler, mock pgxmock.PgxPoolIface) *httptest.ResponseRecorder {
	t.Helper()
	now := nowFixture()
	mock.ExpectQuery(`COUNT\(\*\)\s+FROM "DeviceAssignment"`).WithArgs("u-1").
		WillReturnRows(pgxmock.NewRows([]string{"c"}).AddRow(1))
	mock.ExpectQuery(`FROM "DeviceAssignment" da\s+JOIN thing t`).WithArgs("u-1", 50, 0).
		WillReturnRows(pgxmock.NewRows(fleetUserDeviceCols).AddRow(makeFleetUserDeviceRow(now)...))

	req := httptest.NewRequest(http.MethodGet, "/agent-users/u-1/devices", nil)
	rec := httptest.NewRecorder()
	c, e := echoCtxAdmin(req, rec, "caller-1")
	e.GET("/agent-users/:id/devices", h.ListAgentUserDevices)
	c.SetParamNames("id")
	c.SetParamValues("u-1")
	if err := h.ListAgentUserDevices(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	return rec
}

func rowCount(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	return len(body.Data)
}

// TestListAgentUserDevices_GroupScopedDenyIsHonoured is the leak.
//
// This route is keyed on a USER, so no device-aware middleware can gate it —
// the device ids only exist after the query. A caller holding an unscoped Allow
// plus a device-group-scoped Deny passes the route gate (the Deny does not match
// a wildcard target) and would then read every one of that user's devices,
// including the ones the Deny exists to withhold.
func TestListAgentUserDevices_GroupScopedDenyIsHonoured(t *testing.T) {
	mock := newMockPool(t)
	h := scopedHandler(mock,
		[]cpiam.LoadedPolicy{
			policy("wide", "Allow", []string{"admin:agent-device.read"}, []string{"nrn:nexus:*:*:*/*"}),
			policy("scoped-deny", "Deny", []string{"admin:agent-device.read"},
				[]string{"nrn:nexus:agent:*:agent-device/group:g-restricted/*"}),
		},
		&groupsStub{byDevice: map[string][]string{"agent-1": {"g-restricted"}}},
	)

	rec := listUserDevices(t, h, mock)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := rowCount(t, rec); n != 0 {
		t.Fatalf("%d device row(s) returned for a caller whose group-scoped Deny covers them — the listing ignored the scope", n)
	}
}

// TestListAgentUserDevices_AllowedDeviceStillReturned pins that the fix does not
// read as "return nothing". A caller the policy permits must still see the row.
func TestListAgentUserDevices_AllowedDeviceStillReturned(t *testing.T) {
	mock := newMockPool(t)
	h := scopedHandler(mock,
		[]cpiam.LoadedPolicy{
			policy("wide", "Allow", []string{"admin:agent-device.read"}, []string{"nrn:nexus:*:*:*/*"}),
		},
		&groupsStub{byDevice: map[string][]string{"agent-1": {"g-allowed"}}},
	)

	rec := listUserDevices(t, h, mock)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := rowCount(t, rec); n != 1 {
		t.Fatalf("got %d rows, want 1 — an authorised caller must still see the device", n)
	}
}

// TestListAgentUserDevices_GroupLookupErrorFailsClosed — a lookup outage must
// not authorise. With memberships unknown, only the unscoped candidate remains,
// so a caller whose ONLY grant is group-scoped is denied rather than admitted.
func TestListAgentUserDevices_GroupLookupErrorFailsClosed(t *testing.T) {
	mock := newMockPool(t)
	h := scopedHandler(mock,
		[]cpiam.LoadedPolicy{
			policy("group-only", "Allow", []string{"admin:agent-device.read"},
				[]string{"nrn:nexus:agent:*:agent-device/group:g-allowed/*"}),
		},
		&groupsStub{err: errors.New("group lookup outage")},
	)

	rec := listUserDevices(t, h, mock)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := rowCount(t, rec); n != 0 {
		t.Fatalf("%d row(s) returned while device-group membership was unknown — the outage authorised the read", n)
	}
}

// TestListAgentUserDevices_NoEngineRefusesRatherThanServingUnscoped — a handler
// built without an engine cannot evaluate the scope, so it must refuse the
// listing instead of serving it wide open. This is the arm that makes the
// dependency load-bearing rather than optional.
func TestListAgentUserDevices_NoEngineRefusesRatherThanServingUnscoped(t *testing.T) {
	mock := newMockPool(t)
	h := New(Deps{Pool: mock, Hub: &fakeHub{}, Logger: silentLogger(), Audit: newAuditWriter(&auditSpy{})})

	rec := listUserDevices(t, h, mock)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code=%d — with no IAM engine the listing must refuse, not serve unscoped; body=%s", rec.Code, rec.Body.String())
	}
}

var _ = echo.New
