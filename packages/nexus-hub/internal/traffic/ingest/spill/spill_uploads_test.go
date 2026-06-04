package spill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillupload"
)

const testUUID = "11111111-1111-1111-1111-111111111111"

// metaStub is an in-memory MetadataStore so LoadOrInit bootstraps a real
// epoch-1 signing secret (real HMAC — Sign/Verify are exercised for real).
type metaStub struct{ kv map[string][]byte }

func (m *metaStub) GetSystemMetadata(_ context.Context, key string) ([]byte, error) {
	return m.kv[key], nil
}
func (m *metaStub) SetSystemMetadata(_ context.Context, key string, value any, _ string) error {
	b, _ := json.Marshal(value)
	m.kv[key] = b
	return nil
}

func newSecrets(t *testing.T) *spillupload.SecretStore {
	t.Helper()
	s, err := spillupload.LoadOrInit(context.Background(), &metaStub{kv: map[string][]byte{}})
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	return s
}

// mockSpill implements spillstore.SpillStore + spillstore.Presigner.
type mockSpill struct {
	putErr     error
	presignErr error
	deleted    bool
}

func (m *mockSpill) Put(_ context.Context, content io.Reader, size int64, _ spillstore.PutOptions) (sharedaudit.SpillRef, error) {
	_, _ = io.Copy(io.Discard, content) // drive the sha256Tee so the hash is computed
	if m.putErr != nil {
		return sharedaudit.SpillRef{}, m.putErr
	}
	return sharedaudit.SpillRef{Backend: "localfs", Key: "k", Size: size}, nil
}
func (m *mockSpill) Get(context.Context, sharedaudit.SpillRef) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockSpill) Delete(context.Context, sharedaudit.SpillRef) error { m.deleted = true; return nil }
func (m *mockSpill) Sweep(context.Context, time.Time) (int, error)      { return 0, nil }
func (m *mockSpill) Stat(context.Context) (spillstore.Stats, error)     { return spillstore.Stats{}, nil }
func (m *mockSpill) Backend() string                                    { return "localfs" }
func (m *mockSpill) PresignPut(_ context.Context, _ string, _ int64, _ string, _ time.Duration) (string, error) {
	if m.presignErr != nil {
		return "", m.presignErr
	}
	return "https://s3.example/presigned", nil
}
func (m *mockSpill) KeyFor(_ time.Time, eventID, direction string) string {
	return "spill/" + eventID + "/" + direction
}

// nonPresignSpill implements SpillStore but NOT Presigner: it shadows the
// embedded PresignPut with a no-arg method, so the spillstore.Presigner type
// assertion in MintSpillUpload fails and the handler returns 503.
type nonPresignSpill struct{ mockSpill }

func (nonPresignSpill) PresignPut() {} // wrong signature → does not satisfy Presigner

// badActiveSource fails Active() so MintSpillUpload's Sign call errors with a
// non-ErrTokenInvalid error → the internalError("sign upload token") path.
type badActiveSource struct{}

func (badActiveSource) Active() (string, []byte, error) {
	return "", nil, errors.New("secret store not primed")
}
func (badActiveSource) Lookup(string) ([]byte, error) { return nil, errors.New("unused") }

// badLookupSource fails Lookup() with a non-sentinel error so PutSpillBlob's
// Verify returns a wrapped error → the internalError("verify token") path.
type badLookupSource struct{}

func (badLookupSource) Active() (string, []byte, error) { return "", nil, errors.New("unused") }
func (badLookupSource) Lookup(string) ([]byte, error)   { return nil, errors.New("redis blip") }

type mockDedup struct {
	acquired bool
	err      error
}

func (d *mockDedup) SetNX(context.Context, string, time.Duration) (bool, error) {
	return d.acquired, d.err
}

func rawMint(t *testing.T, h *SpillUploadAPI, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/things/spill-uploads", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.MintSpillUpload(c); err != nil {
		t.Fatalf("MintSpillUpload error: %v", err)
	}
	return rec
}

func mint(t *testing.T, h *SpillUploadAPI, r SpillUploadMintRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(r)
	return rawMint(t, h, string(b))
}

func validMintReq() SpillUploadMintRequest {
	return SpillUploadMintRequest{
		EventID:   testUUID,
		Direction: spillupload.DirectionRequest,
		SizeBytes: 100,
		SHA256:    strings.Repeat("a", 64),
	}
}

func putBlob(t *testing.T, h *SpillUploadAPI, token string, body []byte, contentLength int64) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/api/internal/spill/blob/"+token, bytes.NewReader(body))
	req.ContentLength = contentLength
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token")
	c.SetParamValues(token)
	if err := h.PutSpillBlob(c); err != nil {
		t.Fatalf("PutSpillBlob error: %v", err)
	}
	return rec
}

// signToken mints a real HMAC token for the given claims.
func signToken(t *testing.T, secrets *spillupload.SecretStore, c spillupload.Claims) string {
	t.Helper()
	token, _, err := spillupload.Sign(secrets, c, spillupload.MaxTTL)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return token
}

// ---- MintSpillUpload ----

func TestMint_NotConfigured(t *testing.T) {
	h := &SpillUploadAPI{Spill: nil, Secrets: nil}
	if rec := mint(t, h, validMintReq()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil deps should be 503, got %d", rec.Code)
	}
}

func TestMint_BadBody(t *testing.T) {
	h := &SpillUploadAPI{Spill: &mockSpill{}, Secrets: newSecrets(t)}
	if rec := rawMint(t, h, `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON should be 400, got %d", rec.Code)
	}
}

func TestMint_Validation(t *testing.T) {
	h := &SpillUploadAPI{Spill: &mockSpill{}, Secrets: newSecrets(t), SpillBackend: "localfs"}
	cases := []struct {
		name string
		mut  func(*SpillUploadMintRequest)
	}{
		{"empty eventId", func(r *SpillUploadMintRequest) { r.EventID = "" }},
		{"non-uuid eventId (length)", func(r *SpillUploadMintRequest) { r.EventID = "not-a-uuid" }},
		// 36 chars but dash slots hold hex → looksLikeUUID rejects the bad dash.
		{"uuid non-dash at dash slot", func(r *SpillUploadMintRequest) { r.EventID = "11111111a1111a1111a1111a111111111111" }},
		// 36 chars, correct dashes, but a non-hex char elsewhere.
		{"uuid non-hex char", func(r *SpillUploadMintRequest) { r.EventID = "z1111111-1111-1111-1111-111111111111" }},
		{"bad direction", func(r *SpillUploadMintRequest) { r.Direction = "sideways" }},
		{"zero size", func(r *SpillUploadMintRequest) { r.SizeBytes = 0 }},
		{"short sha", func(r *SpillUploadMintRequest) { r.SHA256 = "abc" }},
		{"non-hex sha", func(r *SpillUploadMintRequest) { r.SHA256 = strings.Repeat("z", 64) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validMintReq()
			tc.mut(&r)
			if rec := mint(t, h, r); rec.Code != http.StatusBadRequest {
				t.Fatalf("%s should be 400, got %d", tc.name, rec.Code)
			}
		})
	}
}

func TestMint_TooLarge(t *testing.T) {
	h := &SpillUploadAPI{Spill: &mockSpill{}, Secrets: newSecrets(t), SpillBackend: "localfs", PerObjectCap: 10}
	r := validMintReq()
	r.SizeBytes = 1000
	if rec := mint(t, h, r); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over cap should be 413, got %d", rec.Code)
	}
}

func TestMint_NotAPresigner(t *testing.T) {
	h := &SpillUploadAPI{Spill: &nonPresignSpill{}, Secrets: newSecrets(t), SpillBackend: "localfs"}
	if rec := mint(t, h, validMintReq()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-presigner backend should be 503, got %d", rec.Code)
	}
}

func TestMint_S3Success(t *testing.T) {
	h := &SpillUploadAPI{Spill: &mockSpill{}, Secrets: newSecrets(t), SpillBackend: "s3"}
	rec := mint(t, h, validMintReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("s3 mint should be 200, got %d", rec.Code)
	}
	var resp SpillUploadMintResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Backend != "s3" || resp.UploadURL != "https://s3.example/presigned" {
		t.Fatalf("s3 response wrong: %+v", resp)
	}
}

func TestMint_S3PresignError(t *testing.T) {
	// Logger set so the error-logging branch (h.logf with a non-nil logger) runs.
	h := &SpillUploadAPI{
		Spill:        &mockSpill{presignErr: errors.New("aws down")},
		Secrets:      newSecrets(t),
		SpillBackend: "s3",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if rec := mint(t, h, validMintReq()); rec.Code != http.StatusInternalServerError {
		t.Fatalf("presign failure should be 500, got %d", rec.Code)
	}
}

func TestMint_LocalfsSuccess(t *testing.T) {
	// HubURL set → URL uses it.
	h := &SpillUploadAPI{Spill: &mockSpill{}, Secrets: newSecrets(t), SpillBackend: "localfs", HubURL: "https://hub.example/"}
	rec := mint(t, h, validMintReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("localfs mint should be 200, got %d", rec.Code)
	}
	var resp SpillUploadMintResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Backend != "localfs" || !strings.HasPrefix(resp.UploadURL, "https://hub.example/api/internal/spill/blob/") {
		t.Fatalf("localfs response wrong: %+v", resp)
	}

	// Empty HubURL → falls back to request scheme+host.
	h2 := &SpillUploadAPI{Spill: &mockSpill{}, Secrets: newSecrets(t), SpillBackend: "localfs"}
	rec2 := mint(t, h2, validMintReq())
	var resp2 SpillUploadMintResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if !strings.Contains(resp2.UploadURL, "/api/internal/spill/blob/") {
		t.Fatalf("fallback URL wrong: %+v", resp2)
	}
}

func TestMint_SignError(t *testing.T) {
	// Active() failure → Sign errors (non-ErrTokenInvalid) → 500.
	h := &SpillUploadAPI{
		Spill:        &mockSpill{},
		Secrets:      badActiveSource{},
		SpillBackend: "localfs",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if rec := mint(t, h, validMintReq()); rec.Code != http.StatusInternalServerError {
		t.Fatalf("sign failure should be 500, got %d", rec.Code)
	}
}

func TestMint_UnsupportedBackend(t *testing.T) {
	h := &SpillUploadAPI{Spill: &mockSpill{}, Secrets: newSecrets(t), SpillBackend: "azure"}
	if rec := mint(t, h, validMintReq()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unsupported backend should be 503, got %d", rec.Code)
	}
}

// ---- PutSpillBlob ----

func baseBlobHandler(t *testing.T, secrets *spillupload.SecretStore, spill *mockSpill, dedup *mockDedup) *SpillUploadAPI {
	return &SpillUploadAPI{Spill: spill, Secrets: secrets, Dedup: dedup, SpillBackend: "localfs"}
}

func claimsFor(body []byte) spillupload.Claims {
	sum := sha256.Sum256(body)
	return spillupload.Claims{
		EventID:   testUUID,
		Direction: spillupload.DirectionRequest,
		Key:       "spill/" + testUUID + "/request",
		SizeBytes: int64(len(body)),
		SHA256:    hex.EncodeToString(sum[:]),
		Backend:   "localfs",
	}
}

func TestPutBlob_NotConfigured(t *testing.T) {
	h := &SpillUploadAPI{Spill: nil}
	if rec := putBlob(t, h, "tok", []byte("x"), 1); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil deps should be 503, got %d", rec.Code)
	}
}

func TestPutBlob_EmptyToken(t *testing.T) {
	h := baseBlobHandler(t, newSecrets(t), &mockSpill{}, &mockDedup{acquired: true})
	if rec := putBlob(t, h, "", []byte("x"), 1); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty token should be 400, got %d", rec.Code)
	}
}

func TestPutBlob_InvalidToken(t *testing.T) {
	h := baseBlobHandler(t, newSecrets(t), &mockSpill{}, &mockDedup{acquired: true})
	if rec := putBlob(t, h, "garbage.token.value", []byte("x"), 1); rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token should be 401, got %d", rec.Code)
	}
}

func TestPutBlob_Expired(t *testing.T) {
	// Inject a clock 10 minutes ahead so the freshly-minted token (MaxTTL=5m)
	// reads as expired at verify time → 400 TOKEN_EXPIRED.
	secrets := newSecrets(t)
	body := []byte("hello")
	token := signToken(t, secrets, claimsFor(body))
	h := baseBlobHandler(t, secrets, &mockSpill{}, &mockDedup{acquired: true})
	h.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	if rec := putBlob(t, h, token, body, int64(len(body))); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired token should be 400, got %d", rec.Code)
	}
}

func TestPutBlob_VerifyError(t *testing.T) {
	// A parseable token whose secret Lookup fails non-sententially → 500.
	realSecrets := newSecrets(t)
	body := []byte("hello")
	token := signToken(t, realSecrets, claimsFor(body))
	h := baseBlobHandler(t, realSecrets, &mockSpill{}, &mockDedup{acquired: true})
	h.Secrets = badLookupSource{} // verify-time Lookup fails with a non-sentinel error
	h.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if rec := putBlob(t, h, token, body, int64(len(body))); rec.Code != http.StatusInternalServerError {
		t.Fatalf("verify lookup failure should be 500, got %d", rec.Code)
	}
}

func TestPutBlob_BackendMismatch(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("hello")
	cl := claimsFor(body)
	cl.Backend = "s3" // token says s3, handler is localfs
	token := signToken(t, secrets, cl)
	h := baseBlobHandler(t, secrets, &mockSpill{}, &mockDedup{acquired: true})
	if rec := putBlob(t, h, token, body, int64(len(body))); rec.Code != http.StatusBadRequest {
		t.Fatalf("backend mismatch should be 400, got %d", rec.Code)
	}
}

func TestPutBlob_LengthRequired(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("hello")
	token := signToken(t, secrets, claimsFor(body))
	h := baseBlobHandler(t, secrets, &mockSpill{}, &mockDedup{acquired: true})
	if rec := putBlob(t, h, token, body, -1); rec.Code != http.StatusLengthRequired {
		t.Fatalf("missing content-length should be 411, got %d", rec.Code)
	}
}

func TestPutBlob_ContentLengthMismatch(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("hello")
	token := signToken(t, secrets, claimsFor(body)) // claims SizeBytes = 5
	h := baseBlobHandler(t, secrets, &mockSpill{}, &mockDedup{acquired: true})
	if rec := putBlob(t, h, token, body, 999); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("content-length mismatch should be 413, got %d", rec.Code)
	}
}

func TestPutBlob_DedupError(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("hello")
	token := signToken(t, secrets, claimsFor(body))
	h := baseBlobHandler(t, secrets, &mockSpill{}, &mockDedup{err: errors.New("redis down")})
	if rec := putBlob(t, h, token, body, int64(len(body))); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("dedup error should be 503, got %d", rec.Code)
	}
}

func TestPutBlob_Replay(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("hello")
	token := signToken(t, secrets, claimsFor(body))
	h := baseBlobHandler(t, secrets, &mockSpill{}, &mockDedup{acquired: false}) // SETNX says already used
	if rec := putBlob(t, h, token, body, int64(len(body))); rec.Code != http.StatusConflict {
		t.Fatalf("replay should be 409, got %d", rec.Code)
	}
}

func TestPutBlob_PutError(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("hello")
	token := signToken(t, secrets, claimsFor(body))
	spill := &mockSpill{putErr: errors.New("disk full")}
	h := baseBlobHandler(t, secrets, spill, &mockDedup{acquired: true})
	if rec := putBlob(t, h, token, body, int64(len(body))); rec.Code != http.StatusInternalServerError {
		t.Fatalf("put error should be 500, got %d", rec.Code)
	}
	if !spill.deleted {
		t.Fatal("partial object should be best-effort deleted on put error")
	}
}

func TestPutBlob_SHA256Mismatch(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("hello")
	cl := claimsFor(body)
	cl.SHA256 = strings.Repeat("b", 64) // wrong hash for body
	token := signToken(t, secrets, cl)
	spill := &mockSpill{}
	h := baseBlobHandler(t, secrets, spill, &mockDedup{acquired: true})
	rec := putBlob(t, h, token, body, int64(len(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sha mismatch should be 400, got %d", rec.Code)
	}
	if !spill.deleted {
		t.Fatal("mismatched object should be deleted")
	}
}

func TestPutBlob_Success(t *testing.T) {
	secrets := newSecrets(t)
	body := []byte("the quick brown fox")
	token := signToken(t, secrets, claimsFor(body))
	spill := &mockSpill{}
	h := baseBlobHandler(t, secrets, spill, &mockDedup{acquired: true})
	rec := putBlob(t, h, token, body, int64(len(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("happy path should be 204, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if spill.deleted {
		t.Fatal("successful upload must not delete the object")
	}
}

// logf is nil-safe; a handler with a logger should not panic either.
func TestLogfNilSafe(t *testing.T) {
	(&SpillUploadAPI{}).logf(0, "noop") // Logger nil → no-op, must not panic
}
