package spillupload

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// LoadOrInit error branches

// errMetaStore lets tests inject an error on either Get or Set without
// having to plumb separate stub types. A non-nil err pre-empts whatever
// the underlying stubMetaStore would have returned.
type errMetaStore struct {
	getErr error
	setErr error
	rows   map[string][]byte
	setBy  map[string]string
}

func newErrMetaStore() *errMetaStore {
	return &errMetaStore{
		rows:  map[string][]byte{},
		setBy: map[string]string{},
	}
}

func (s *errMetaStore) GetSystemMetadata(_ context.Context, key string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.rows[key], nil
}

func (s *errMetaStore) SetSystemMetadata(_ context.Context, key string, value any, updatedBy string) error {
	if s.setErr != nil {
		return s.setErr
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.rows[key] = b
	s.setBy[key] = updatedBy
	return nil
}

func TestLoadOrInit_NilStore(t *testing.T) {
	_, err := LoadOrInit(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil metadata store") {
		t.Fatalf("want nil-store error, got %v", err)
	}
}

func TestLoadOrInit_GetSystemMetadataError(t *testing.T) {
	boom := errors.New("db is down")
	db := newErrMetaStore()
	db.getErr = boom
	_, err := LoadOrInit(context.Background(), db)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want wrapped getErr, got %v", err)
	}
	if !strings.Contains(err.Error(), SystemMetadataKey) {
		t.Errorf("error message should name the metadata key for ops debugging: %v", err)
	}
}

func TestLoadOrInit_BadJSON_Surfaces(t *testing.T) {
	db := newErrMetaStore()
	// Hand-write an invalid JSON blob into the row so json.Unmarshal
	// fires the decode-error branch.
	db.rows[SystemMetadataKey] = []byte("{not valid json")
	_, err := LoadOrInit(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
}

func TestLoadOrInit_SetMetadataError_Surfaces(t *testing.T) {
	boom := errors.New("write denied")
	db := newErrMetaStore()
	db.setErr = boom
	_, err := LoadOrInit(context.Background(), db)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want wrapped setErr, got %v", err)
	}
	if !strings.Contains(err.Error(), "persist epoch-1") {
		t.Errorf("error should name the bootstrap step: %v", err)
	}
}

func TestLoadOrInit_SelfHealEmptyActive(t *testing.T) {
	// Operator hand-edited the row leaving active="" but a valid secret.
	// LoadOrInit must self-heal by picking the lone kid as active so
	// Verify still works after the next Hub restart.
	db := newStubMetaStore()
	preseed := map[string]any{
		"active": "",
		"secrets": map[string]string{
			"epoch-9": base64.StdEncoding.EncodeToString([]byte("seed-that-is-at-least-sixteen-bytes")),
		},
	}
	if err := db.SetSystemMetadata(context.Background(), SystemMetadataKey, preseed, "operator"); err != nil {
		t.Fatal(err)
	}
	store, err := LoadOrInit(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	kid, _, err := store.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if kid != "epoch-9" {
		t.Errorf("self-heal should pick the lone secret as active; got %q", kid)
	}
}

func TestLoadOrInit_RejectsActiveNotInSecretsMap(t *testing.T) {
	db := newStubMetaStore()
	preseed := map[string]any{
		"active": "epoch-missing",
		"secrets": map[string]string{
			"epoch-present": base64.StdEncoding.EncodeToString([]byte("a-secret-that-is-sixteen-bytes!!")),
		},
	}
	if err := db.SetSystemMetadata(context.Background(), SystemMetadataKey, preseed, "operator"); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrInit(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "active kid") {
		t.Fatalf("want active-kid-missing error, got %v", err)
	}
}

func TestLoadOrInit_DecodeSecretsBase64Error(t *testing.T) {
	db := newStubMetaStore()
	// A secret value that is not valid base64 must surface a decode
	// error at boot — failing loudly is the explicit design choice
	// over silently dropping the entry.
	preseed := map[string]any{
		"active": "epoch-1",
		"secrets": map[string]string{
			"epoch-1": "@@@ not base64 @@@",
		},
	}
	if err := db.SetSystemMetadata(context.Background(), SystemMetadataKey, preseed, "operator"); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrInit(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "decode secret") {
		t.Fatalf("want decode-secret error, got %v", err)
	}
}

// SecretStore.Active error branches (constructed directly to bypass
// LoadOrInit's invariants and exercise the defensive guards).

func TestSecretStore_Active_EmptyActive(t *testing.T) {
	s := &SecretStore{active: "", secrets: map[string][]byte{}}
	_, _, err := s.Active()
	if err == nil || !strings.Contains(err.Error(), "no active secret") {
		t.Fatalf("want no-active-secret error, got %v", err)
	}
}

func TestSecretStore_Active_KidMissingFromMap(t *testing.T) {
	// Pathological state: active points at a kid that was scrubbed from
	// the map. Active() must surface a clear error rather than return
	// nil bytes.
	s := &SecretStore{active: "epoch-7", secrets: map[string][]byte{}}
	_, _, err := s.Active()
	if err == nil || !strings.Contains(err.Error(), "missing in map") {
		t.Fatalf("want missing-in-map error, got %v", err)
	}
}

// Claims.Validate — the one branch the Sign tests can't reach because
// Sign always stamps a positive ExpiresAt before calling Validate.

func TestClaims_Validate_NonPositiveExpiry(t *testing.T) {
	c := validClaims()
	c.KID = "epoch-1"
	c.ExpiresAt = 0
	if err := c.Validate(); err == nil || !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "expiry") {
		t.Fatalf("want missing-expiry error, got %v", err)
	}
}

// Sign — Active() failure must propagate, not panic.

// failingSource returns the supplied error from Active and never resolves
// any kid via Lookup.
type failingSource struct{ active error }

func (f *failingSource) Active() (string, []byte, error) { return "", nil, f.active }
func (f *failingSource) Lookup(string) ([]byte, error)   { return nil, ErrUnknownKID }

func TestSign_ActiveErrorPropagates(t *testing.T) {
	boom := errors.New("kms unavailable")
	_, _, err := Sign(&failingSource{active: boom}, validClaims(), MaxTTL)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want wrapped active err, got %v", err)
	}
	if !strings.Contains(err.Error(), "load active secret") {
		t.Errorf("error should name the failing stage: %v", err)
	}
}

// Verify — every decode/parse/lookup error branch.

func TestVerify_MalformedToken_NoDot(t *testing.T) {
	_, err := Verify(helperStore(t), "no-dot-anywhere-in-here", time.Now())
	if err == nil || !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("want malformed-token error, got %v", err)
	}
}

func TestVerify_BadBase64Payload(t *testing.T) {
	// Payload portion contains a character outside the base64url alphabet.
	_, err := Verify(helperStore(t), "@@@.AAAA", time.Now())
	if err == nil || !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "decode payload") {
		t.Fatalf("want payload-decode error, got %v", err)
	}
}

func TestVerify_BadBase64Signature(t *testing.T) {
	// Payload is valid base64 (decodes to "{}"), signature is garbage.
	good := base64.RawURLEncoding.EncodeToString([]byte("{}"))
	_, err := Verify(helperStore(t), good+".@@@", time.Now())
	if err == nil || !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "decode sig") {
		t.Fatalf("want sig-decode error, got %v", err)
	}
}

func TestVerify_BadJSONPayload(t *testing.T) {
	// Payload decodes to bytes that aren't valid JSON.
	junk := base64.RawURLEncoding.EncodeToString([]byte("not json at all"))
	sig := base64.RawURLEncoding.EncodeToString([]byte("anything"))
	_, err := Verify(helperStore(t), junk+"."+sig, time.Now())
	if err == nil || !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "parse payload") {
		t.Fatalf("want parse-payload error, got %v", err)
	}
}

func TestVerify_MissingKID(t *testing.T) {
	// Payload is valid JSON but omits "kid".
	payload, _ := json.Marshal(map[string]any{"eid": "x", "dir": DirectionRequest})
	tok := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
	_, err := Verify(helperStore(t), tok, time.Now())
	if err == nil || !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "missing kid") {
		t.Fatalf("want missing-kid error, got %v", err)
	}
}

// lookupErrSource always returns a non-ErrUnknownKID error from Lookup so
// Verify's "secret-store backend failure" wrapping branch executes (the
// branch that turns a Redis/DB hiccup into a 5xx rather than 401).
type lookupErrSource struct {
	store *SecretStore
	boom  error
}

func (l *lookupErrSource) Active() (string, []byte, error) { return l.store.Active() }
func (l *lookupErrSource) Lookup(string) ([]byte, error)   { return nil, l.boom }

func TestVerify_LookupBackendError(t *testing.T) {
	store := helperStore(t)
	tok, _, err := Sign(store, validClaims(), MaxTTL)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	boom := errors.New("kms timeout")
	_, err = Verify(&lookupErrSource{store: store, boom: boom}, tok, time.Now())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want wrapped lookup err, got %v", err)
	}
	if errors.Is(err, ErrUnknownKID) {
		t.Errorf("backend error must not be misclassified as unknown-kid")
	}
}

// validatedAfterVerifySource signs with the helper store but the produced
// token is post-tampered to break Validate while keeping signature
// validity. Verify's "validate after HMAC ok" branch must surface the
// invariant breach.
func TestVerify_ValidateAfterHMACFails(t *testing.T) {
	// Construct a token directly with a perfectly-signed but
	// structurally-invalid payload (sizeBytes=0). We sign with the
	// active secret so HMAC passes; Validate must then reject.
	store := helperStore(t)
	kid, secret, err := store.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	bad := Claims{
		KID:       kid,
		EventID:   "evt-1",
		Direction: DirectionRequest,
		Key:       "k",
		SizeBytes: 0, // <-- invariant breach
		SHA256:    strings.Repeat("a", 64),
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Build the HMAC the same way Sign does so the signature verifies
	// and the failure has to come from claims.Validate() post-verify.
	sig := signForTest(secret, payload)
	tok := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
	_, err = Verify(store, tok, time.Now())
	if err == nil || !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "sizeBytes") {
		t.Fatalf("want sizeBytes invariant error, got %v", err)
	}
}

// GenerateSecret — happy path (entropy error is structurally unreachable
// without a seam, so it's pinned only at the contract level).

func TestGenerateSecret_ReturnsThirtyTwoBytes(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(a) != 32 {
		t.Errorf("want 32 bytes, got %d", len(a))
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret 2: %v", err)
	}
	// Two successive calls must not collide — crypto/rand would have
	// to be catastrophically broken for this to fail.
	if string(a) == string(b) {
		t.Error("two successive GenerateSecret calls produced identical bytes")
	}
}

// RedisDedup — constructor + nil-guard + error-wrap branches.

func TestNewRedisDedup_WrapsClient(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	d := NewRedisDedup(rdb)
	if d == nil {
		t.Fatal("NewRedisDedup returned nil")
	}
	if d.client != rdb {
		t.Error("NewRedisDedup did not retain the supplied client")
	}
}

func TestRedisDedup_SetNX_NilReceiver(t *testing.T) {
	var d *RedisDedup
	ok, err := d.SetNX(context.Background(), "k", time.Second)
	if err == nil || !strings.Contains(err.Error(), "nil redis client") {
		t.Fatalf("want nil-client error, got %v", err)
	}
	if ok {
		t.Error("nil-receiver must not report acquired slot")
	}
}

func TestRedisDedup_SetNX_NilInnerClient(t *testing.T) {
	d := &RedisDedup{client: nil}
	ok, err := d.SetNX(context.Background(), "k", time.Second)
	if err == nil || !strings.Contains(err.Error(), "nil redis client") {
		t.Fatalf("want nil-client error, got %v", err)
	}
	if ok {
		t.Error("nil inner client must not report acquired slot")
	}
}

func TestRedisDedup_SetNX_ErrorWrappingOnDeadDialer(t *testing.T) {
	// Point the client at a closed local TCP port so SetNX's underlying
	// dial fails synchronously inside the .Result() call. The wrapping
	// path must (a) propagate the error and (b) report acquired=false
	// so callers don't mistake a transport hiccup for a successful
	// dedup acquisition.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close() // free the port so subsequent dial RSTs immediately

	rdb := redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1, // do not retry on connection-refused
	})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	d := NewRedisDedup(rdb)
	ok, err := d.SetNX(ctx, "spillupload:test:key", time.Second)
	if err == nil {
		t.Fatal("expected SETNX error against closed port, got nil")
	}
	if !strings.Contains(err.Error(), "redis SETNX") {
		t.Errorf("error should be wrapped with package prefix: %v", err)
	}
	if ok {
		t.Error("a failed SETNX must report acquired=false")
	}
}

// signForTest mirrors the HMAC construction inside Sign so a test can
// hand-craft a token that has a valid signature but a payload Verify will
// reject on Validate.
func signForTest(secret, payload []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return mac.Sum(nil)
}
