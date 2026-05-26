package oauth_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/oauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	cpstore "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// agentDeviceCtxKey mirrors the unexported key used by
// internal/middleware/agentmtls.go. The middleware sets this on the Echo
// context after mTLS passes; tests simulate the middleware having run by
// setting it directly, avoiding the need to spin up a TLS listener.
const agentDeviceCtxKey = "agentDevice"

// setDevice installs device on the Echo context using the same key the mTLS
// middleware writes to. Passing nil leaves the key unset to simulate the
// middleware having rejected the request.
func setDevice(c echo.Context, device *cpstore.ThingNodeInfo) {
	if device != nil {
		c.Set(agentDeviceCtxKey, device)
	}
}

// dbFixture bundles the collaborators device-binding tests need. Each test
// builds its own fixture so BindingStore state is isolated.
type dbFixture struct {
	t        *testing.T
	bindings *store.BindingStore
	handler  echo.HandlerFunc
	echo     *echo.Echo
}

func newDBFixture(t *testing.T) *dbFixture {
	t.Helper()
	b := store.NewBindingStore()
	t.Cleanup(b.Close)

	h := oauth.DeviceBindingHandler(oauth.DeviceBindingDeps{Bindings: b})
	e := echo.New()
	return &dbFixture{t: t, bindings: b, handler: h, echo: e}
}

// call invokes the handler with the given JSON body. device, when non-nil, is
// stashed in the Echo context to simulate the mTLS middleware having run.
// When body is nil the request is sent with Content-Type application/json and
// an empty body to exercise the bind-error path.
func (f *dbFixture) call(device *cpstore.ThingNodeInfo, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/device-binding", reader)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := f.echo.NewContext(req, rec)
	setDevice(c, device)
	if err := f.handler(c); err != nil {
		f.echo.HTTPErrorHandler(err, c)
	}
	return rec
}

// mustJSON marshals v or fails the test. Panic vs fatal keeps callers terse.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// activeDevice is the stock ThingNodeInfo used whenever a test needs a
// well-formed, active device in the context.
func activeDevice() *cpstore.ThingNodeInfo {
	return &cpstore.ThingNodeInfo{
		ID:         "dev-123",
		Hostname:   "alice-macbook",
		Status:     "active",
		CertSerial: "AABBCCDD",
	}
}

func TestDeviceBinding_Success(t *testing.T) {
	f := newDBFixture(t)
	before := time.Now()
	rec := f.call(activeDevice(), mustJSON(t, map[string]string{
		"binding_id":     "bind-abc",
		"state":          "state-xyz",
		"code_challenge": "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
	}))
	after := time.Now()

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	entry, ok := f.bindings.Get("bind-abc")
	if !ok {
		t.Fatal("binding not stored under binding_id key")
	}
	if entry.DeviceID != "dev-123" {
		t.Errorf("DeviceID = %q, want dev-123", entry.DeviceID)
	}
	if entry.State != "state-xyz" {
		t.Errorf("State = %q, want state-xyz", entry.State)
	}
	if entry.CodeChallenge != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("CodeChallenge = %q, want fixture", entry.CodeChallenge)
	}
	// ExpiresAt must be ~5 min after the handler ran. Use a generous window
	// bounded by the call-site clock samples so we catch drift without
	// tripping on scheduling jitter.
	wantLow := before.Add(5 * time.Minute)
	wantHigh := after.Add(5*time.Minute + time.Second)
	if entry.ExpiresAt.Before(wantLow) || entry.ExpiresAt.After(wantHigh) {
		t.Errorf("ExpiresAt = %v, want in [%v, %v]", entry.ExpiresAt, wantLow, wantHigh)
	}
}

func TestDeviceBinding_MissingDeviceContext(t *testing.T) {
	f := newDBFixture(t)
	rec := f.call(nil, mustJSON(t, map[string]string{
		"binding_id":     "bind-abc",
		"state":          "state-xyz",
		"code_challenge": "cc",
	}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidClient {
		t.Errorf("error = %q, want %q", code, oauth.ErrInvalidClient)
	}
}

func TestDeviceBinding_DeviceRevoked(t *testing.T) {
	f := newDBFixture(t)
	// Simulate a downstream caller that reached the handler with a non-active
	// device. Real middleware rejects "revoked" upstream, but the handler's
	// defensive check must still reject anything != "active".
	d := activeDevice()
	d.Status = "revoked"
	rec := f.call(d, mustJSON(t, map[string]string{
		"binding_id":     "bind-abc",
		"state":          "state-xyz",
		"code_challenge": "cc",
	}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidClient {
		t.Errorf("error = %q, want %q", code, oauth.ErrInvalidClient)
	}
}

func TestDeviceBinding_MissingBindingID(t *testing.T) {
	f := newDBFixture(t)
	rec := f.call(activeDevice(), mustJSON(t, map[string]string{
		"state":          "state-xyz",
		"code_challenge": "cc",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidRequest {
		t.Errorf("error = %q, want %q", code, oauth.ErrInvalidRequest)
	}
}

func TestDeviceBinding_MissingState(t *testing.T) {
	f := newDBFixture(t)
	rec := f.call(activeDevice(), mustJSON(t, map[string]string{
		"binding_id":     "bind-abc",
		"code_challenge": "cc",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidRequest {
		t.Errorf("error = %q, want %q", code, oauth.ErrInvalidRequest)
	}
}

func TestDeviceBinding_MissingCodeChallenge(t *testing.T) {
	f := newDBFixture(t)
	rec := f.call(activeDevice(), mustJSON(t, map[string]string{
		"binding_id": "bind-abc",
		"state":      "state-xyz",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidRequest {
		t.Errorf("error = %q, want %q", code, oauth.ErrInvalidRequest)
	}
}

func TestDeviceBinding_MalformedJSON(t *testing.T) {
	f := newDBFixture(t)
	rec := f.call(activeDevice(), []byte("{not-json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != oauth.ErrInvalidRequest {
		t.Errorf("error = %q, want %q", code, oauth.ErrInvalidRequest)
	}
}

func TestDeviceBinding_TTL(t *testing.T) {
	f := newDBFixture(t)
	before := time.Now()
	rec := f.call(activeDevice(), mustJSON(t, map[string]string{
		"binding_id":     "bind-ttl",
		"state":          "state-ttl",
		"code_challenge": "cc-ttl",
	}))
	after := time.Now()

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	entry, ok := f.bindings.Get("bind-ttl")
	if !ok {
		t.Fatal("binding missing from store")
	}
	// CreatedAt must be bracketed by the pre-call / post-call samples; this
	// catches regressions that forget to stamp CreatedAt.
	if entry.CreatedAt.Before(before) || entry.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want in [%v, %v]", entry.CreatedAt, before, after)
	}
	// ExpiresAt - CreatedAt should equal the documented TTL exactly because
	// both are derived from the same wall-clock sample inside the handler.
	if got := entry.ExpiresAt.Sub(entry.CreatedAt); got != 5*time.Minute {
		t.Errorf("TTL = %v, want 5m", got)
	}
}

// sanity-check: decodeError helper is shared across oauth tests. Assert that
// a simple RFC 6749 error round-trips so a future refactor of the envelope is
// caught by *all* handlers at once.
func TestDeviceBinding_ErrorEnvelopeRoundTrip(t *testing.T) {
	f := newDBFixture(t)
	rec := f.call(nil, mustJSON(t, map[string]string{})) // no device, empty body
	if !strings.Contains(rec.Body.String(), `"error":"invalid_client"`) {
		t.Errorf("body = %s, want invalid_client envelope", rec.Body.String())
	}
}
