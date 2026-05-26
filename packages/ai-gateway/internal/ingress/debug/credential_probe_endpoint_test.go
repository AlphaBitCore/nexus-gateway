// DB-integration tests for the credential probe handler.
// Uses a real local Postgres + a real cachelayer + a real MultiDecryptor
// + a stub Adapter (the only test double, allowed per CLAUDE.md test-code
// policy). Skips when DB is unavailable.

package debug

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	creddecrypt "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/decrypt"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// stubProbeAdapter is the test double for provcore.Adapter. Only Probe is
// exercised by these tests; the other methods panic to surface accidental
// callers. Configurable success / error / latency / detail behaviour lets
// each test target the response shape it cares about.
type stubProbeAdapter struct {
	format    provcore.Format
	probeOK   bool
	probeErr  error
	latencyMs int64
	detail    string
}

func (s *stubProbeAdapter) Format() provcore.Format         { return s.format }
func (s *stubProbeAdapter) SupportsShape(shape typology.WireShape) bool { return shape == typology.WireShapeOpenAIChat }
func (s *stubProbeAdapter) Probe(_ context.Context, _ provcore.CallTarget) (*provcore.ProbeResult, error) {
	if s.probeErr != nil {
		return nil, s.probeErr
	}
	return &provcore.ProbeResult{OK: s.probeOK, LatencyMs: s.latencyMs, Detail: s.detail}, nil
}

// Execute / PrepareBody / ExecuteWithBody are unreachable from the probe
// handler. Panic loudly if anyone calls them — a passing test means we
// avoided them entirely.
func (s *stubProbeAdapter) Execute(context.Context, provcore.Request) (*provcore.Response, error) {
	panic("stubProbeAdapter.Execute should not be invoked from the probe handler")
}
func (s *stubProbeAdapter) PrepareBody(req provcore.Request) ([]byte, []string, error) {
	panic("stubProbeAdapter.PrepareBody should not be invoked from the probe handler")
}
func (s *stubProbeAdapter) ExecuteWithBody(context.Context, provcore.Request, []byte, []string) (*provcore.Response, error) {
	panic("stubProbeAdapter.ExecuteWithBody should not be invoked from the probe handler")
}

// probeTestEnv wires every dependency the probe handler needs against the
// local dev DB. Tests inherit the shared cleanup via t.Cleanup.
type probeTestEnv struct {
	db      *store.DB
	cache   *cachelayer.Layer
	credMgr *credmanager.Manager
	reg     *provcore.Registry
	keyHex  string
	keyID   string
}

func newProbeTestEnv(t *testing.T) *probeTestEnv {
	t.Helper()
	dsn := envOrDefault("TEST_DATABASE_URL", "postgres://postgres:postgres@localhost:55532/nexus_gateway?sslmode=disable")
	db, err := store.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	t.Cleanup(db.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cache, err := cachelayer.New(db, logger, cachelayer.Config{})
	if err != nil {
		t.Fatalf("cachelayer.New: %v", err)
	}
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("cache.Start: %v", err)
	}

	// 32-byte hex key — same shape NewMultiDecryptor expects.
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	keyHex := hex.EncodeToString(keyBytes)
	keyID := "test"
	md, err := creddecrypt.NewMultiDecryptor(fmt.Sprintf("%s:%s", keyID, keyHex))
	if err != nil {
		t.Fatalf("NewMultiDecryptor: %v", err)
	}
	credMgr := credmanager.NewMultiKeyManager(cache, md, logger)

	return &probeTestEnv{
		db:      db,
		cache:   cache,
		credMgr: credMgr,
		reg:     provcore.NewRegistry(),
		keyHex:  keyHex,
		keyID:   keyID,
	}
}

// envOrDefault is a tiny test helper.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// encryptForKey produces hex-encoded ciphertext + iv + tag for plaintext
// under the test key. Mirrors the format ai-gateway's MultiDecryptor reads
// from Credential.encrypted{Key,Iv,Tag}.
func (e *probeTestEnv) encryptForKey(plaintext string) (cipherHex, ivHex, tagHex string) {
	keyBytes, _ := hex.DecodeString(e.keyHex)
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		panic(err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	// Go appends the 16-byte tag to ciphertext — split for storage.
	ct := sealed[:len(sealed)-16]
	tag := sealed[len(sealed)-16:]
	return hex.EncodeToString(ct), hex.EncodeToString(iv), hex.EncodeToString(tag)
}

// seedProviderForProbe inserts a Provider row with the supplied adapterType.
// Returns the new provider ID + cleanup.
func (e *probeTestEnv) seedProvider(t *testing.T, adapterType string) (string, func()) {
	t.Helper()
	id := uuid.NewString()
	_, err := e.db.Pool.Exec(context.Background(), `
		INSERT INTO "Provider" (id, name, adapter_type, "baseUrl", "pathPrefix", enabled, "createdAt", "updatedAt")
		VALUES ($1, $1, $2, 'https://example.invalid', '/v1', TRUE, NOW(), NOW())
	`, id, adapterType)
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	return id, func() { _, _ = e.db.Pool.Exec(context.Background(), `DELETE FROM "Provider" WHERE id = $1`, id) }
}

// seedCredentialForProbe inserts a Credential row with the supplied
// encrypted-key triple. Returns the new credential ID + cleanup.
func (e *probeTestEnv) seedCredential(t *testing.T, providerID, cipherHex, ivHex, tagHex string) (string, func()) {
	t.Helper()
	id := uuid.NewString()
	_, err := e.db.Pool.Exec(context.Background(), `
		INSERT INTO "Credential" (
			id, name, "providerId",
			"encryptedKey", "encryptionIv", "encryptionTag", encryption_key_id,
			enabled, "createdAt", "updatedAt"
		)
		VALUES (
			$1, $1, $2,
			$3, $4, $5, $6,
			TRUE, NOW(), NOW()
		)
	`, id, providerID, cipherHex, ivHex, tagHex, e.keyID)
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return id, func() { _, _ = e.db.Pool.Exec(context.Background(), `DELETE FROM "Credential" WHERE id = $1`, id) }
}

// reloadCaches re-loads the credential + provider snapshots so seeded
// rows are visible to the handler.
func (e *probeTestEnv) reloadCaches(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := e.cache.ReloadCredentials(ctx); err != nil {
		t.Fatalf("ReloadCredentials: %v", err)
	}
	if err := e.cache.ReloadProviders(ctx); err != nil {
		t.Fatalf("ReloadProviders: %v", err)
	}
}

// callProbe builds an http request against the handler, returns the parsed
// JSON body + status.
func (e *probeTestEnv) callProbe(t *testing.T, credID string, body string) (int, map[string]any) {
	t.Helper()
	if body == "" {
		body = "{}"
	}
	r := httptest.NewRequest(http.MethodPost,
		"/internal/v1/credentials/"+credID+"/probe", strings.NewReader(body))
	r.SetPathValue("id", credID)
	w := httptest.NewRecorder()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := CredentialProbeHandler(e.cache, e.reg, e.credMgr, logger)
	h(w, r)
	var out map[string]any
	_ = json.NewDecoder(w.Body).Decode(&out)
	return w.Code, out
}

// TestProbe_HappyPath: a properly-encrypted credential against a registered
// stub adapter returns ok=true with the latency and detail surfaced.
func TestProbe_HappyPath(t *testing.T) {
	env := newProbeTestEnv(t)
	pid, cleanProv := env.seedProvider(t, "openai")
	defer cleanProv()
	ct, iv, tag := env.encryptForKey("sk-test-1234")
	cid, cleanCred := env.seedCredential(t, pid, ct, iv, tag)
	defer cleanCred()
	env.reloadCaches(t)

	if err := env.reg.Register(&stubProbeAdapter{format: provcore.FormatOpenAI, probeOK: true, latencyMs: 17, detail: "200 OK"}); err != nil {
		t.Fatalf("register adapter: %v", err)
	}

	code, body := env.callProbe(t, cid, "")
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%v", code, body)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("ok: got %v want true; body=%v", body["ok"], body)
	}
	if got, _ := body["adapterType"].(string); got != "openai" {
		t.Errorf("adapterType: got %q want openai", got)
	}
	if got, _ := body["credentialId"].(string); got != cid {
		t.Errorf("credentialId: got %q want %q", got, cid)
	}
	if got, _ := body["detail"].(string); got != "200 OK" {
		t.Errorf("detail: got %q want '200 OK'", got)
	}
}

// TestProbe_UnknownCredential: a UUID that doesn't exist returns 404.
func TestProbe_UnknownCredential(t *testing.T) {
	env := newProbeTestEnv(t)
	code, body := env.callProbe(t, uuid.NewString(), "")
	if code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%v", code, body)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Errorf("ok: got true, want false")
	}
}

// TestProbe_MissingIDIs400: an empty path value returns 400.
func TestProbe_MissingIDIs400(t *testing.T) {
	env := newProbeTestEnv(t)
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/credentials//probe", strings.NewReader("{}"))
	r.SetPathValue("id", "")
	w := httptest.NewRecorder()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := CredentialProbeHandler(env.cache, env.reg, env.credMgr, logger)
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

// TestProbe_AdapterNotRegistered: a credential pointing at a provider
// whose adapter is not registered yields 400 "no adapter registered".
func TestProbe_AdapterNotRegistered(t *testing.T) {
	env := newProbeTestEnv(t)
	pid, cleanProv := env.seedProvider(t, "openai")
	defer cleanProv()
	ct, iv, tag := env.encryptForKey("sk-test-2")
	cid, cleanCred := env.seedCredential(t, pid, ct, iv, tag)
	defer cleanCred()
	env.reloadCaches(t)
	// Note: NO adapter registered → handler should bail.

	code, body := env.callProbe(t, cid, "")
	if code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%v", code, body)
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "no adapter") {
		t.Errorf("error: got %q, want substring 'no adapter'", got)
	}
}

// TestProbe_InvalidAdapterType: a provider row with an unknown adapter
// type returns 400 "invalid provider adapterType".
func TestProbe_InvalidAdapterType(t *testing.T) {
	env := newProbeTestEnv(t)
	pid, cleanProv := env.seedProvider(t, "not-a-real-adapter")
	defer cleanProv()
	ct, iv, tag := env.encryptForKey("sk-test-3")
	cid, cleanCred := env.seedCredential(t, pid, ct, iv, tag)
	defer cleanCred()
	env.reloadCaches(t)

	code, body := env.callProbe(t, cid, "")
	if code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%v", code, body)
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "invalid provider adapterType") {
		t.Errorf("error: got %q, want substring 'invalid provider adapterType'", got)
	}
}

// TestProbe_DecryptFailure: a credential encrypted under an unknown key
// version surfaces 500 with a decrypt error.
func TestProbe_DecryptFailure(t *testing.T) {
	env := newProbeTestEnv(t)
	pid, cleanProv := env.seedProvider(t, "openai")
	defer cleanProv()
	// Seed with a different keyID — the MultiDecryptor has only "test".
	id := uuid.NewString()
	ct, iv, tag := env.encryptForKey("sk-test-4")
	_, err := env.db.Pool.Exec(context.Background(), `
		INSERT INTO "Credential" (
			id, name, "providerId",
			"encryptedKey", "encryptionIv", "encryptionTag", encryption_key_id,
			enabled, "createdAt", "updatedAt"
		)
		VALUES ($1, $1, $2, $3, $4, $5, 'unknown-key', TRUE, NOW(), NOW())
	`, id, pid, ct, iv, tag)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer func() {
		_, _ = env.db.Pool.Exec(context.Background(), `DELETE FROM "Credential" WHERE id = $1`, id)
	}()
	env.reloadCaches(t)
	if err := env.reg.Register(&stubProbeAdapter{format: provcore.FormatOpenAI, probeOK: true}); err != nil {
		t.Fatalf("register adapter: %v", err)
	}

	code, body := env.callProbe(t, id, "")
	if code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500; body=%v", code, body)
	}
	if got, _ := body["error"].(string); !strings.Contains(got, "decrypt credential") {
		t.Errorf("error: got %q, want substring 'decrypt credential'", got)
	}
}

// TestProbe_UpstreamFailure: the stub adapter's Probe returns an error
// → handler surfaces ok=false with the error string but still 200.
// (The protocol is: 200 if the probe completed; the body's `ok` tells
// the truth.)
func TestProbe_UpstreamFailure(t *testing.T) {
	env := newProbeTestEnv(t)
	pid, cleanProv := env.seedProvider(t, "openai")
	defer cleanProv()
	ct, iv, tag := env.encryptForKey("sk-test-5")
	cid, cleanCred := env.seedCredential(t, pid, ct, iv, tag)
	defer cleanCred()
	env.reloadCaches(t)
	if err := env.reg.Register(&stubProbeAdapter{
		format:   provcore.FormatOpenAI,
		probeErr: errors.New("upstream 503"),
	}); err != nil {
		t.Fatalf("register adapter: %v", err)
	}

	code, body := env.callProbe(t, cid, "")
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (probe completed); body=%v", code, body)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Errorf("ok: got true, want false")
	}
	if got, _ := body["error"].(string); got != "upstream 503" {
		t.Errorf("error: got %q want 'upstream 503'", got)
	}
}
