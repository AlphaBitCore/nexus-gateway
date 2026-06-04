package oauth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// Tests live in package oauth (not oauth_test) so they can call unexported
// helpers extractClientSecret / verifyClientAuth directly. Both helpers
// belong to the package's tightly scoped client_secret contract and are
// exercised here in isolation rather than via the wider token handler.

func makeCtx(t *testing.T, basic, formSecret string) echo.Context {
	t.Helper()
	form := ""
	if formSecret != "" {
		form = "client_secret=" + formSecret
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(basic)))
	}
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec)
}

func TestExtractClientSecret_BasicHeaderTakesPrecedence(t *testing.T) {
	c := makeCtx(t, "id-x:secret-from-basic", "secret-from-form")
	if got := extractClientSecret(c); got != "secret-from-basic" {
		t.Fatalf("got=%q, want secret-from-basic (Basic must win over form per RFC 6749 §2.3.1)", got)
	}
}

func TestExtractClientSecret_FormFallback(t *testing.T) {
	c := makeCtx(t, "", "secret-from-form")
	if got := extractClientSecret(c); got != "secret-from-form" {
		t.Fatalf("got=%q, want secret-from-form", got)
	}
}

func TestExtractClientSecret_MalformedBasicFallsThrough(t *testing.T) {
	// Authorization: Basic <not-valid-base64> — must fall through to form.
	req := httptest.NewRequest(http.MethodPost, "/oauth/token",
		strings.NewReader("client_secret=form-x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic !!!not-base64!!!")
	c := echo.New().NewContext(req, httptest.NewRecorder())
	if got := extractClientSecret(c); got != "form-x" {
		t.Fatalf("got=%q, want form-x", got)
	}
}

func TestExtractClientSecret_BasicWithoutColonFallsThrough(t *testing.T) {
	// Basic encoded as "just-an-id" with no colon — SplitN returns one
	// element so we must fall through to the form.
	c := makeCtx(t, "id-with-no-colon", "secret-from-form")
	if got := extractClientSecret(c); got != "secret-from-form" {
		t.Fatalf("got=%q, want secret-from-form", got)
	}
}

func TestExtractClientSecret_None(t *testing.T) {
	c := makeCtx(t, "", "")
	if got := extractClientSecret(c); got != "" {
		t.Fatalf("got=%q, want empty", got)
	}
}

// fakeClientLoader is a one-row clientLoader for the verifyClientAuth tests
// — avoids depending on the package-level fakeClients map declared in the
// authorize_test.go file (different test file, same package).
type fakeClientLoader struct {
	id  string
	c   *store.OAuthClient
	err error
}

func (f *fakeClientLoader) GetByID(_ context.Context, id string) (*store.OAuthClient, error) {
	if f.err != nil {
		return nil, f.err
	}
	if id != f.id {
		return nil, store.ErrClientNotFound
	}
	return f.c, nil
}

func TestVerifyClientAuth_NilClientsFailsClosed(t *testing.T) {
	c := makeCtx(t, "", "")
	r := verifyClientAuth(context.Background(), c, nil, "anything")
	if r.ErrCode != ErrServerError || r.Status != http.StatusInternalServerError {
		t.Fatalf("got %+v, want server_error 500", r)
	}
}

func TestVerifyClientAuth_UnknownClientID(t *testing.T) {
	c := makeCtx(t, "", "")
	loader := &fakeClientLoader{id: "real", err: store.ErrClientNotFound}
	r := verifyClientAuth(context.Background(), c, loader, "ghost")
	if r.ErrCode != ErrInvalidClient || r.Status != http.StatusUnauthorized {
		t.Fatalf("got %+v, want invalid_client 401", r)
	}
}

func TestVerifyClientAuth_PublicWithoutSecret_OK(t *testing.T) {
	c := makeCtx(t, "", "")
	loader := &fakeClientLoader{id: "pub", c: &store.OAuthClient{ID: "pub", Type: "public"}}
	r := verifyClientAuth(context.Background(), c, loader, "pub")
	if r.ErrCode != "" || r.Client == nil {
		t.Fatalf("got %+v, want success", r)
	}
}

func TestVerifyClientAuth_PublicWithSecretRejected(t *testing.T) {
	c := makeCtx(t, "", "anything")
	loader := &fakeClientLoader{id: "pub", c: &store.OAuthClient{ID: "pub", Type: "public"}}
	r := verifyClientAuth(context.Background(), c, loader, "pub")
	if r.ErrCode != ErrInvalidClient {
		t.Fatalf("got %+v, want invalid_client (public client must not present a secret)", r)
	}
}

func TestVerifyClientAuth_ConfidentialMissingSecret(t *testing.T) {
	c := makeCtx(t, "", "")
	hash := "anything"
	loader := &fakeClientLoader{id: "conf", c: &store.OAuthClient{
		ID: "conf", Type: "confidential", ClientSecretHash: &hash,
	}}
	r := verifyClientAuth(context.Background(), c, loader, "conf")
	if r.ErrCode != ErrInvalidClient || !strings.Contains(r.Desc, "required") {
		t.Fatalf("got %+v, want invalid_client/required", r)
	}
}

func TestVerifyClientAuth_ConfidentialEmptyHashRejected(t *testing.T) {
	c := makeCtx(t, "", "any-secret")
	empty := ""
	loader := &fakeClientLoader{id: "conf", c: &store.OAuthClient{
		ID: "conf", Type: "confidential", ClientSecretHash: &empty,
	}}
	r := verifyClientAuth(context.Background(), c, loader, "conf")
	if r.ErrCode != ErrInvalidClient || !strings.Contains(r.Desc, "missing stored secret") {
		t.Fatalf("got %+v, want invalid_client/missing", r)
	}
}

func TestVerifyClientAuth_ConfidentialWrongSecretRejected(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("real-secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	hashStr := string(hash)
	c := makeCtx(t, "", "WRONG")
	loader := &fakeClientLoader{id: "conf", c: &store.OAuthClient{
		ID: "conf", Type: "confidential", ClientSecretHash: &hashStr,
	}}
	r := verifyClientAuth(context.Background(), c, loader, "conf")
	if r.ErrCode != ErrInvalidClient || !strings.Contains(r.Desc, "mismatch") {
		t.Fatalf("got %+v, want invalid_client/mismatch", r)
	}
}

func TestVerifyClientAuth_ConfidentialCorrectSecretViaForm_OK(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("real-secret"), bcrypt.MinCost)
	hashStr := string(hash)
	c := makeCtx(t, "", "real-secret")
	loader := &fakeClientLoader{id: "conf", c: &store.OAuthClient{
		ID: "conf", Type: "confidential", ClientSecretHash: &hashStr,
	}}
	r := verifyClientAuth(context.Background(), c, loader, "conf")
	if r.ErrCode != "" || r.Client == nil {
		t.Fatalf("got %+v, want success", r)
	}
}

func TestVerifyClientAuth_ConfidentialCorrectSecretViaBasic_OK(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("real-secret"), bcrypt.MinCost)
	hashStr := string(hash)
	c := makeCtx(t, "conf:real-secret", "")
	loader := &fakeClientLoader{id: "conf", c: &store.OAuthClient{
		ID: "conf", Type: "confidential", ClientSecretHash: &hashStr,
	}}
	r := verifyClientAuth(context.Background(), c, loader, "conf")
	if r.ErrCode != "" || r.Client == nil {
		t.Fatalf("got %+v, want success", r)
	}
}

func TestVerifyClientAuth_UnsupportedTypeRejected(t *testing.T) {
	c := makeCtx(t, "", "")
	loader := &fakeClientLoader{id: "weird", c: &store.OAuthClient{
		ID: "weird", Type: "something-else",
	}}
	r := verifyClientAuth(context.Background(), c, loader, "weird")
	if r.ErrCode != ErrInvalidClient || !strings.Contains(r.Desc, "unsupported") {
		t.Fatalf("got %+v, want invalid_client/unsupported", r)
	}
}
