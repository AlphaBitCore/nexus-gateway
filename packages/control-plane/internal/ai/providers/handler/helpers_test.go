// Shared test plumbing for the providers handler package: pgxmock-backed
// *store.DB, audit MQ spy, hub-invalidator spy, redis (miniredis) factory,
// and Echo context builders. All siblings in this file are intentionally
// tiny so individual *_test.go files stay focused on per-handler assertions.
package providers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	cpgx "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/pgx"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// silentLogger discards output so error-path tests don't spam stderr.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// auditSpy implements mq.Producer (Publish/Enqueue/Close) so audit.Writer
// publishes into an in-memory buffer instead of NATS.
type auditSpy struct {
	mu    sync.Mutex
	calls [][]byte
}

func (a *auditSpy) Publish(context.Context, string, []byte) error { return nil }
func (a *auditSpy) Enqueue(_ context.Context, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	a.calls = append(a.calls, cp)
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

// hubSpy records every InvalidateConfig fan-out for assertion.
type hubSpy struct {
	mu    sync.Mutex
	calls []string // "thingType/configKey"
}

func (h *hubSpy) InvalidateConfig(_ context.Context, thingType, configKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, thingType+"/"+configKey)
}

func (h *hubSpy) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.calls))
	copy(out, h.calls)
	return out
}

// newMockStore returns a pgxmock-backed *store.DB.
func newMockStore(t *testing.T) (pgxmock.PgxPoolIface, *store.DB) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewWithPgxPool(mock)
}

// newMiniRedis returns a miniredis + go-redis client wired through it,
// suitable for the credential circuit / withCircuit live-view tests.
func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// newHandler constructs a providers Handler with sensible defaults. Callers
// pass nil for anything they don't need; the audit writer is always real
// (with the spy producer) so EntryFor → LogObserved paths exercise json
// marshalling.
func newHandler(db *store.DB, hub HubInvalidator, aud *auditSpy, rdb redis.UniversalClient,
	vault *crypto.Vault, multi *crypto.MultiVault, proxy ProxyConfig) *Handler {
	var pool cpgx.PgxPool
	if db != nil {
		pool = db.InternalPool()
	}
	return New(Deps{
		Pool:       pool,
		Hub:        hub,
		Audit:      audit.NewWriter(aud, "nexus.event.admin-audit", silentLogger()),
		Logger:     silentLogger(),
		Vault:      vault,
		MultiVault: multi,
		Proxy:      proxy,
		Redis:      rdb,
	})
}

// echoCtx builds an Echo context with an authenticated admin attached so the
// audit middleware extraction works end-to-end.
func echoCtx(req *http.Request, rec *httptest.ResponseRecorder, userID string) (echo.Context, *echo.Echo) {
	e := echo.New()
	c := e.NewContext(req, rec)
	middleware.WithAdminAuth(c, &auth.AdminAuth{
		KeyID:             userID,
		KeyName:           "admin-" + userID,
		AuthPrincipalType: "admin_user",
	})
	return c, e
}

// anonEchoCtx is echoCtx without an attached admin auth — exercises the
// nil-auth branches of actorFromContext / isSuperAdmin.
func anonEchoCtx(req *http.Request, rec *httptest.ResponseRecorder) echo.Context {
	e := echo.New()
	return e.NewContext(req, rec)
}

// Test fixtures: column lists + row builders mirroring store/

// providerCols mirrors the 13-column GetProvider / Create / Update projection.
var providerCols = []string{
	"id", "name", "displayName", "description", "adapter_type", "baseUrl",
	"pathPrefix", "apiVersion", "region", "enabled", "headers",
	"createdAt", "updatedAt",
}

// providerListCols appends model_count for the ListProviders projection.
var providerListCols = append(append([]string{}, providerCols...), "model_count")

func strPtr(s string) *string   { return &s }
func intPtr(n int) *int         { return &n }
func f64Ptr(f float64) *float64 { return &f }

// nowFixture returns a stable UTC timestamp suitable for fixture rows.
func nowFixture() time.Time { return time.Now().UTC().Truncate(time.Second) }

func makeProviderRow(now time.Time) []any {
	displayName := "Test Provider"
	desc := "A test provider"
	apiVer := "2025-01-01"
	region := "us-east-1"
	return []any{
		"prov-1", "test-provider", &displayName, &desc, "openai",
		"https://api.test.com", "/test", &apiVer, &region, true,
		json.RawMessage(`{"x":"y"}`), now, now,
	}
}

// modelCols mirrors the 25-column modelColumns projection (includes 4
// capability matrix columns: inputModalities, outputModalities,
// lifecycle, capabilityJson; and 2 cached price columns
// cachedInputReadPricePerMillion + cachedInputWritePricePerMillion).
var modelCols = []string{
	"id", "code", "name", "description", "providerId", "providerModelId",
	"type", "features", "inputPricePerMillion", "outputPricePerMillion",
	"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
	"maxContextTokens", "maxOutputTokens", "status", "deprecationDate",
	"replacedBy", "aliases",
	"inputModalities", "outputModalities", "lifecycle", "capabilityJson",
	"enabled", "createdAt", "updatedAt",
}

func makeModelRow(now time.Time) []any {
	desc := "A test model"
	replacedBy := "gpt-5"
	return []any{
		"model-1", "gpt-4o", "GPT-4o", &desc, "prov-1", "gpt-4o-2024-08-06",
		"chat", []string{"vision", "tools"},
		f64Ptr(2.5), f64Ptr(10.0),
		f64Ptr(0.3), f64Ptr(3.75),
		intPtr(128000), intPtr(16384),
		"active", &now,
		&replacedBy, []string{"gpt4o"},
		[]string{"text"}, []string{"text"}, "ga", jsonRawPtr(`{}`),
		true, now, now,
	}
}

// jsonRawPtr returns *json.RawMessage for the embedded JSONB capability
// column — Scan target type matches modelstore.Model.CapabilityJson.
func jsonRawPtr(s string) *json.RawMessage {
	v := json.RawMessage(s)
	return &v
}

// credentialMetadataCols mirrors the 30-column credMetadataColumns projection.
var credentialMetadataCols = []string{
	"id", "name", "providerId", "enabled", "rotationState",
	"lastRotatedAt", "lastUsedAt", "lastSuccessAt", "lastFailureAt",
	"lastFailureReason", "totalUsageCount", "expiresAt",
	"selectionWeight", "status", "retireAt",
	"circuitState", "circuitReason", "circuitOpenedAt", "circuitNextProbeAt",
	"healthStatus", "healthSuccessRate5m", "healthSuccessRate1h", "healthSamplesObserved",
	"healthDominantError", "healthTrend", "healthStatusChangedAt", "healthCheckedAt",
	"reliabilityOverrides",
	"createdAt", "updatedAt",
}

// credentialEncryptedCols extends credentialMetadataCols with the 4 encrypted
// fields ListCredentialsForRotation / GetCredentialEncrypted append.
var credentialEncryptedCols = append(append([]string{}, credentialMetadataCols...),
	"encryptedKey", "encryptionIv", "encryptionTag", "encryption_key_id")

func makeCredentialRow(now time.Time) []any {
	rotState := "active"
	failReason := "auth_failed"
	circuitReason := "5xx_burst"
	healthErr := "timeout"
	healthTrend := "stable"
	return []any{
		"cred-1", "test-cred", "prov-1", true, &rotState,
		&now, &now, &now, &now,
		&failReason, 42, &now,
		100, "active", &now,
		"closed", &circuitReason, &now, &now,
		"healthy", f64Ptr(0.95), f64Ptr(0.97), 200,
		&healthErr, &healthTrend, &now, &now,
		json.RawMessage(`{"errorRate":0.1}`),
		now, now,
	}
}

// makeCredentialEncryptedRow extends makeCredentialRow with the four
// encrypted-key fields ListCredentialsForRotation / GetCredentialEncrypted
// append.
func makeCredentialEncryptedRow(now time.Time, keyID string) []any {
	return append(makeCredentialRow(now), "enc-key-blob", "enc-iv", "enc-tag", keyID)
}

// makeProviderInsertWithChildrenCredRow returns the 14-column row shape that
// CreateProviderWithChildren scans back for the inline credential — note
// this is a DIFFERENT projection than credMetadataColumns (only 14 cols).
func makeProviderInsertWithChildrenCredRow(now time.Time) []any {
	rotState := "none"
	failReason := "n/a"
	return []any{
		"cred-1", "test-cred", "prov-1", true, &rotState,
		&now, &now, &now, &now,
		&failReason, 0, &now, now, now,
	}
}

// newTestVault returns a Vault with a deterministic 32-byte key for tests
// that exercise the legacy single-key encryption path.
func newTestVault(t *testing.T) *crypto.Vault {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	v, err := crypto.NewVault(key)
	if err != nil {
		t.Fatalf("new vault: %v", err)
	}
	return v
}

// v1RawKey + v2RawKey are the raw 32-byte keys behind newTestMultiVault's
// "v1"/"v2" pair, exported so tests can build a standalone Vault for the
// non-current key (needed to seed ciphertext that the rotation worker will
// re-encrypt to the current key).
var (
	v1RawKey, _ = hexBytes("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	v2RawKey, _ = hexBytes("ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")
)

func hexBytes(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var b byte
		for j := range 2 {
			c := s[i+j]
			var nibble byte
			switch {
			case c >= '0' && c <= '9':
				nibble = c - '0'
			case c >= 'a' && c <= 'f':
				nibble = c - 'a' + 10
			}
			b = (b << 4) | nibble
		}
		out[i/2] = b
	}
	return out, nil
}

// newTestMultiVault returns a MultiVault with two keys ("v1" and "v2"; "v2"
// is current) for tests that exercise the multi-key rotation paths.
func newTestMultiVault(t *testing.T) *crypto.MultiVault {
	t.Helper()
	keyMap := "v1:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff," +
		"v2:ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	mv, err := crypto.NewMultiVault(keyMap, silentLogger())
	if err != nil {
		t.Fatalf("new multi vault: %v", err)
	}
	return mv
}

// vaultForKey returns a Vault seeded with the raw 32-byte key for the named
// key version. Use this when a test needs to seed ciphertext under a key
// that is NOT the MultiVault's current key (e.g. to drive the rotation
// worker from v1 → v2).
func vaultForKey(t *testing.T, keyID string) *crypto.Vault {
	t.Helper()
	var raw []byte
	switch keyID {
	case "v1":
		raw = v1RawKey
	case "v2":
		raw = v2RawKey
	default:
		t.Fatalf("vaultForKey: unknown id %q", keyID)
	}
	v, err := crypto.NewVault(raw)
	if err != nil {
		t.Fatalf("vaultForKey: %v", err)
	}
	return v
}
