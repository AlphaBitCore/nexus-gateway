package hubapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/thingtype"
)

// TestRequireMutationAuthority is the SEC-W2-02 (FIX-5/C C2) unit regression for
// the device-mutation gate. It pins both halves of the closed invariant:
//
//   - device-token callers are bound to their OWN id (unchanged from
//     requireThingMatch);
//   - SERVICE-token callers (thing == nil) may operate only on a
//     backend-service Thing, NEVER an agent — so a leaked fleet-shared service
//     token can no longer act as an arbitrary agent (forge its shadow, flip its
//     kill-switch via break-glass, deregister it).
func TestRequireMutationAuthority(t *testing.T) {
	e := newTestEcho()

	// Device-token caller operating on its own id — allowed.
	t.Run("device_self_allowed", func(t *testing.T) {
		h := &InternalThingsAPI{}
		c, _ := echoCtxJSON(e, http.MethodPost, nil, nil)
		c.Set(thingContextKey, &store.Thing{ID: "agent-1"})
		if h.requireMutationAuthority(c, "agent-1", "") {
			t.Fatal("device caller on its own id must not be blocked")
		}
	})

	// Device-token caller operating on another id — blocked (cross-Thing).
	t.Run("device_cross_thing_blocked", func(t *testing.T) {
		h := &InternalThingsAPI{}
		c, rec := echoCtxJSON(e, http.MethodPost, nil, nil)
		c.Set(thingContextKey, &store.Thing{ID: "agent-1"})
		if !h.requireMutationAuthority(c, "agent-2", "") {
			t.Fatal("device caller on another id must be blocked")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", rec.Code)
		}
	})

	// Service-token caller, type known from hint (register body) — a service
	// type passes, an agent type is refused, with NO DB lookup.
	t.Run("service_hint_service_type_allowed", func(t *testing.T) {
		h := &InternalThingsAPI{}
		c, _ := echoCtxJSON(e, http.MethodPost, nil, nil) // no Thing set => service token
		if h.requireMutationAuthority(c, "ai-gw-1", thingtype.AIGateway) {
			t.Fatal("service token self-registering a service-type Thing must be allowed")
		}
	})
	t.Run("service_hint_agent_type_blocked", func(t *testing.T) {
		h := &InternalThingsAPI{}
		c, rec := echoCtxJSON(e, http.MethodPost, nil, nil)
		if !h.requireMutationAuthority(c, "agent-1", thingtype.Agent) {
			t.Fatal("service token registering an agent-type Thing must be blocked")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", rec.Code)
		}
	})

	// Service-token caller, type resolved by lookup (heartbeat/shadow/etc.).
	t.Run("service_lookup_service_type_allowed", func(t *testing.T) {
		h, mock := newInternalAPIMock(t)
		mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg()).WillReturnRows(oneThingRow("cp-1", thingtype.ControlPlane))
		c, _ := echoCtxJSON(e, http.MethodPost, nil, nil)
		if h.requireMutationAuthority(c, "cp-1", "") {
			t.Fatal("service token operating on a service-type Thing must be allowed")
		}
	})
	t.Run("service_lookup_agent_type_blocked", func(t *testing.T) {
		h, mock := newInternalAPIMock(t)
		mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg()).WillReturnRows(oneThingRow("agent-9", thingtype.Agent))
		c, rec := echoCtxJSON(e, http.MethodPost, nil, nil)
		if !h.requireMutationAuthority(c, "agent-9", "") {
			t.Fatal("service token operating on an agent Thing must be blocked")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", rec.Code)
		}
	})

	// Service-token caller targeting an unresolvable id — fail closed.
	t.Run("service_lookup_unknown_blocked", func(t *testing.T) {
		h, mock := newInternalAPIMock(t)
		mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg()).WillReturnError(errors.New("sql: no rows in result set"))
		c, rec := echoCtxJSON(e, http.MethodPost, nil, nil)
		if !h.requireMutationAuthority(c, "ghost", "") {
			t.Fatal("service token targeting an unknown Thing must be blocked (fail closed)")
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d want 403", rec.Code)
		}
	})
}

// TestInternalThingsAPI_ServiceTokenImpersonatingAgent_Returns403 is the
// handler-level wiring guard: every device-mutation handler actually calls the
// gate, so a service-token caller (thing == nil) targeting an AGENT Thing is
// refused. Register carries the type in its body (no lookup); the rest resolve
// the type via a single GetThing the mock answers with an agent row.
func TestInternalThingsAPI_ServiceTokenImpersonatingAgent_Returns403(t *testing.T) {
	e := newTestEcho()
	const victim = "agent-victim"

	// Register: type in body, no lookup, bare handler.
	t.Run("Register", func(t *testing.T) {
		h := &InternalThingsAPI{}
		c, rec := echoCtxJSON(e, http.MethodPost, map[string]any{"id": victim, "type": thingtype.Agent}, nil)
		// service token => no Thing in context
		_ = h.Register(c)
		assertForbidden(t, rec)
	})

	// Lookup-path mutations: each resolves the operated Thing's type via GetThing.
	lookupCases := []struct {
		name   string
		body   map[string]any
		invoke func(h *InternalThingsAPI, c echo.Context) error
	}{
		{"Heartbeat", map[string]any{"id": victim, "status": "online"},
			func(h *InternalThingsAPI, c echo.Context) error { return h.Heartbeat(c) }},
		{"ShadowReport", map[string]any{"id": victim, "reported": map[string]any{}, "reportedVer": 0},
			func(h *InternalThingsAPI, c echo.Context) error { return h.ShadowReport(c) }},
		{"BreakGlassReport", map[string]any{"id": victim, "reported": map[string]any{"killswitch": map[string]any{"engaged": true}}, "reportedVer": 4, "keyVersions": map[string]any{"killswitch": 4}, "actorTokenId": "a1b2c3d4"},
			func(h *InternalThingsAPI, c echo.Context) error { return h.BreakGlassReport(c) }},
		{"Deregister", map[string]any{"id": victim},
			func(h *InternalThingsAPI, c echo.Context) error { return h.Deregister(c) }},
		{"ExemptionUpload", map[string]any{"thingId": victim, "host": "x.example", "expiresAt": "2999-01-01T00:00:00Z"},
			func(h *InternalThingsAPI, c echo.Context) error { return h.ExemptionUpload(c) }},
	}
	for _, tc := range lookupCases {
		t.Run(tc.name, func(t *testing.T) {
			h, mock := newInternalAPIMock(t)
			mock.ExpectQuery(`SELECT`).WithArgs(pgxmock.AnyArg()).WillReturnRows(oneThingRow(victim, thingtype.Agent))
			c, rec := echoCtxJSON(e, http.MethodPost, tc.body, nil)
			// service token => no Thing in context
			_ = tc.invoke(h, c)
			assertForbidden(t, rec)
		})
	}
}

func assertForbidden(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 (service token must not impersonate an agent); body=%s", rec.Code, rec.Body.String())
	}
}
