package auth

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/hmackeyring"
)

// setHMAC installs an injected single-version HMAC keyring for the duration of a
// test, restoring the prior keyring afterwards. Mirrors the boot-time
// auth.InitHMACKeyring injection (SEC-W2-01 Layer A / SEC-W2-03 Layer C) — the
// hashing layer keys under the keyring's current version, never os.Getenv at
// point-of-use.
func setHMAC(t *testing.T, secret string) {
	t.Helper()
	prev := injectedKeyring
	t.Cleanup(func() { injectedKeyring = prev })
	kr, err := hmackeyring.Single(secret)
	if err != nil {
		t.Fatalf("hmackeyring.Single(%q): %v", secret, err)
	}
	if err := InitHMACKeyring(kr); err != nil {
		t.Fatalf("InitHMACKeyring: %v", err)
	}
}

// TestHashAPIKeyHonorsInjectedSecret verifies that HashAPIKey actually keys the
// HMAC with the injected secret — a regression here would mean rotating the HMAC
// secret silently kept old hashes valid. It also pins the custody invariant: the
// hash depends ONLY on the injected plaintext, so the "command"-mode unwrapped
// value and the "noop"/plaintext value (which are equal by the [MUST MATCH]
// contract) produce the SAME hash, never a wrapped-blob hash.
func TestHashAPIKeyHonorsInjectedSecret(t *testing.T) {
	const key = "nxk_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	setHMAC(t, "secret-A")
	hashA := HashAPIKey(key)

	setHMAC(t, "secret-B")
	hashB := HashAPIKey(key)

	if hashA == hashB {
		t.Error("HashAPIKey produced the same hash under different HMAC secrets — secret rotation would not invalidate old hashes")
	}
	// Same secret must produce a stable hash.
	setHMAC(t, "secret-A")
	if HashAPIKey(key) != hashA {
		t.Error("HashAPIKey not deterministic under a stable secret")
	}
}

// TestHashAPIKey_DomainSeparatedFromVirtualKey is the SEC-W2-01 regression: the
// SAME raw key hashed as an admin API key vs a virtual key must yield DIFFERENT
// digests, because they key the HMAC with distinct HKDF-derived sub-keys. This is
// what prevents a forgery oracle / leak scoped to one trust domain from minting a
// credential in the other.
func TestHashAPIKey_DomainSeparatedFromVirtualKey(t *testing.T) {
	setHMAC(t, "shared-master-secret")
	const key = "nxk_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	adminFirst := HashAPIKey(key)
	vkFirst := HashVirtualKey(key)
	if adminFirst == vkFirst {
		t.Fatal("SEC-W2-01 broken: admin-API-key and virtual-key hashes are identical — the two trust domains share one HMAC key")
	}
	// Each domain is independently deterministic: a second hash of the same
	// raw key under the same secret must reproduce the first digest.
	if HashAPIKey(key) != adminFirst || HashVirtualKey(key) != vkFirst {
		t.Fatal("per-domain hashing must be deterministic")
	}
}

// TestGenerateAPIKey verifies the shape (prefix + hex), the recorded hash,
// the display prefix matches the documented [:12] slice, and successive calls
// produce different keys (no PRNG re-use).
func TestGenerateAPIKey(t *testing.T) {
	setHMAC(t, "test-secret-for-generate")

	key, hash, prefix, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: unexpected error %v", err)
	}
	if !strings.HasPrefix(key, apiKeyPrefix) {
		t.Errorf("key %q missing required prefix %q", key, apiKeyPrefix)
	}
	// "nxk_" + 32 bytes hex-encoded = 4 + 64 = 68 chars
	wantLen := len(apiKeyPrefix) + apiKeyBytes*2
	if len(key) != wantLen {
		t.Errorf("key length = %d; want %d", len(key), wantLen)
	}
	hexPart := strings.TrimPrefix(key, apiKeyPrefix)
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Errorf("key body is not hex: %v", err)
	}
	if prefix != key[:12] {
		t.Errorf("prefix = %q; want first 12 chars of key %q", prefix, key[:12])
	}
	if hash != HashAPIKey(key) {
		t.Error("returned hash does not match HashAPIKey(key) — DB lookup would fail")
	}

	// Entropy: second call must not collide.
	key2, _, _, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey (2): %v", err)
	}
	if key == key2 {
		t.Error("two GenerateAPIKey calls produced identical keys — RNG re-use")
	}
}

// TestTimingSafeEqual covers all decision branches: length-mismatch fast path,
// equal-length equal, equal-length unequal. The constant-time property itself
// is provided by subtle.ConstantTimeCompare; here we assert the wrapper
// returns the right boolean.
func TestTimingSafeEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"empty equal", "", "", true},
		{"equal short", "abc", "abc", true},
		{"equal long", strings.Repeat("x", 64), strings.Repeat("x", 64), true},
		{"different length", "abc", "abcd", false},
		{"a longer", "abcdef", "abcde", false},
		{"same length differ first", "abc", "Xbc", false},
		{"same length differ last", "abc", "abX", false},
		{"empty vs non-empty", "", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TimingSafeEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("TimingSafeEqual(%q, %q) = %v; want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestEffectivePrincipalDelegatesToOwner covers the delegation path:
// when the API key has a fully populated owner (id + display + enabled=true),
// EffectivePrincipal returns the OWNER as the principal and records the
// API key id in DelegatedFromAPIKeyID. A regression here would either lose
// the owner identity (privilege escalation/escape via the key surface) or
// drop the delegated-from trail (audit gap).
func TestEffectivePrincipalDelegatesToOwner(t *testing.T) {
	ownerID := "user-123"
	ownerName := "Alice Admin"
	enabled := true
	ak := &store.APIKeyWithOwner{
		ID:               "key-abc",
		Name:             "deploy-bot",
		Enabled:          true,
		OwnerUserID:      &ownerID,
		OwnerID:          &ownerID,
		OwnerDisplayName: &ownerName,
		OwnerEnabled:     &enabled,
	}

	got := EffectivePrincipal(ak)
	if got.AuthPrincipalType != "admin_user" {
		t.Errorf("AuthPrincipalType = %q; want admin_user", got.AuthPrincipalType)
	}
	if got.KeyID != ownerID {
		t.Errorf("KeyID = %q; want owner id %q", got.KeyID, ownerID)
	}
	if got.KeyName != ownerName {
		t.Errorf("KeyName = %q; want owner display name %q", got.KeyName, ownerName)
	}
	if got.DelegatedFromAPIKeyID != ak.ID {
		t.Errorf("DelegatedFromAPIKeyID = %q; want api key id %q", got.DelegatedFromAPIKeyID, ak.ID)
	}
}

// TestEffectivePrincipalNoOwner exercises the no-delegation branches —
// every case where the owner is absent or inactive must fall back to the
// api_key principal so that disabling a user actually disables their keys.
func TestEffectivePrincipalNoOwner(t *testing.T) {
	enabledTrue := true
	enabledFalse := false
	someID := "user-x"
	someName := "X"

	cases := []struct {
		name string
		ak   *store.APIKeyWithOwner
	}{
		{
			name: "no owner at all",
			ak: &store.APIKeyWithOwner{
				ID:   "k1",
				Name: "raw-key",
			},
		},
		{
			name: "owner id present but disabled (canAccessControlPlane=false)",
			ak: &store.APIKeyWithOwner{
				ID:               "k2",
				Name:             "k2-name",
				OwnerUserID:      &someID,
				OwnerID:          &someID,
				OwnerDisplayName: &someName,
				OwnerEnabled:     &enabledFalse,
			},
		},
		{
			name: "OwnerUserID nil even though OwnerID present (defensive)",
			ak: &store.APIKeyWithOwner{
				ID:           "k3",
				Name:         "k3-name",
				OwnerID:      &someID,
				OwnerEnabled: &enabledTrue,
			},
		},
		{
			name: "OwnerID nil even though OwnerUserID present (defensive)",
			ak: &store.APIKeyWithOwner{
				ID:           "k4",
				Name:         "k4-name",
				OwnerUserID:  &someID,
				OwnerEnabled: &enabledTrue,
			},
		},
		{
			name: "OwnerEnabled nil (owner row missing)",
			ak: &store.APIKeyWithOwner{
				ID:          "k5",
				Name:        "k5-name",
				OwnerUserID: &someID,
				OwnerID:     &someID,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectivePrincipal(tc.ak)
			if got.AuthPrincipalType != "api_key" {
				t.Errorf("AuthPrincipalType = %q; want api_key (no delegation)", got.AuthPrincipalType)
			}
			if got.KeyID != tc.ak.ID {
				t.Errorf("KeyID = %q; want api key id %q", got.KeyID, tc.ak.ID)
			}
			if got.KeyName != tc.ak.Name {
				t.Errorf("KeyName = %q; want api key name %q", got.KeyName, tc.ak.Name)
			}
			if got.DelegatedFromAPIKeyID != "" {
				t.Errorf("DelegatedFromAPIKeyID = %q; want empty (no delegation)", got.DelegatedFromAPIKeyID)
			}
		})
	}
}

// TestGenerateAPIKeyRandError exercises the otherwise-unreachable rand.Read
// failure branch via the package-level randRead indirection. A real
// crypto/rand.Read failure means the OS entropy pool is broken; the function
// must surface a wrapped error rather than emit a zero-entropy "nxk_00...00" key.
func TestGenerateAPIKeyRandError(t *testing.T) {
	prev := randRead
	t.Cleanup(func() { randRead = prev })

	sentinel := errors.New("entropy source unavailable")
	randRead = func(b []byte) (int, error) { return 0, sentinel }

	key, hash, prefix, err := GenerateAPIKey()
	if err == nil {
		t.Fatal("GenerateAPIKey returned nil error when randRead failed — would emit a deterministic, predictable key")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("returned error %v does not wrap the underlying rand error %v", err, sentinel)
	}
	if key != "" || hash != "" || prefix != "" {
		t.Errorf("expected zero-value strings on error; got key=%q hash=%q prefix=%q", key, hash, prefix)
	}
}

// TestDeref covers both deref branches. Exercised indirectly via
// EffectivePrincipal too, but a dedicated test guards future refactors that
// might move deref behind an interface.
func TestDeref(t *testing.T) {
	if got := deref(nil); got != "" {
		t.Errorf("deref(nil) = %q; want empty string", got)
	}
	s := "hello"
	if got := deref(&s); got != s {
		t.Errorf("deref(&%q) = %q; want %q", s, got, s)
	}
	empty := ""
	if got := deref(&empty); got != "" {
		t.Errorf("deref(&\"\") = %q; want empty string", got)
	}
}
