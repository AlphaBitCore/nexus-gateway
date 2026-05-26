package oauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// fakeClients is an in-memory clientLoader fake so authorize tests can run
// without Postgres. Keys are client ids; absent keys surface as
// ErrClientNotFound to exercise the invalid_client error path.
type fakeClients map[string]*store.OAuthClient

func (f fakeClients) GetByID(_ context.Context, id string) (*store.OAuthClient, error) {
	c, ok := f[id]
	if !ok {
		return nil, store.ErrClientNotFound
	}
	return c, nil
}

// fixture bundles the stores + handler wiring that every authorize test
// needs. Each test gets its own instance so store state does not leak.
type fixture struct {
	t        *testing.T
	bindings *store.BindingStore
	pending  *store.PendingAuthzStore
	echo     *echo.Echo
}

func newFixture(t *testing.T, clients fakeClients) *fixture {
	t.Helper()
	b := store.NewBindingStore()
	p := store.NewPendingAuthzStore()
	t.Cleanup(b.Close)
	t.Cleanup(p.Close)

	e := echo.New()
	e.GET("/oauth/authorize", oauth.AuthorizeHandler(oauth.AuthorizeDeps{
		Clients:  clients,
		Bindings: b,
		Pending:  p,
	}))
	return &fixture{t: t, bindings: b, pending: p, echo: e}
}

// do runs the authorize handler against the given query and returns the
// recorded response. state is always included via the helper because it is
// required by every valid request; callers override via params.
func (f *fixture) do(params url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil)
	rec := httptest.NewRecorder()
	f.echo.ServeHTTP(rec, req)
	return rec
}

// decodeError unmarshals the RFC 6749 error JSON body. Tests use this to
// assert both status and code in a single check.
func decodeError(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var got struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode error body: %v (body=%q)", err, string(body))
	}
	return got.Error, got.ErrorDescription
}

const (
	publicClientID = "web-console"
	agentClientID  = "agent-desktop"
	validRedirect  = "http://127.0.0.1:54321/callback"
	s256Challenge  = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" // sha256("the-verifier") base64url
)

func webConsoleClient() *store.OAuthClient {
	return &store.OAuthClient{
		ID:           publicClientID,
		Name:         "Web Console",
		Type:         "public",
		RedirectURIs: []string{"https://cp.nexus.ai/callback"},
		RequirePKCE:  true,
	}
}

func agentDesktopClient() *store.OAuthClient {
	return &store.OAuthClient{
		ID:           agentClientID,
		Name:         "Agent Desktop",
		Type:         "public",
		RedirectURIs: []string{"http://127.0.0.1:*/callback"},
		RequirePKCE:  true,
	}
}

func validAgentParams() url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {agentClientID},
		"redirect_uri":          {validRedirect},
		"scope":                 {"traffic:write"},
		"state":                 {"xyz-state"},
		"nonce":                 {"n-123"},
		"code_challenge":        {s256Challenge},
		"code_challenge_method": {"S256"},
		"binding_id":            {"bind-abc"},
	}
}

func TestAuthorize_ValidNonAgentRedirectsToLogin(t *testing.T) {
	clients := fakeClients{publicClientID: webConsoleClient()}
	f := newFixture(t, clients)

	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {publicClientID},
		"redirect_uri":          {"https://cp.nexus.ai/callback"},
		"scope":                 {"openid profile"},
		"state":                 {"s1"},
		"nonce":                 {"n1"},
		"code_challenge":        {s256Challenge},
		"code_challenge_method": {"S256"},
	}
	rec := f.do(params)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Path != "/login" {
		t.Fatalf("want path /login, got %q", loc.Path)
	}
	authctx := loc.Query().Get("authctx")
	if authctx == "" {
		t.Fatalf("authctx missing from Location: %q", rec.Header().Get("Location"))
	}

	entry, ok := f.pending.Take(authctx)
	if !ok {
		t.Fatalf("pending entry not found for authctx=%q", authctx)
	}
	if entry.ClientID != publicClientID || entry.State != "s1" || entry.Nonce != "n1" {
		t.Fatalf("unexpected pending entry: %+v", entry)
	}
	if entry.Scope != "openid profile" {
		t.Fatalf("scope round-trip mismatch: %q", entry.Scope)
	}
	if entry.CodeChallenge != s256Challenge {
		t.Fatalf("code_challenge round-trip mismatch: %q", entry.CodeChallenge)
	}
	if entry.DeviceID != "" {
		t.Fatalf("non-agent flow should not carry DeviceID, got %q", entry.DeviceID)
	}
	if time.Until(entry.ExpiresAt) <= 0 {
		t.Fatalf("expires_at should be in the future: %v", entry.ExpiresAt)
	}
}

func TestAuthorize_AgentDesktopConsumesBinding(t *testing.T) {
	clients := fakeClients{agentClientID: agentDesktopClient()}
	f := newFixture(t, clients)

	const bindingID = "bind-abc"
	const deviceID = "dev-42"
	f.bindings.Put(bindingID, store.BindingEntry{
		DeviceID:      deviceID,
		State:         "xyz-state",
		CodeChallenge: s256Challenge,
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})

	rec := f.do(validAgentParams())
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	authctx := loc.Query().Get("authctx")
	if authctx == "" {
		t.Fatalf("authctx missing")
	}

	entry, ok := f.pending.Take(authctx)
	if !ok {
		t.Fatalf("pending entry not stored")
	}
	if entry.DeviceID != deviceID {
		t.Fatalf("DeviceID mismatch: want %q, got %q", deviceID, entry.DeviceID)
	}

	// Binding must be single-use: consumed on successful authorize.
	if _, ok := f.bindings.Get(bindingID); ok {
		t.Fatalf("binding should have been deleted after authorize")
	}
}

func TestAuthorize_ValidationErrors(t *testing.T) {
	cases := []struct {
		name         string
		clients      fakeClients
		params       func() url.Values
		wantStatus   int
		wantCode     string
		wantDescSubs string // substring check on error_description
	}{
		{
			name:    "response_type_not_code",
			clients: fakeClients{publicClientID: webConsoleClient()},
			params: func() url.Values {
				p := url.Values{
					"response_type":         {"token"},
					"client_id":             {publicClientID},
					"redirect_uri":          {"https://cp.nexus.ai/callback"},
					"state":                 {"s"},
					"code_challenge":        {s256Challenge},
					"code_challenge_method": {"S256"},
				}
				return p
			},
			wantStatus:   http.StatusBadRequest,
			wantCode:     "invalid_request",
			wantDescSubs: "response_type",
		},
		{
			name:    "unknown_client_id",
			clients: fakeClients{},
			params: func() url.Values {
				return url.Values{
					"response_type": {"code"},
					"client_id":     {"ghost"},
					"redirect_uri":  {"https://cp.nexus.ai/callback"},
					"state":         {"s"},
				}
			},
			wantStatus:   http.StatusBadRequest,
			wantCode:     "invalid_client",
			wantDescSubs: "client_id",
		},
		{
			name:    "redirect_uri_not_allowed",
			clients: fakeClients{publicClientID: webConsoleClient()},
			params: func() url.Values {
				return url.Values{
					"response_type":         {"code"},
					"client_id":             {publicClientID},
					"redirect_uri":          {"https://attacker.example/cb"},
					"state":                 {"s"},
					"code_challenge":        {s256Challenge},
					"code_challenge_method": {"S256"},
				}
			},
			wantStatus:   http.StatusBadRequest,
			wantCode:     "invalid_request",
			wantDescSubs: "redirect_uri",
		},
		{
			name:    "missing_code_challenge_when_pkce_required",
			clients: fakeClients{publicClientID: webConsoleClient()},
			params: func() url.Values {
				return url.Values{
					"response_type": {"code"},
					"client_id":     {publicClientID},
					"redirect_uri":  {"https://cp.nexus.ai/callback"},
					"state":         {"s"},
				}
			},
			wantStatus:   http.StatusBadRequest,
			wantCode:     "invalid_request",
			wantDescSubs: "code_challenge required",
		},
		{
			name:    "code_challenge_method_plain_rejected",
			clients: fakeClients{publicClientID: webConsoleClient()},
			params: func() url.Values {
				return url.Values{
					"response_type":         {"code"},
					"client_id":             {publicClientID},
					"redirect_uri":          {"https://cp.nexus.ai/callback"},
					"state":                 {"s"},
					"code_challenge":        {s256Challenge},
					"code_challenge_method": {"plain"},
				}
			},
			wantStatus:   http.StatusBadRequest,
			wantCode:     "invalid_request",
			wantDescSubs: "S256",
		},
		{
			name:    "missing_state",
			clients: fakeClients{publicClientID: webConsoleClient()},
			params: func() url.Values {
				return url.Values{
					"response_type":         {"code"},
					"client_id":             {publicClientID},
					"redirect_uri":          {"https://cp.nexus.ai/callback"},
					"code_challenge":        {s256Challenge},
					"code_challenge_method": {"S256"},
				}
			},
			wantStatus:   http.StatusBadRequest,
			wantCode:     "invalid_request",
			wantDescSubs: "state",
		},
		// NOTE: the legacy "agent_desktop_missing_binding_id rejects" case
		// was REMOVED — binding_id is now optional on /oauth/authorize.
		// The first-enrollment flow has no device cert and therefore cannot
		// pre-flight a binding_id; PKCE S256 + SSO + loopback redirect URI
		// is the bootstrap trust for that path. See
		// TestAuthorize_AgentDesktopFirstEnrollment_NoBinding below for
		// the positive assertion of the new behaviour.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.clients)
			rec := f.do(tc.params())
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d, body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			code, desc := decodeError(t, rec.Body.Bytes())
			if code != tc.wantCode {
				t.Fatalf("error=%q, want %q", code, tc.wantCode)
			}
			if tc.wantDescSubs != "" && !strings.Contains(desc, tc.wantDescSubs) {
				t.Fatalf("description=%q does not contain %q", desc, tc.wantDescSubs)
			}
		})
	}
}

// TestAuthorize_AgentDesktopFirstEnrollment_NoBinding pins the
// first-enrollment path: a pre-cert agent (no mTLS, no binding_id)
// must succeed through /oauth/authorize so the SSO flow can finish
// and the agent can collect an enrollment JWT at /api/agent/sso-enroll.
// The deviceID on the resulting PendingAuthzEntry is empty (no
// device known yet); the enrollment JWT path fills it in later via
// the Hub-side CSR exchange.
func TestAuthorize_AgentDesktopFirstEnrollment_NoBinding(t *testing.T) {
	clients := fakeClients{agentClientID: agentDesktopClient()}
	f := newFixture(t, clients)

	params := validAgentParams()
	params.Del("binding_id")

	rec := f.do(params)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s (want 302 — first-enrollment with no binding_id must succeed)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	authctx := loc.Query().Get("authctx")
	if authctx == "" {
		t.Fatalf("authctx missing in redirect Location")
	}
	entry, ok := f.pending.Take(authctx)
	if !ok {
		t.Fatalf("pending entry not stored")
	}
	if entry.DeviceID != "" {
		t.Fatalf("DeviceID=%q on first-enrollment entry — want empty (device cert is issued later by /things/enroll)", entry.DeviceID)
	}
}

func TestAuthorize_AgentDesktop_BindingUnknown(t *testing.T) {
	clients := fakeClients{agentClientID: agentDesktopClient()}
	f := newFixture(t, clients)

	// No binding stored — authorize must reject.
	rec := f.do(validAgentParams())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	code, desc := decodeError(t, rec.Body.Bytes())
	if code != "invalid_request" {
		t.Fatalf("error=%q", code)
	}
	if !strings.Contains(desc, "binding_id unknown") {
		t.Fatalf("description=%q", desc)
	}
}

func TestAuthorize_AgentDesktop_BindingChallengeMismatch(t *testing.T) {
	clients := fakeClients{agentClientID: agentDesktopClient()}
	f := newFixture(t, clients)

	const bindingID = "bind-abc"
	f.bindings.Put(bindingID, store.BindingEntry{
		DeviceID:      "dev-1",
		State:         "xyz-state",
		CodeChallenge: "different-challenge",
		CreatedAt:     time.Now(),
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})

	rec := f.do(validAgentParams())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != "invalid_request" {
		t.Fatalf("error=%q", code)
	}
	// On mismatch the binding must NOT be consumed so the client can retry.
	if _, ok := f.bindings.Get(bindingID); !ok {
		t.Fatalf("binding should survive a mismatch; got deleted")
	}
}
