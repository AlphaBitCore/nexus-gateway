// Tests asserting the providers BFF forwarders attach the internal-service
// bearer token on every CP→ai-gateway /internal/* call (F-0001).
package providers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// cpInternalToken is the shared internal-service token threaded through
// ProxyConfig.AIGatewayInternalToken in these tests.
const cpInternalToken = "cp-internal-token"

// TestForwardProviderTest_AttachesBearer verifies POST /internal/provider-test
// carries Authorization: Bearer <token>.
func TestForwardProviderTest_AttachesBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL:           srv.URL,
		AIGatewayInternalToken: cpInternalToken,
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")

	if err := h.forwardProviderTest(c, "OpenAI", "openai", "https://api.openai.com", "sk-x"); err != nil {
		t.Fatalf("forwardProviderTest: %v", err)
	}
	if want := "Bearer " + cpInternalToken; gotAuth != want {
		t.Errorf("Authorization = %q; want %q", gotAuth, want)
	}
}

// TestForwardEmbeddingProbe_AttachesBearer verifies POST /internal/embedding-probe
// carries Authorization: Bearer <token>.
func TestForwardEmbeddingProbe_AttachesBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	h := newHandler(nil, nil, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL:           srv.URL,
		AIGatewayInternalToken: cpInternalToken,
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")

	if err := h.forwardEmbeddingProbe(c, "prov-1", "model-1", "text-embedding-3-small",
		"text-embedding-3-small", "https://api.openai.com", "sk-x", 1536); err != nil {
		t.Fatalf("forwardEmbeddingProbe: %v", err)
	}
	if want := "Bearer " + cpInternalToken; gotAuth != want {
		t.Errorf("Authorization = %q; want %q", gotAuth, want)
	}
}

// TestProbeCredential_AttachesBearer verifies POST /internal/v1/credentials/:id/probe
// carries Authorization: Bearer <token>.
func TestProbeCredential_AttachesBearer(t *testing.T) {
	mock, db := newMockStore(t)
	now := nowFixture()
	mock.ExpectQuery(`FROM "Credential" WHERE id`).WithArgs("cred-1").
		WillReturnRows(pgxmock.NewRows(credentialMetadataCols).AddRow(makeCredentialRow(now)...))

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"latencyMs":7}`))
	}))
	defer srv.Close()

	h := newHandler(db, &hubSpy{}, &auditSpy{}, nil, nil, nil, ProxyConfig{
		AIGatewayURL:           srv.URL,
		AIGatewayInternalToken: cpInternalToken,
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c, _ := echoCtx(req, rec, "u-1")
	c.SetParamNames("id")
	c.SetParamValues("cred-1")

	if err := h.ProbeCredential(c); err != nil {
		t.Fatalf("ProbeCredential: %v", err)
	}
	if want := "Bearer " + cpInternalToken; gotAuth != want {
		t.Errorf("Authorization = %q; want %q", gotAuth, want)
	}
}
