package infra

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	shariam "github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// F-0100: POST /config-sync/update must be gated on the node-level IAM action
// (admin:node.update), matching the node.update audit row the handler stamps,
// not the generic admin:settings.update. We assert by capturing the action
// string RegisterNodeRoutes passes to iamMW for the config-sync/update route.
func TestConfigSyncUpdate_GatedOnNodeUpdate(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	e := echo.New()
	g := e.Group("/api/admin")

	// Capturing iamMW: stamps the gated action on the response so a route
	// probe reveals exactly which IAM action guards it.
	iamMW := func(action string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Response().Header().Set("X-Gated-Action", action)
				// Short-circuit before the real handler runs so we don't need a
				// live Hub — the action header is all this test inspects.
				return c.NoContent(http.StatusTeapot)
			}
		}
	}
	h.RegisterNodeRoutes(g, iamMW)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/config-sync/update", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Gated-Action")
	want := shariam.ResourceNode.Action(shariam.VerbUpdate)
	if got != want {
		t.Errorf("config-sync/update gated on %q; want %q", got, want)
	}
	if got == shariam.ResourceSettings.Action(shariam.VerbUpdate) {
		t.Errorf("config-sync/update still gated on settings.update (F-0100 regression)")
	}
}

// F-0100 sibling guard: the config-sync READ surface stays on settings.read —
// it is the generic fleet-config inspection view, not a node mutation.
func TestConfigSyncReads_GatedOnSettingsRead(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	e := echo.New()
	g := e.Group("/api/admin")
	iamMW := func(action string) echo.MiddlewareFunc {
		return func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Response().Header().Set("X-Gated-Action", action)
				return c.NoContent(http.StatusTeapot)
			}
		}
	}
	h.RegisterNodeRoutes(g, iamMW)

	for _, path := range []string{
		"/api/admin/config-sync/out-of-sync",
		"/api/admin/config-sync/history",
		"/api/admin/config-sync/catalog",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if got, want := rec.Header().Get("X-Gated-Action"), shariam.ResourceSettings.Action(shariam.VerbRead); got != want {
			t.Errorf("%s gated on %q; want %q", path, got, want)
		}
	}
}

// F-0104: PATCH /setup/proxy/:thingId/onboarding must reject an unknown thingId
// with 404 (propagated from Hub's GetThingServiceMeta "not found") before
// pushing any type-wide config, rather than silently pushing under a bogus path
// param and returning 200.
func TestSetupPatchOnboarding_UnknownThingId404(t *testing.T) {
	fh := &fakeHub{
		baseURL:        "http://hub",
		serviceMetaErr: errors.New("hubclient: thing t-missing not found"),
	}
	spy := &auditSpy{}
	h := newHandler(t, nil, fh, spy)
	c, rec := echoCtxParam(
		http.MethodPatch, "/api/admin/setup/proxy/t-missing/onboarding",
		`{"enabled":true}`, true,
		[]string{"thingId"}, []string{"t-missing"},
	)
	if err := h.SetupPatchOnboarding(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d; want 404; body %s", rec.Code, rec.Body.String())
	}
	// No config push and no audit when the thing does not exist.
	if fh.notifyHits != 0 {
		t.Errorf("NotifyConfigChange called %d times for unknown thing; want 0", fh.notifyHits)
	}
	if spy.count() != 0 {
		t.Errorf("audit emitted %d times for unknown thing; want 0", spy.count())
	}
}

// F-0105 + F-0106: a successful onboarding toggle (a) emits exactly one audit
// row and (b) stamps the admin actor + source IP on the Hub config push.
func TestSetupPatchOnboarding_Success_AuditsAndStampsActorSourceIP(t *testing.T) {
	fh := &fakeHub{
		baseURL:     "http://hub",
		serviceMeta: &hub.ThingServiceMeta{ThingID: "t-1", ManagementURL: "http://proxy"},
		notifyResp:  &hub.ConfigChangeResponse{OK: true, Version: 7},
	}
	spy := &auditSpy{}
	h := newHandler(t, nil, fh, spy)

	c, rec := echoCtxParam(
		http.MethodPatch, "/api/admin/setup/proxy/t-1/onboarding",
		`{"enabled":true}`, true,
		[]string{"thingId"}, []string{"t-1"},
	)
	// Echo derives RealIP from RemoteAddr when no proxy headers are present.
	c.Request().RemoteAddr = "203.0.113.9:51000"

	if err := h.SetupPatchOnboarding(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200; body %s", rec.Code, rec.Body.String())
	}

	// F-0106: actor + source IP stamped on the push.
	if fh.notifyReq.ActorID != "admin-1" {
		t.Errorf("ActorID = %q; want admin-1", fh.notifyReq.ActorID)
	}
	if fh.notifyReq.ActorName != "Alice" {
		t.Errorf("ActorName = %q; want Alice", fh.notifyReq.ActorName)
	}
	if fh.notifyReq.SourceIP != "203.0.113.9" {
		t.Errorf("SourceIP = %q; want 203.0.113.9", fh.notifyReq.SourceIP)
	}
	if fh.notifyReq.ThingType != "compliance-proxy" {
		t.Errorf("ThingType = %q; want compliance-proxy", fh.notifyReq.ThingType)
	}

	// F-0105: exactly one audit row, recording the entity + new state.
	if spy.count() != 1 {
		t.Fatalf("audit count = %d; want 1", spy.count())
	}
	entry := spy.last()
	if entry["entityId"] != "t-1" {
		t.Errorf("audit entityId = %v; want t-1", entry["entityId"])
	}
}

// F-0105 negative: a failed Hub push must NOT emit an audit row (the audit
// only lands when the user-visible mutation is durable, matching siblings).
func TestSetupPatchOnboarding_HubPushFails_NoAudit(t *testing.T) {
	fh := &fakeHub{
		baseURL:     "http://hub",
		serviceMeta: &hub.ThingServiceMeta{ThingID: "t-1", ManagementURL: "http://proxy"},
		notifyErr:   errors.New("hub down"),
	}
	spy := &auditSpy{}
	h := newHandler(t, nil, fh, spy)
	c, rec := echoCtxParam(
		http.MethodPatch, "/api/admin/setup/proxy/t-1/onboarding",
		`{"enabled":false}`, true,
		[]string{"thingId"}, []string{"t-1"},
	)
	if err := h.SetupPatchOnboarding(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d; want 502", rec.Code)
	}
	if spy.count() != 0 {
		t.Errorf("audit emitted %d times on hub failure; want 0", spy.count())
	}
}

// F-0100 configKey allowlist: ConfigSyncUpdate must reject sensitive configKeys
// that have dedicated endpoints with narrower IAM verbs and additional business
// rules. The generic update path must not bypass those surfaces.

// TestConfigSyncUpdate_AllowedKey_ForwardedToHub asserts that a safe configKey
// (e.g., log_level) is accepted and forwarded. The Hub HTTP call uses an
// httptest server so no live Hub is needed.
func TestConfigSyncUpdate_AllowedKey_ForwardedToHub(t *testing.T) {
	// Hub stub: accept the POST and return 200 with a counter response.
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"updated":1,"notified":1}`))
	}))
	defer hubSrv.Close()

	fh := &fakeHub{baseURL: hubSrv.URL, token: "tok"}
	h := newHandler(t, nil, fh, nil)
	// Inject the httptest server's client so hubForward dials the stub.
	h.hubProxyClientRef = hubSrv.Client()

	body := `{"nodeType":"ai-gateway","configKey":"` + configkey.LogLevel + `","state":{"level":"debug"}}`
	c, rec := echoCtx(http.MethodPost, "/api/admin/config-sync/update", body, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d; want 200 for allowed key; body: %s", rec.Code, rec.Body.String())
	}
}

// TestConfigSyncUpdate_KillswitchKeyRejected asserts that the killswitch
// configKey is rejected with 400 and a meaningful error message.
func TestConfigSyncUpdate_KillswitchKeyRejected(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	body := `{"nodeType":"compliance-proxy","configKey":"` + configkey.Killswitch + `","state":{"engaged":true}}`
	c, rec := echoCtx(http.MethodPost, "/api/admin/config-sync/update", body, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for killswitch key", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CONFIG_KEY_DENIED") {
		t.Errorf("want CONFIG_KEY_DENIED in body; got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "killswitch") {
		t.Errorf("want configKey name in body; got: %s", rec.Body.String())
	}
}

// TestConfigSyncUpdate_GatewayPassthroughKeyRejected asserts that the
// gateway_passthrough configKey is rejected via the generic endpoint.
func TestConfigSyncUpdate_GatewayPassthroughKeyRejected(t *testing.T) {
	h := newHandler(t, nil, &fakeHub{}, nil)
	body := `{"nodeType":"ai-gateway","configKey":"` + configkey.GatewayPassthrough + `","state":{}}`
	c, rec := echoCtx(http.MethodPost, "/api/admin/config-sync/update", body, true)
	if err := h.ConfigSyncUpdate(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d; want 400 for gateway_passthrough key", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CONFIG_KEY_DENIED") {
		t.Errorf("want CONFIG_KEY_DENIED in body; got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_passthrough") {
		t.Errorf("want configKey name in body; got: %s", rec.Body.String())
	}
}
