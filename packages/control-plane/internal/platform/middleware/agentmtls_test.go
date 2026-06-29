package middleware_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/goccy/go-json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// fakeNodeLookup is an in-memory ThingNodeLookup keyed by the
// uppercased, colon-stripped 32-hex serial form the middleware
// computes from cert.SerialNumber. Tests pre-populate this map (or
// set err) to drive the four branches of AgentMTLSAuth.
type fakeNodeLookup struct {
	nodes map[string]*store.ThingNodeInfo
	err   error
}

func (f *fakeNodeLookup) LookupThingNodeByCertSerial(_ context.Context, serial string) (*store.ThingNodeInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.nodes[serial], nil
}

// makeTLSRequest builds a GET /ping with an attached tls.ConnectionState
// carrying a single x509 certificate with the given serial. The serial
// is supplied as a big.Int because that's the production data shape
// from x/crypto.
func makeTLSRequest(serial *big.Int) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	if serial != nil {
		req.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{
				{SerialNumber: serial},
			},
		}
	}
	return req
}

// errorEnvelope decodes the {error:{message,type,code}} envelope.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeErr(t *testing.T, body []byte) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body decode: %v: %q", err, body)
	}
	return env
}

// TestAgentMTLSAuth_NoCertReturns401 covers req.TLS == nil — the most
// common production failure (client forgot mTLS cert or proxy stripped
// it). Must reject with AGENT_CERT_REQUIRED.
func TestAgentMTLSAuth_NoCertReturns401(t *testing.T) {
	t.Parallel()
	lookup := &fakeNodeLookup{}
	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.AgentMTLSAuth(lookup))
	g.GET("/ping", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := makeTLSRequest(nil) // no TLS at all
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	env := decodeErr(t, rec.Body.Bytes())
	if env.Error.Code != "AGENT_CERT_REQUIRED" {
		t.Errorf("error.code=%q want AGENT_CERT_REQUIRED", env.Error.Code)
	}
}

// TestAgentMTLSAuth_EmptyPeerCertsReturns401 covers the boundary case
// where req.TLS is set but PeerCertificates is empty — happens if the
// server is configured with ClientAuth=RequestClientCert but the
// client doesn't supply one. Same envelope as the nil-TLS case.
func TestAgentMTLSAuth_EmptyPeerCertsReturns401(t *testing.T) {
	t.Parallel()
	lookup := &fakeNodeLookup{}
	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.AgentMTLSAuth(lookup))
	g.GET("/ping", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.TLS = &tls.ConnectionState{} // present but empty
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	env := decodeErr(t, rec.Body.Bytes())
	if env.Error.Code != "AGENT_CERT_REQUIRED" {
		t.Errorf("error.code=%q want AGENT_CERT_REQUIRED", env.Error.Code)
	}
}

// TestAgentMTLSAuth_LookupErrorReturns500 covers DB outage / planner
// error during cert→node resolution. Must surface as 500
// AUTH_SERVICE_ERROR so the client can distinguish a transient
// infra issue from a genuine cred problem.
func TestAgentMTLSAuth_LookupErrorReturns500(t *testing.T) {
	t.Parallel()
	lookup := &fakeNodeLookup{err: errors.New("DB outage")}
	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.AgentMTLSAuth(lookup))
	g.GET("/ping", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := makeTLSRequest(big.NewInt(0x1234))
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	env := decodeErr(t, rec.Body.Bytes())
	if env.Error.Code != "AUTH_SERVICE_ERROR" {
		t.Errorf("error.code=%q want AUTH_SERVICE_ERROR", env.Error.Code)
	}
}

// TestAgentMTLSAuth_UnknownCertReturns401 covers cert present + lookup
// returns nil (no DB row): rejected as AGENT_CERT_UNKNOWN. Critical:
// this branch must NOT 500 — the agent UX depends on a 401 for
// "device not enrolled" to tell the user to re-enroll.
func TestAgentMTLSAuth_UnknownCertReturns401(t *testing.T) {
	t.Parallel()
	lookup := &fakeNodeLookup{nodes: map[string]*store.ThingNodeInfo{}}
	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.AgentMTLSAuth(lookup))
	g.GET("/ping", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := makeTLSRequest(big.NewInt(0xabcdef))
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	env := decodeErr(t, rec.Body.Bytes())
	if env.Error.Code != "AGENT_CERT_UNKNOWN" {
		t.Errorf("error.code=%q want AGENT_CERT_UNKNOWN", env.Error.Code)
	}
}

// TestAgentMTLSAuth_RevokedDeviceReturns403 covers the revoked-status
// branch — the device's cert is still valid X.509-wise but admin
// pulled the row. Must 403 AGENT_DEVICE_REVOKED, not 401: this is the
// "admin actively wants this device locked out" signal, distinct
// from "device unknown" (re-enroll fixes it).
func TestAgentMTLSAuth_RevokedDeviceReturns403(t *testing.T) {
	t.Parallel()
	// Production serial format: 32 hex uppercase chars (no colons).
	// big.NewInt(0x42).Bytes() is [0x42] → padded to "0000...0042".
	// The middleware formats with %032X so we check via its output.
	serial := big.NewInt(0x42)
	const expectedKey = "00000000000000000000000000000042"
	lookup := &fakeNodeLookup{nodes: map[string]*store.ThingNodeInfo{
		expectedKey: {ID: "thing-1", Hostname: "host-1", Status: "revoked", CertSerial: expectedKey},
	}}

	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.AgentMTLSAuth(lookup))
	g.GET("/ping", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := makeTLSRequest(serial)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", rec.Code)
	}
	env := decodeErr(t, rec.Body.Bytes())
	if env.Error.Code != "AGENT_DEVICE_REVOKED" {
		t.Errorf("error.code=%q want AGENT_DEVICE_REVOKED", env.Error.Code)
	}
}

// TestAgentMTLSAuth_HappyPath covers the success path: cert resolved,
// node status not "revoked", handler reached with the device on
// context. Asserts AgentDeviceFromContext returns the same row the
// lookup produced (load-bearing for downstream handlers that key
// off device.ID).
func TestAgentMTLSAuth_HappyPath(t *testing.T) {
	t.Parallel()
	serial := big.NewInt(0xdeadbeef)
	const expectedKey = "000000000000000000000000DEADBEEF"
	row := &store.ThingNodeInfo{ID: "thing-7", Hostname: "h7", Status: "online", CertSerial: expectedKey}
	lookup := &fakeNodeLookup{nodes: map[string]*store.ThingNodeInfo{expectedKey: row}}

	e := echo.New()
	e.HideBanner = true
	var seen *store.ThingNodeInfo
	g := e.Group("", middleware.AgentMTLSAuth(lookup))
	g.GET("/ping", func(c echo.Context) error {
		seen = middleware.AgentDeviceFromContext(c)
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := makeTLSRequest(serial)
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if seen == nil {
		t.Fatal("AgentDeviceFromContext returned nil despite happy path")
	}
	if seen.ID != "thing-7" || seen.Status != "online" {
		t.Errorf("device=%+v want ID=thing-7 status=online", seen)
	}
}

// TestAgentDeviceFromContext_EmptyContext asserts the helper returns
// nil when the middleware hasn't run — handlers must not see a stale
// type-asserted value from another request's context state.
func TestAgentDeviceFromContext_EmptyContext(t *testing.T) {
	t.Parallel()
	e := echo.New()
	e.HideBanner = true
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if got := middleware.AgentDeviceFromContext(c); got != nil {
		t.Errorf("got=%+v, want nil", got)
	}
}

// integration with the real *store.DB seam
//
// The cases above use a hand-rolled ThingNodeLookup. That covers every
// AgentMTLSAuth branch but doesn't catch a regression where the
// AgentMTLSAuth middleware passes a malformed serial to the DB layer.
// The block below wires AgentMTLSAuth against a real *store.DB built
// from a pgxmock pool — same seam cp/store uses for unit tests — so
// the SQL-shape contract between the middleware (which formats and
// normalises the cert serial) and store.LookupThingNodeByCertSerial
// is locked in.

// TestAgentMTLSAuth_RealStoreSeam_PassesNormalizedSerial asserts the
// middleware feeds the DB query a serial that's uppercased,
// colon-stripped, and zero-padded to 32 hex chars — the format
// LookupThingNodeByCertSerial's WHERE clause expects.
func TestAgentMTLSAuth_RealStoreSeam_PassesNormalizedSerial(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)

	db := store.NewWithPgxPool(mock)
	// Serial 0xabcd → "0000...0000ABCD" (32-hex, uppercase).
	mock.ExpectQuery(`FROM thing t\s+JOIN thing_agent ta`).
		WithArgs("0000000000000000000000000000ABCD").
		WillReturnRows(pgxmock.NewRows([]string{"id", "hostname", "status", "cert_serial"}).
			AddRow("thing-x", "host-x", "online", "0000000000000000000000000000ABCD"))

	e := echo.New()
	e.HideBanner = true
	var seen *store.ThingNodeInfo
	g := e.Group("", middleware.AgentMTLSAuth(db))
	g.GET("/ping", func(c echo.Context) error {
		seen = middleware.AgentDeviceFromContext(c)
		return c.NoContent(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := makeTLSRequest(big.NewInt(0xabcd))
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if seen == nil || seen.ID != "thing-x" {
		t.Errorf("device=%+v want ID=thing-x", seen)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("pgxmock expectations: %v", err)
	}
}

// TestAgentMTLSAuth_RealStoreSeam_ErrorBubbles asserts a DB query
// error round-trips through the middleware as 500 AUTH_SERVICE_ERROR.
// This guards against a regression where a future store wrapper
// swallows the wrap and the middleware silently allows.
func TestAgentMTLSAuth_RealStoreSeam_ErrorBubbles(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)

	db := store.NewWithPgxPool(mock)
	mock.ExpectQuery(`FROM thing t\s+JOIN thing_agent ta`).
		WithArgs("00000000000000000000000000000001").
		WillReturnError(errors.New("connection reset"))

	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.AgentMTLSAuth(db))
	g.GET("/ping", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := makeTLSRequest(big.NewInt(1))
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	env := decodeErr(t, rec.Body.Bytes())
	if env.Error.Code != "AUTH_SERVICE_ERROR" {
		t.Errorf("error.code=%q want AUTH_SERVICE_ERROR", env.Error.Code)
	}
}

// TestAgentMTLSAuth_RealStoreSeam_NotFound asserts pgx.ErrNoRows from
// LookupThingNodeByCertSerial collapses to the 401 AGENT_CERT_UNKNOWN
// path — the store wraps the "no rows" sentinel into (nil, nil) and
// the middleware turns that into a 401.
func TestAgentMTLSAuth_RealStoreSeam_NotFound(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)

	db := store.NewWithPgxPool(mock)
	mock.ExpectQuery(`FROM thing t\s+JOIN thing_agent ta`).
		WithArgs("00000000000000000000000000000002").
		WillReturnError(pgx.ErrNoRows)

	e := echo.New()
	e.HideBanner = true
	g := e.Group("", middleware.AgentMTLSAuth(db))
	g.GET("/ping", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := makeTLSRequest(big.NewInt(2))
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	env := decodeErr(t, rec.Body.Bytes())
	if env.Error.Code != "AGENT_CERT_UNKNOWN" {
		t.Errorf("error.code=%q want AGENT_CERT_UNKNOWN", env.Error.Code)
	}
}
