package auth

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

// TestHMACSecretFromEnv exercises the branch where ADMIN_KEY_HMAC_SECRET is set.
// Verifies HMACSecret returns the env value verbatim, not the dev fallback.
func TestHMACSecretFromEnv(t *testing.T) {
	const want = "an-explicit-secret-from-env"
	t.Setenv("ADMIN_KEY_HMAC_SECRET", want)

	got := HMACSecret()
	if got != want {
		t.Errorf("HMACSecret() = %q; want env value %q", got, want)
	}
	if got == hmacDevFallback {
		t.Error("HMACSecret() returned dev fallback when env was set")
	}
}

// TestHMACSecretDevFallback exercises the branch where the env var is unset.
// Verifies the documented dev fallback is returned (so HashAPIKey is deterministic
// in dev across processes that share the same default).
func TestHMACSecretDevFallback(t *testing.T) {
	// t.Setenv with empty string would still set it; we need it unset.
	prev, had := os.LookupEnv("ADMIN_KEY_HMAC_SECRET")
	_ = os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("ADMIN_KEY_HMAC_SECRET", prev)
		} else {
			_ = os.Unsetenv("ADMIN_KEY_HMAC_SECRET")
		}
	})

	if got := HMACSecret(); got != hmacDevFallback {
		t.Errorf("HMACSecret() with unset env = %q; want dev fallback %q", got, hmacDevFallback)
	}
}

// TestHashAPIKeyHonorsHMACSecret verifies that HashAPIKey actually keys the HMAC
// with the env-provided secret — a regression here would mean rotating the
// HMAC secret silently kept old hashes valid.
func TestHashAPIKeyHonorsHMACSecret(t *testing.T) {
	const key = "nxk_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	t.Setenv("ADMIN_KEY_HMAC_SECRET", "secret-A")
	hashA := HashAPIKey(key)

	t.Setenv("ADMIN_KEY_HMAC_SECRET", "secret-B")
	hashB := HashAPIKey(key)

	if hashA == hashB {
		t.Error("HashAPIKey produced the same hash under different HMAC secrets — secret rotation would not invalidate old hashes")
	}
	// Same secret must produce a stable hash.
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "secret-A")
	if HashAPIKey(key) != hashA {
		t.Error("HashAPIKey not deterministic under a stable secret")
	}
}

// TestGenerateAPIKey verifies the shape (prefix + hex), the recorded hash,
// the display prefix matches the documented [:12] slice, and successive calls
// produce different keys (no PRNG re-use).
func TestGenerateAPIKey(t *testing.T) {
	t.Setenv("ADMIN_KEY_HMAC_SECRET", "test-secret-for-generate")

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
