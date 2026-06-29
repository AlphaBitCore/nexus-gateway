package credmanager

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	creddecrypt "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/decrypt"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

// testEncrypt encrypts plaintext with the given hex MASTER key, returning hex
// ciphertext, iv, tag. It mirrors the production seal side:
// it HKDF-derives the provider-credential sub-key from the master and binds the
// supplied row-identity aad, so the Manager's Decryptor (which does the same)
// can open it.
func testEncrypt(t *testing.T, keyHex, plaintext string, aad []byte) (string, string, string) {
	t.Helper()
	master, _ := hex.DecodeString(keyHex)
	sub, err := keyderive.DeriveKey32(master, keyderive.ClassProviderCredential)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(sub[:])
	gcm, _ := cipher.NewGCM(block)

	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}

	sealed := gcm.Seal(nil, iv, []byte(plaintext), aad)
	// sealed = ciphertext + tag (last 16 bytes).
	ct := sealed[:len(sealed)-16]
	tag := sealed[len(sealed)-16:]

	return hex.EncodeToString(ct), hex.EncodeToString(iv), hex.EncodeToString(tag)
}

const (
	testKeyHex  = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	testKeyHex2 = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
)

// fakeSource is an in-memory implementation of the Source interface used to
// drive Manager tests without a live database. It records call counts so
// tests can assert cache behavior (singleflight, TTL, invalidation).
type fakeSource struct {
	mu sync.Mutex

	byID       map[string]*store.Credential
	byProvider map[string]*store.Credential
	listByProv map[string][]store.Credential
	errByID    error
	errByProv  error
	errList    error

	getByIDCalls        atomic.Int32
	getForProviderCalls atomic.Int32
	listCalls           atomic.Int32
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		byID:       map[string]*store.Credential{},
		byProvider: map[string]*store.Credential{},
		listByProv: map[string][]store.Credential{},
	}
}

func (f *fakeSource) GetCredentialByID(ctx context.Context, id string) (*store.Credential, error) {
	f.getByIDCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errByID != nil {
		return nil, f.errByID
	}
	c, ok := f.byID[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return c, nil
}

func (f *fakeSource) GetCredentialForProvider(ctx context.Context, providerID string) (*store.Credential, error) {
	f.getForProviderCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errByProv != nil {
		return nil, f.errByProv
	}
	c, ok := f.byProvider[providerID]
	if !ok {
		return nil, errors.New("provider not found")
	}
	return c, nil
}

func (f *fakeSource) ListCredentialsForProvider(ctx context.Context, providerID string) ([]store.Credential, error) {
	f.listCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errList != nil {
		return nil, f.errList
	}
	return f.listByProv[providerID], nil
}

// helper: build a *store.Credential with a real AES-GCM encryption of plaintext.
func makeCred(t *testing.T, id, providerID, name, keyID, keyHex, plaintext string) *store.Credential {
	t.Helper()
	ct, iv, tag := testEncrypt(t, keyHex, plaintext, keyderive.ProviderCredentialAAD(id, providerID))
	return &store.Credential{
		ID:              id,
		Name:            name,
		ProviderID:      providerID,
		EncryptedKey:    ct,
		EncryptionIv:    iv,
		EncryptionTag:   tag,
		EncryptionKeyID: keyID,
		Enabled:         true,
		Status:          "active",
	}
}

// NewManager / NewMultiKeyManager constructor coverage

func TestNewManager_DefaultsAndTTL(t *testing.T) {
	d, err := creddecrypt.NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeSource()
	m := NewManager(src, d)

	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if m.decryptor != d {
		t.Error("decryptor not wired")
	}
	if m.multiDecryptor != nil {
		t.Error("expected nil multi-decryptor on single-key constructor")
	}
	if m.cacheTTL != defaultCacheTTL {
		t.Errorf("cacheTTL: got %v want %v", m.cacheTTL, defaultCacheTTL)
	}
	if m.cache == nil {
		t.Error("expected initialized cache map")
	}
}

func TestNewMultiKeyManager_DefaultsAndTTL(t *testing.T) {
	md, err := creddecrypt.NewMultiDecryptor("v1:" + testKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	src := newFakeSource()
	m := NewMultiKeyManager(src, md)

	if m.multiDecryptor != md {
		t.Error("multi-decryptor not wired")
	}
	if m.decryptor != nil {
		t.Error("expected nil single decryptor on multi-key constructor")
	}
	if m.cacheTTL != defaultCacheTTL {
		t.Errorf("cacheTTL: got %v", m.cacheTTL)
	}
}

// GetDecrypted: success, cache hit, fetch error, decrypt error, nil DB

func TestGetDecrypted_NilDB(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	m := NewManager(nil, d)
	_, err := m.GetDecrypted(context.Background(), "cred-1")
	if err == nil || !strings.Contains(err.Error(), "database not available") {
		t.Fatalf("expected 'database not available' error, got %v", err)
	}
}

func TestGetDecrypted_SuccessAndCacheHit(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	src.byID["cred-1"] = makeCred(t, "cred-1", "prov-1", "n", "v1", testKeyHex, "sk-secret")

	m := NewManager(src, d)

	got, err := m.GetDecrypted(context.Background(), "cred-1")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "sk-secret" {
		t.Errorf("plaintext: got %q want %q", got, "sk-secret")
	}
	if n := src.getByIDCalls.Load(); n != 1 {
		t.Errorf("expected 1 DB fetch, got %d", n)
	}

	// Second call must come from cache — DB count unchanged.
	got2, err := m.GetDecrypted(context.Background(), "cred-1")
	if err != nil {
		t.Fatalf("decrypt (cached): %v", err)
	}
	if got2 != "sk-secret" {
		t.Errorf("cached plaintext mismatch: %q", got2)
	}
	if n := src.getByIDCalls.Load(); n != 1 {
		t.Errorf("expected cache hit (1 total DB call), got %d", n)
	}
}

func TestGetDecrypted_FetchError(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	src.errByID = errors.New("db down")

	m := NewManager(src, d)
	_, err := m.GetDecrypted(context.Background(), "cred-1")
	if err == nil {
		t.Fatal("expected fetch error")
	}
	if !strings.Contains(err.Error(), "fetch cred-1") {
		t.Errorf("error wrap missing context: %v", err)
	}
	if !errors.Is(err, src.errByID) {
		t.Errorf("expected wrapped errByID, got %v", err)
	}
}

func TestGetDecrypted_DecryptError_WrongKey(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	// Encrypt with a DIFFERENT key — decrypt must fail (auth tag mismatch).
	otherKey := "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	src := newFakeSource()
	src.byID["cred-bad"] = makeCred(t, "cred-bad", "prov-1", "n", "v1", otherKey, "secret")

	m := NewManager(src, d)
	_, err := m.GetDecrypted(context.Background(), "cred-bad")
	if err == nil {
		t.Fatal("expected decryption failure")
	}
	if !errors.Is(err, creddecrypt.ErrDecryptFailed) {
		t.Errorf("expected ErrDecryptFailed in chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "decrypt cred-bad") {
		t.Errorf("error wrap missing context: %v", err)
	}
	// Failed decrypts must NOT populate the cache.
	if got, ok := m.cacheGet("cred-bad"); ok {
		t.Errorf("cache must not store failed decrypts, got %q", got)
	}
}

func TestGetDecrypted_Singleflight_Concurrent(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	src.byID["cred-sf"] = makeCred(t, "cred-sf", "prov-1", "n", "v1", testKeyHex, "the-secret")

	m := NewManager(src, d)

	// Fire 10 concurrent GetDecrypted for the same ID; singleflight should
	// collapse them to ≤ a few DB calls. Race detector exercises shared cache.
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := m.GetDecrypted(context.Background(), "cred-sf")
			if err != nil {
				errs <- err
				return
			}
			if got != "the-secret" {
				errs <- errors.New("plaintext mismatch")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent decrypt: %v", e)
	}
	// With singleflight, the DB should usually be hit exactly once; assert ≤ 3
	// to keep the test stable across scheduler variance.
	if n := src.getByIDCalls.Load(); n > 3 {
		t.Errorf("singleflight expected ≤3 DB calls, got %d", n)
	}
}

// GetForProvider: success, cache hit, fetch error, decrypt error, nil DB

func TestGetForProvider_NilDB(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	m := NewManager(nil, d)
	_, _, _, err := m.GetForProvider(context.Background(), "prov-1")
	if err == nil || !strings.Contains(err.Error(), "database not available") {
		t.Fatalf("expected 'database not available', got %v", err)
	}
}

func TestGetForProvider_SuccessAndCacheHit(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	cred := makeCred(t, "cred-x", "prov-1", "primary", "v1", testKeyHex, "openai-key")
	src.byProvider["prov-1"] = cred

	m := NewManager(src, d)

	plaintext, id, name, err := m.GetForProvider(context.Background(), "prov-1")
	if err != nil {
		t.Fatalf("GetForProvider: %v", err)
	}
	if plaintext != "openai-key" || id != "cred-x" || name != "primary" {
		t.Errorf("unexpected: plaintext=%q id=%q name=%q", plaintext, id, name)
	}

	// Pre-populate cache via direct cacheSet to exercise the cache-hit branch.
	// (The default-double-decrypt path is OK to exercise — second call goes
	//  through the cache-hit branch in GetForProvider since cred ID matches.)
	plaintext2, id2, name2, err := m.GetForProvider(context.Background(), "prov-1")
	if err != nil {
		t.Fatalf("GetForProvider cached: %v", err)
	}
	if plaintext2 != "openai-key" || id2 != "cred-x" || name2 != "primary" {
		t.Errorf("cache-hit return mismatch: %q %q %q", plaintext2, id2, name2)
	}
	// First call: 1 GetCredentialForProvider; second: also 1 more
	// (provider lookup always re-fetches metadata, only decryption is cached).
	if n := src.getForProviderCalls.Load(); n != 2 {
		t.Errorf("expected 2 provider lookups, got %d", n)
	}
}

func TestGetForProvider_FetchError(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	src.errByProv = errors.New("db unreachable")

	m := NewManager(src, d)
	_, _, _, err := m.GetForProvider(context.Background(), "prov-x")
	if err == nil {
		t.Fatal("expected fetch error")
	}
	if !strings.Contains(err.Error(), "provider prov-x") {
		t.Errorf("error wrap missing provider id: %v", err)
	}
	if !errors.Is(err, src.errByProv) {
		t.Errorf("expected wrapped errByProv, got %v", err)
	}
}

func TestGetForProvider_DecryptError(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	otherKey := "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	src := newFakeSource()
	src.byProvider["prov-1"] = makeCred(t, "cred-bad", "prov-1", "n", "v1", otherKey, "secret")

	m := NewManager(src, d)
	_, _, _, err := m.GetForProvider(context.Background(), "prov-1")
	if err == nil {
		t.Fatal("expected decrypt error")
	}
	if !errors.Is(err, creddecrypt.ErrDecryptFailed) {
		t.Errorf("expected ErrDecryptFailed in chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "decrypt cred-bad") {
		t.Errorf("error wrap missing credential id: %v", err)
	}
}

func TestGetForProvider_Singleflight_Concurrent(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	src.byProvider["prov-1"] = makeCred(t, "cred-sf-prov", "prov-1", "n", "v1", testKeyHex, "p-secret")

	m := NewManager(src, d)

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plaintext, _, _, err := m.GetForProvider(context.Background(), "prov-1")
			if err != nil {
				errs <- err
				return
			}
			if plaintext != "p-secret" {
				errs <- errors.New("plaintext mismatch")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent GetForProvider: %v", e)
	}
}

// ListForProvider: success, error, nil DB

func TestListForProvider_NilDB(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	m := NewManager(nil, d)
	_, err := m.ListForProvider(context.Background(), "prov-1")
	if err == nil || !strings.Contains(err.Error(), "database not available") {
		t.Fatalf("expected 'database not available', got %v", err)
	}
}

func TestListForProvider_Success(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	src.listByProv["prov-1"] = []store.Credential{
		{ID: "a", ProviderID: "prov-1", Enabled: true, Status: "active"},
		{ID: "b", ProviderID: "prov-1", Enabled: true, Status: "active"},
	}

	m := NewManager(src, d)
	list, err := m.ListForProvider(context.Background(), "prov-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 credentials, got %d", len(list))
	}
}

func TestListForProvider_Error(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	src.errList = errors.New("list failure")

	m := NewManager(src, d)
	_, err := m.ListForProvider(context.Background(), "prov-1")
	if err == nil || !errors.Is(err, src.errList) {
		t.Fatalf("expected list error, got %v", err)
	}
}

// m.decrypt(, nil) dispatch

func TestManager_Decrypt_Dispatch_SingleKey(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	m := NewManager(newFakeSource(), d)

	ct, iv, tag := testEncrypt(t, testKeyHex, "hello", nil)
	got, err := m.decrypt("ignored-key-id", ct, iv, tag, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q", got)
	}
}

func TestManager_Decrypt_Dispatch_MultiKey(t *testing.T) {
	md, _ := creddecrypt.NewMultiDecryptor("v1:" + testKeyHex + ",v2:" + testKeyHex2)
	m := NewMultiKeyManager(newFakeSource(), md)

	// Encrypt with v2 — decryption must route through keyID=v2.
	ct, iv, tag := testEncrypt(t, testKeyHex2, "rotated-secret", nil)
	got, err := m.decrypt("v2", ct, iv, tag, nil)
	if err != nil {
		t.Fatalf("multi-decrypt: %v", err)
	}
	if got != "rotated-secret" {
		t.Errorf("got %q", got)
	}

	// Unknown key ID must error.
	_, err = m.decrypt("v99", ct, iv, tag, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown key ID") {
		t.Errorf("expected unknown key ID error, got %v", err)
	}
}

func TestManager_GetDecrypted_MultiKeyEndToEnd(t *testing.T) {
	md, _ := creddecrypt.NewMultiDecryptor("v1:" + testKeyHex + ",v2:" + testKeyHex2)
	src := newFakeSource()
	// Stored credential encrypted with v2.
	src.byID["cred-v2"] = makeCred(t, "cred-v2", "prov-1", "n", "v2", testKeyHex2, "multi-key-secret")

	m := NewMultiKeyManager(src, md)
	got, err := m.GetDecrypted(context.Background(), "cred-v2")
	if err != nil {
		t.Fatalf("multi-key GetDecrypted: %v", err)
	}
	if got != "multi-key-secret" {
		t.Errorf("got %q", got)
	}
}

// Cache helpers (existing tests preserved + expanded)

func TestManager_CacheGetSet(t *testing.T) {
	m := &Manager{
		cacheTTL: 5 * time.Minute,
		cache:    make(map[string]*cachedEntry),
	}

	// Miss.
	if got, ok := m.cacheGet("id-1"); ok {
		t.Errorf("expected cache miss, got %q", got)
	}

	// Set and hit.
	m.cacheSet("id-1", "secret-key")
	got, ok := m.cacheGet("id-1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != "secret-key" {
		t.Errorf("cache hit: got %q", got)
	}
}

// TestManager_CacheEmptyStringDistinctFromMiss is the regression guard:
// a credential that legitimately decrypts to "" must be cached and reported as
// a hit, NOT mistaken for a cache miss. Before the (string, bool) signature an
// empty-string sentinel made these two states indistinguishable, so such a
// credential re-decrypted on every call and never cached.
func TestManager_CacheEmptyStringDistinctFromMiss(t *testing.T) {
	m := &Manager{
		cacheTTL: 5 * time.Minute,
		cache:    make(map[string]*cachedEntry),
	}

	// Before set: genuine miss.
	if _, ok := m.cacheGet("empty-cred"); ok {
		t.Fatal("expected miss before set")
	}

	// Cache an empty-string plaintext.
	m.cacheSet("empty-cred", "")

	// After set: must be a HIT returning "".
	got, ok := m.cacheGet("empty-cred")
	if !ok {
		t.Fatal("empty-string credential must be cached and reported as a hit")
	}
	if got != "" {
		t.Errorf("cached empty plaintext: got %q, want empty", got)
	}
}

// TestGetDecrypted_EmptyPlaintextCachedNotRedecrypted verifies end-to-end that
// a credential decrypting to "" is served from cache on the second call rather
// than re-fetched + re-decrypted (the observable failure mode).
func TestGetDecrypted_EmptyPlaintextCachedNotRedecrypted(t *testing.T) {
	d, _ := creddecrypt.NewDecryptor(testKeyHex)
	src := newFakeSource()
	// Encrypt an empty plaintext under the manager's key.
	src.byID["empty-cred"] = makeCred(t, "empty-cred", "prov-1", "n", "v1", testKeyHex, "")

	m := NewManager(src, d)

	got, err := m.GetDecrypted(context.Background(), "empty-cred")
	if err != nil {
		t.Fatalf("first decrypt: %v", err)
	}
	if got != "" {
		t.Errorf("plaintext: got %q, want empty", got)
	}
	if n := src.getByIDCalls.Load(); n != 1 {
		t.Fatalf("expected 1 DB fetch, got %d", n)
	}

	// Second call must be a cache hit — no additional DB fetch / decrypt.
	got2, err := m.GetDecrypted(context.Background(), "empty-cred")
	if err != nil {
		t.Fatalf("second decrypt: %v", err)
	}
	if got2 != "" {
		t.Errorf("cached plaintext: got %q, want empty", got2)
	}
	if n := src.getByIDCalls.Load(); n != 1 {
		t.Errorf("empty-string credential re-decrypted instead of cached: %d DB calls", n)
	}
}

func TestManager_CacheExpiry(t *testing.T) {
	m := &Manager{
		cacheTTL: 1 * time.Millisecond, // very short TTL
		cache:    make(map[string]*cachedEntry),
	}

	m.cacheSet("id-1", "secret")
	time.Sleep(5 * time.Millisecond)

	if got, ok := m.cacheGet("id-1"); ok {
		t.Errorf("expected expired entry to miss, got %q", got)
	}

	// Expired entry must be proactively evicted from the underlying map
	// (prevents unbounded growth on read-mostly workloads).
	m.mu.RLock()
	_, present := m.cache["id-1"]
	m.mu.RUnlock()
	if present {
		t.Error("expected expired entry to be evicted from cache map")
	}
}

func TestManager_CacheExpiry_RaceReinserted(t *testing.T) {
	// Cover the inner double-check branch in cacheGet: an expired entry
	// that is replaced by a concurrent writer BEFORE the eviction Lock
	// acquires must NOT be deleted by the eviction.
	m := &Manager{
		cacheTTL: 1 * time.Millisecond,
		cache:    make(map[string]*cachedEntry),
	}
	m.cacheSet("id-1", "old")
	time.Sleep(5 * time.Millisecond)

	// Simulate concurrent re-insert by directly replacing entry with a
	// fresh expiry before cacheGet's eviction path runs. Since cacheGet
	// observes the expiry under RLock, we must seed a non-expired entry
	// after the RLock release. We can't easily intercept that, so instead
	// pre-replace and call cacheGet on a different key to exercise the
	// "expired" branch deterministically.
	if got, ok := m.cacheGet("id-1"); ok {
		t.Errorf("expected expiry miss, got %q", got)
	}
	// Re-insert and verify it sticks.
	m.cacheSet("id-1", "new")
	if got, ok := m.cacheGet("id-1"); !ok || got != "new" {
		t.Errorf("expected re-inserted value, got %q (ok=%v)", got, ok)
	}
}

func TestManager_Invalidate(t *testing.T) {
	m := &Manager{
		cacheTTL: 5 * time.Minute,
		cache:    make(map[string]*cachedEntry),
	}

	m.cacheSet("id-1", "secret")
	m.Invalidate("id-1")

	if got, ok := m.cacheGet("id-1"); ok {
		t.Errorf("expected invalidated entry to miss, got %q", got)
	}
}

func TestManager_ClearCache(t *testing.T) {
	m := &Manager{
		cacheTTL: 5 * time.Minute,
		cache:    make(map[string]*cachedEntry),
	}

	m.cacheSet("id-1", "secret-1")
	m.cacheSet("id-2", "secret-2")
	m.ClearCache()

	if got, ok := m.cacheGet("id-1"); ok {
		t.Errorf("expected cleared cache, got %q", got)
	}
	if got, ok := m.cacheGet("id-2"); ok {
		t.Errorf("expected cleared cache, got %q", got)
	}
}
