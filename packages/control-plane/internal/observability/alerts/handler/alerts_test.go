package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// per R6 runbook §4.2 option 1; promoted to handler/util/ when the 4th
// domain extraction lands). ---

// auditSpy captures MQ enqueues so tests can assert admin audit entries.
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := make([]byte, len(data))
	copy(copied, data)
	a.calls = append(a.calls, copied)
	return nil
}
func (a *auditSpy) Close() error { return nil }

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *auditSpy) last() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(a.calls[len(a.calls)-1], &m)
	return m
}

// echoContext builds an Echo context with the authenticated admin
// principal populated under the same key the real AdminAuth middleware
// uses.
func echoContext(req *http.Request, rec *httptest.ResponseRecorder, userName, userID string) echo.Context {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           userName,
		AuthPrincipalType: "admin_user",
	})
	return c
}

// newAlertsHandler creates an alerts.Handler whose Hub field points at
// the given fake Hub URL. A real hub.Client is used so the
// Forward path exercises the actual HTTP machinery.
func newAlertsHandler(t *testing.T, hubURL string) *Handler {
	t.Helper()
	return New(Deps{
		Hub:    hub.New(hubURL, "svc-token", nil, slog.Default()),
		Logger: slog.Default(),
		Audit:  audit.NewWriter(nil, "", slog.Default()),
	})
}

// TestAlertsIAM_ViewerDeniedOnAck verifies that a route wired with
// iamMW(iam.ResourceAlert.Action(iam.VerbAcknowledge)) rejects a
// principal that holds no policies. The IAM engine is wired with a nil
// loader (no policies loaded) so every action defaults to deny — which
// is the deny-by-default posture the engine enforces when no Allow
// statement matches.
func TestAlertsIAM_ViewerDeniedOnAck(t *testing.T) {
	var hubHit bool
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer stub.Close()

	eng := iam.NewEngine(nil, slog.Default())
	h := New(Deps{
		Hub:    hub.New(stub.URL, "svc-token", nil, slog.Default()),
		Logger: slog.Default(),
		Audit:  audit.NewWriter(nil, "", slog.Default()),
	})

	e := echo.New()
	iamMW := func(action string) echo.MiddlewareFunc {
		return middleware.RequireIAMPermission(eng, action, nil)
	}
	g := e.Group("/api/admin")
	h.RegisterRoutes(g, iamMW)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/alert-1/ack", nil)
	rec := httptest.NewRecorder()
	// No admin auth on context → iam engine gets nil principal → denies.
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401/403, got %d; body=%s", rec.Code, rec.Body.String())
	}
	if hubHit {
		t.Fatal("Hub was called despite IAM denial — handler should not have been reached")
	}
}

// TestListAlerts_QueryStringForwarded verifies that query params the
// admin UI sends are forwarded verbatim to Hub's ListAlerts endpoint.
func TestListAlerts_QueryStringForwarded(t *testing.T) {
	var capturedPath string
	var capturedQuery string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"alerts":[],"total":0}`)
	}))
	defer stub.Close()

	h := newAlertsHandler(t, stub.URL)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/api/admin/alerts?state=firing&severity=high&offset=0&limit=20", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.ListAlerts(c); err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if capturedPath != "/api/v1/admin/alerts" {
		t.Errorf("hub path = %q; want /api/v1/admin/alerts", capturedPath)
	}
	parsedReq, err := http.NewRequest(http.MethodGet, "http://x/?"+capturedQuery, nil)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	q := parsedReq.URL.Query()
	if q.Get("state") != "firing" {
		t.Errorf("state = %q; want firing", q.Get("state"))
	}
	if q.Get("severity") != "high" {
		t.Errorf("severity = %q; want high", q.Get("severity"))
	}
}

// TestTestAlertChannel_ActorHeaderForwarded verifies that POST
// /api/admin/alerts/channels/:id/test carries X-Nexus-Actor-User-Id
// with the authenticated admin's ID, and that the request body is
// forwarded unchanged.
func TestTestAlertChannel_ActorHeaderForwarded(t *testing.T) {
	var capturedActorID string
	var capturedPath string
	var capturedBody []byte
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedActorID = r.Header.Get("X-Nexus-Actor-User-Id")
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"statusCode":200,"dispatchId":"d-1"}`)
	}))
	defer stub.Close()

	h := newAlertsHandler(t, stub.URL)

	reqBody := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/alerts/channels/c-1/test", bytes.NewReader(reqBody))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("c-1")

	if err := h.TestAlertChannel(c); err != nil {
		t.Fatalf("TestAlertChannel: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if capturedPath != "/api/v1/admin/alerts/channels/c-1/test" {
		t.Errorf("hub path = %q; want /api/v1/admin/alerts/channels/c-1/test", capturedPath)
	}
	if capturedActorID != "user-1" {
		t.Errorf("X-Nexus-Actor-User-Id = %q; want user-1", capturedActorID)
	}
	if string(capturedBody) != string(reqBody) {
		t.Errorf("body = %q; want %q", capturedBody, reqBody)
	}
}

// TestAckAlert_AdminAuditOn2xx verifies a successful Hub ack enqueues
// admin audit.
func TestAckAlert_AdminAuditOn2xx(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/alerts/alert-1/ack" || r.Method != http.MethodPost {
			t.Errorf("hub path = %q %s; want POST .../alert-1/ack", r.URL.Path, r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer stub.Close()

	spy := &auditSpy{}
	h := New(Deps{
		Hub:    hub.New(stub.URL, "svc-token", nil, slog.Default()),
		Logger: slog.Default(),
		Audit:  audit.NewWriter(spy, "audit", slog.Default()),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/alert-1/ack", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("alert-1")

	if err := h.AckAlert(c); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", rec.Code)
	}
	if spy.count() != 1 {
		t.Fatalf("audit count = %d; want 1", spy.count())
	}
	entry := spy.last()
	if entry["action"] != "acknowledge" || entry["entityType"] != "alert" || entry["entityId"] != "alert-1" {
		t.Fatalf("audit mismatch: %+v", entry)
	}
}

// TestAckAlert_NoAdminAuditOnHubError verifies failed Hub responses
// skip audit.
func TestAckAlert_NoAdminAuditOnHubError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"conflict"}`)
	}))
	defer stub.Close()

	spy := &auditSpy{}
	h := New(Deps{
		Hub:    hub.New(stub.URL, "svc-token", nil, slog.Default()),
		Logger: slog.Default(),
		Audit:  audit.NewWriter(spy, "audit", slog.Default()),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/alert-1/ack", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("alert-1")

	if err := h.AckAlert(c); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409", rec.Code)
	}
	if spy.count() != 0 {
		t.Fatalf("audit count = %d; want 0 on hub error", spy.count())
	}
}

// a stub Hub. Verifies HTTP method + Hub path + (for mutating handlers)
// audit verb/sub-entity stamping. ---

// hubStub captures method + path of each request and serves a canned
// status + body, simplifying the per-handler tests below.
type hubStub struct {
	method string
	path   string
	status int
	body   string
	mu     sync.Mutex
	hits   int
}

func (s *hubStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.method = r.Method
		s.path = r.URL.Path
		s.hits++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if s.status == 0 {
			s.status = http.StatusOK
		}
		w.WriteHeader(s.status)
		if s.body != "" {
			_, _ = io.WriteString(w, s.body)
		}
	}
}

// TestReadHandlersForwardCorrectPath verifies the five read-only
// handlers (GetAlert, ListAlertRules, GetAlertRule, ListAlertChannels,
// GetAlertChannel) hit the matching Hub path with GET.
func TestReadHandlersForwardCorrectPath(t *testing.T) {
	tests := []struct {
		name     string
		invoke   func(h *Handler, c echo.Context) error
		setParam string
		wantPath string
	}{
		{
			name:     "GetAlert",
			invoke:   (*Handler).GetAlert,
			setParam: "alert-7",
			wantPath: "/api/v1/admin/alerts/alert-7",
		},
		{
			name:     "ListAlertRules",
			invoke:   (*Handler).ListAlertRules,
			wantPath: "/api/v1/admin/alerts/rules",
		},
		{
			name:     "GetAlertRule",
			invoke:   (*Handler).GetAlertRule,
			setParam: "rule-9",
			wantPath: "/api/v1/admin/alerts/rules/rule-9",
		},
		{
			name:     "ListAlertChannels",
			invoke:   (*Handler).ListAlertChannels,
			wantPath: "/api/v1/admin/alerts/channels",
		},
		{
			name:     "GetAlertChannel",
			invoke:   (*Handler).GetAlertChannel,
			setParam: "ch-3",
			wantPath: "/api/v1/admin/alerts/channels/ch-3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &hubStub{body: `{"ok":true}`}
			srv := httptest.NewServer(stub.handler(t))
			defer srv.Close()

			h := newAlertsHandler(t, srv.URL)

			req := httptest.NewRequest(http.MethodGet, "/api/admin"+tc.wantPath, nil)
			rec := httptest.NewRecorder()
			c := echoContext(req, rec, "alice", "user-1")
			if tc.setParam != "" {
				c.SetParamNames("id")
				c.SetParamValues(tc.setParam)
			}

			if err := tc.invoke(h, c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
			}
			if stub.method != http.MethodGet {
				t.Errorf("method = %q; want GET", stub.method)
			}
			if stub.path != tc.wantPath {
				t.Errorf("path = %q; want %q", stub.path, tc.wantPath)
			}
		})
	}
}

// TestMutatingHandlersForwardAndAudit verifies every mutating handler
// uses the correct HTTP method + Hub path AND records an admin audit
// entry whose verb / sub-entity / entityID matches the catalog mapping
// in alerts.go.
func TestMutatingHandlersForwardAndAudit(t *testing.T) {
	tests := []struct {
		name       string
		invoke     func(h *Handler, c echo.Context) error
		setParam   string
		wantMethod string
		wantPath   string
		wantVerb   string
		wantSubEnt string
		wantEntID  string
		stubStatus int
		stubBody   string
		wantReplay bool
	}{
		{
			name:       "ResolveAlert",
			invoke:     (*Handler).ResolveAlert,
			setParam:   "alert-5",
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/admin/alerts/alert-5/resolve",
			wantVerb:   "acknowledge",
			wantSubEnt: "alert",
			wantEntID:  "alert-5",
			stubStatus: http.StatusNoContent,
		},
		{
			name:       "UpdateAlertRule",
			invoke:     (*Handler).UpdateAlertRule,
			setParam:   "rule-1",
			wantMethod: http.MethodPut,
			wantPath:   "/api/v1/admin/alerts/rules/rule-1",
			wantVerb:   "update",
			wantSubEnt: "alertRule",
			wantEntID:  "rule-1",
			stubStatus: http.StatusOK,
			stubBody:   `{"id":"rule-1","enabled":true}`,
			wantReplay: true,
		},
		{
			name:       "ResetAlertRule",
			invoke:     (*Handler).ResetAlertRule,
			setParam:   "rule-1",
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/admin/alerts/rules/rule-1/reset",
			wantVerb:   "update",
			wantSubEnt: "alertRule",
			wantEntID:  "rule-1",
			stubStatus: http.StatusNoContent,
		},
		{
			name:       "CreateAlertChannel",
			invoke:     (*Handler).CreateAlertChannel,
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/admin/alerts/channels",
			wantVerb:   "create",
			wantSubEnt: "alertChannel",
			wantEntID:  "",
			stubStatus: http.StatusCreated,
			stubBody:   `{"id":"ch-new"}`,
			wantReplay: true,
		},
		{
			name:       "UpdateAlertChannel",
			invoke:     (*Handler).UpdateAlertChannel,
			setParam:   "ch-2",
			wantMethod: http.MethodPut,
			wantPath:   "/api/v1/admin/alerts/channels/ch-2",
			wantVerb:   "update",
			wantSubEnt: "alertChannel",
			wantEntID:  "ch-2",
			stubStatus: http.StatusOK,
			stubBody:   `{"id":"ch-2"}`,
			wantReplay: true,
		},
		{
			name:       "DeleteAlertChannel",
			invoke:     (*Handler).DeleteAlertChannel,
			setParam:   "ch-2",
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v1/admin/alerts/channels/ch-2",
			wantVerb:   "delete",
			wantSubEnt: "alertChannel",
			wantEntID:  "ch-2",
			stubStatus: http.StatusNoContent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &hubStub{status: tc.stubStatus, body: tc.stubBody}
			srv := httptest.NewServer(stub.handler(t))
			defer srv.Close()

			spy := &auditSpy{}
			h := New(Deps{
				Hub:    hub.New(srv.URL, "svc-token", nil, slog.Default()),
				Logger: slog.Default(),
				Audit:  audit.NewWriter(spy, "audit", slog.Default()),
			})

			var body io.Reader
			if tc.wantMethod == http.MethodPost || tc.wantMethod == http.MethodPut {
				body = bytes.NewReader([]byte(`{}`))
			}
			req := httptest.NewRequest(tc.wantMethod, "/api/admin"+tc.wantPath, body)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := echoContext(req, rec, "alice", "user-1")
			if tc.setParam != "" {
				c.SetParamNames("id")
				c.SetParamValues(tc.setParam)
			}

			if err := tc.invoke(h, c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if rec.Code != tc.stubStatus {
				t.Fatalf("status = %d; want %d; body=%s", rec.Code, tc.stubStatus, rec.Body.String())
			}
			if stub.method != tc.wantMethod {
				t.Errorf("hub method = %q; want %q", stub.method, tc.wantMethod)
			}
			if stub.path != tc.wantPath {
				t.Errorf("hub path = %q; want %q", stub.path, tc.wantPath)
			}
			if tc.wantReplay && rec.Body.String() != tc.stubBody {
				t.Errorf("response body = %q; want %q", rec.Body.String(), tc.stubBody)
			}

			if spy.count() != 1 {
				t.Fatalf("audit count = %d; want 1", spy.count())
			}
			entry := spy.last()
			if entry["action"] != tc.wantVerb {
				t.Errorf("audit verb = %v; want %s", entry["action"], tc.wantVerb)
			}
			if entry["entityId"] != tc.wantEntID {
				t.Errorf("audit entityId = %v; want %q", entry["entityId"], tc.wantEntID)
			}
			afterState, _ := entry["afterState"].(map[string]any)
			if afterState == nil {
				t.Fatalf("afterState missing: %+v", entry)
			}
			if afterState["subEntity"] != tc.wantSubEnt {
				t.Errorf("afterState.subEntity = %v; want %s", afterState["subEntity"], tc.wantSubEnt)
			}
			if afterState["method"] != tc.wantMethod {
				t.Errorf("afterState.method = %v; want %s", afterState["method"], tc.wantMethod)
			}
			if afterState["hubPath"] != tc.wantPath {
				t.Errorf("afterState.hubPath = %v; want %s", afterState["hubPath"], tc.wantPath)
			}
		})
	}
}

// stubHub is a HubBaseURLToken implementation used to exercise the
// hub-not-configured branch by returning an empty BaseURL.
type stubHub struct {
	baseURL string
	token   string
}

func (s *stubHub) BaseURL() string { return s.baseURL }
func (s *stubHub) Token() string   { return s.token }

// TestHubNotConfigured_Read covers the early-return 503 when hub.BaseURL()
// is empty on the read-only forward path.
func TestHubNotConfigured_Read(t *testing.T) {
	h := New(Deps{
		Hub:    &stubHub{},
		Logger: slog.Default(),
		Audit:  audit.NewWriter(nil, "", slog.Default()),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/alerts", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")

	if err := h.ListAlerts(c); err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("HUB_NOT_CONFIGURED")) {
		t.Errorf("body missing HUB_NOT_CONFIGURED: %s", rec.Body.String())
	}
}

// TestHubNotConfigured_Mutating covers the same early-return on the
// mutating forward path. Audit must NOT fire.
func TestHubNotConfigured_Mutating(t *testing.T) {
	spy := &auditSpy{}
	h := New(Deps{
		Hub:    &stubHub{},
		Logger: slog.Default(),
		Audit:  audit.NewWriter(spy, "audit", slog.Default()),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/alert-1/ack", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("alert-1")

	if err := h.AckAlert(c); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	if spy.count() != 0 {
		t.Fatalf("audit count = %d; want 0", spy.count())
	}
}

// TestHubUnreachable_Read covers the 502 returned when the Hub HTTP
// client errors (server closed, connection refused).
func TestHubUnreachable_Read(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	stub.Close() // immediately close → subsequent dial fails

	h := newAlertsHandler(t, stub.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/alerts", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")

	if err := h.ListAlerts(c); err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("HUB_UNREACHABLE")) {
		t.Errorf("body missing HUB_UNREACHABLE: %s", rec.Body.String())
	}
}

// TestHubUnreachable_Mutating covers the 502 on the mutating forward
// path. Audit must NOT fire.
func TestHubUnreachable_Mutating(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	stub.Close()

	spy := &auditSpy{}
	h := New(Deps{
		Hub:    hub.New(stub.URL, "svc-token", nil, slog.Default()),
		Logger: slog.Default(),
		Audit:  audit.NewWriter(spy, "audit", slog.Default()),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/alert-1/ack", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("alert-1")

	if err := h.AckAlert(c); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", rec.Code)
	}
	if spy.count() != 0 {
		t.Fatalf("audit count = %d; want 0", spy.count())
	}
}

// TestMutatingDefaultsContentTypeWhenHubOmits exercises the branch where
// Hub responds without a Content-Type header — the proxy must default to
// application/json on the client response. The stub hijacks the
// connection and writes a raw HTTP response so Go's net/http does NOT
// auto-sniff Content-Type from the body.
func TestMutatingDefaultsContentTypeWhenHubOmits(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter is not a Hijacker")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close() //nolint:errcheck
		// Raw HTTP/1.1 response with no Content-Type header at all.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		_ = buf.Flush()
	}))
	defer stub.Close()

	spy := &auditSpy{}
	h := New(Deps{
		Hub:    hub.New(stub.URL, "svc-token", nil, slog.Default()),
		Logger: slog.Default(),
		Audit:  audit.NewWriter(spy, "audit", slog.Default()),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/alert-1/ack", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("alert-1")

	if err := h.AckAlert(c); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", got)
	}
}

// TestMutatingForwardQueryString verifies the mutating forward path
// appends any incoming query string to the Hub URL.
func TestMutatingForwardQueryString(t *testing.T) {
	var capturedQuery string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer stub.Close()

	h := newAlertsHandler(t, stub.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/alerts/alert-1/ack?dryRun=1", nil)
	rec := httptest.NewRecorder()
	c := echoContext(req, rec, "alice", "user-1")
	c.SetParamNames("id")
	c.SetParamValues("alert-1")

	if err := h.AckAlert(c); err != nil {
		t.Fatalf("AckAlert: %v", err)
	}
	if capturedQuery != "dryRun=1" {
		t.Errorf("hub query = %q; want dryRun=1", capturedQuery)
	}
}

// TestErrJSON_Shape covers the local errJSON helper (also exercised
// indirectly via the HUB_NOT_CONFIGURED / HUB_UNREACHABLE branches above,
// but the helper merits a direct shape assertion).
func TestErrJSON_Shape(t *testing.T) {
	got := errJSON("Hub is broken", "server_error", "BOOM")
	inner, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %+v", got)
	}
	if inner["message"] != "Hub is broken" {
		t.Errorf("message = %v; want Hub is broken", inner["message"])
	}
	if inner["type"] != "server_error" {
		t.Errorf("type = %v; want server_error", inner["type"])
	}
	if inner["code"] != "BOOM" {
		t.Errorf("code = %v; want BOOM", inner["code"])
	}
}
