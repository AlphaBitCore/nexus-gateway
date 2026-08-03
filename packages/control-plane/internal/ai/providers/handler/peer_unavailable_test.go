package providers

// Peer-resolution failure UX: every CP→ai-gateway forward in this package
// resolves the gateway base URL from the Hub at request time. When that
// resolution fails (gateway not registered yet / Hub briefly down) the
// endpoint must answer 503 with code PEER_SERVICE_UNAVAILABLE — never a raw
// 500 and never the transport-level soft errors used after resolution.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/peer"
)

func failingBase(context.Context) (string, error) {
	return "", errors.New("peer service URL not reported yet")
}

func assertPeer503(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), peer.CodeUnavailable) {
		t.Errorf("body missing %s: %s", peer.CodeUnavailable, rec.Body.String())
	}
}

func TestProbeCredential_ResolverError503(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))
	h := newHandler(db, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayBase: failingBase})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")
	_ = h.ProbeCredential(c)
	assertPeer503(t, rec)
}

func TestProviderTestConnection_ResolverError503(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayBase: failingBase})
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"name":"p","adapterType":"openai","baseUrl":"https://api.example"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ProviderTestConnection(c)
	assertPeer503(t, rec)
}

func TestProviderDiscoverModels_ResolverError503(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayBase: failingBase})
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"adapterType":"openai","baseUrl":"https://api.example"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ProviderDiscoverModels(c)
	assertPeer503(t, rec)
}

func TestForwardEmbeddingProbe_ResolverError503(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{AIGatewayBase: failingBase})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.forwardEmbeddingProbe(c, "p1", "m1", "name", "pm1", "https://api.example", "key", 0)
	assertPeer503(t, rec)
}

// TestAIGatewayBase_NilProvider503 locks the hand-wired-Handler case: a nil
// provider behaves like an unresolved peer (transient 503), not a panic.
func TestAIGatewayBase_NilProvider503(t *testing.T) {
	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{})
	if _, err := h.aiGatewayBase(context.Background()); !errors.Is(err, peer.ErrUnavailable) {
		t.Fatalf("nil provider must yield peer.ErrUnavailable, got %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"adapterType":"openai","baseUrl":"https://api.example"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	_ = h.ProviderDiscoverModels(c)
	assertPeer503(t, rec)
}
