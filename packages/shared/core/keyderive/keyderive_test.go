package keyderive

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The whole point of this package is a STABLE, cross-service contract: the CP
// seal side and the ai-gw open side must derive identical sub-keys and build
// identical AAD. These tests pin that contract — including golden vectors, so a
// silent change to the derivation (which would brick every cross-service
// decrypt) fails the build instead of production.

func TestDeriveKey32_Deterministic(t *testing.T) {
	master := []byte("01234567890123456789012345678901") // 32 bytes
	a, err := DeriveKey32(master, ClassProviderCredential)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	b, err := DeriveKey32(master, ClassProviderCredential)
	if err != nil {
		t.Fatalf("derive 2: %v", err)
	}
	if a != b {
		t.Fatal("same (master,class) must derive identical keys — cross-service agreement depends on it")
	}
}

func TestDeriveKey32_ClassSeparation(t *testing.T) {
	master := []byte("01234567890123456789012345678901")
	seen := map[string][32]byte{}
	for _, class := range []string{
		ClassProviderCredential, ClassAlertChannelSecret,
		ClassAPIKeyAdmin, ClassAPIKeyVirtualKey,
	} {
		k, err := DeriveKey32(master, class)
		if err != nil {
			t.Fatalf("derive %s: %v", class, err)
		}
		for prevClass, prevK := range seen {
			if k == prevK {
				t.Fatalf("class %q and %q derived the SAME key — domain separation is broken", class, prevClass)
			}
		}
		seen[class] = k
	}
}

func TestDeriveKey32_MasterSeparation(t *testing.T) {
	// A different master must yield a different sub-key for the same class.
	k1, _ := DeriveKey32([]byte("01234567890123456789012345678901"), ClassProviderCredential)
	k2, _ := DeriveKey32([]byte("abcdefghabcdefghabcdefghabcdefgh"), ClassProviderCredential)
	if k1 == k2 {
		t.Fatal("different masters must derive different keys")
	}
}

func TestDeriveKey32_Errors(t *testing.T) {
	if _, err := DeriveKey32(nil, ClassProviderCredential); err == nil {
		t.Error("empty master must be rejected (fail-closed)")
	}
	if _, err := DeriveKey32([]byte("x"), ""); err == nil {
		t.Error("empty class must be rejected")
	}
}

// TestDeriveKey32_GoldenVector pins the exact derived bytes for a fixed master +
// class. If HKDF params (hash, salt handling, info string) ever change, this
// fails — which is the signal that every previously-sealed ciphertext in the
// fleet just became undecryptable. Treat a change here as a scheme version bump.
func TestDeriveKey32_GoldenVector(t *testing.T) {
	master := make([]byte, 32) // all-zero master, deterministic vector
	got, err := DeriveKey32(master, ClassProviderCredential)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// Golden value computed by this same HKDF-SHA256(info="nexus/cred/provider-api-key/v1",
	// salt=∅) over a 32-byte zero master. Pinned to detect accidental scheme drift.
	const want = "b91e69b8acf4040d45f026936f32194367b8a9a32ec754eac3565a0779dc3667"
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("HKDF derivation drifted — scheme change would brick existing ciphertext.\n got=%s\nwant=%s\n(if this change is intentional, bump the class /vN version and update this vector)",
			hex.EncodeToString(got[:]), want)
	}
}

func TestDeriveSubkey_MatchesStrictForNonEmpty(t *testing.T) {
	// For a non-empty master, DeriveSubkey must equal DeriveKey32 (same HKDF),
	// so the HMAC path and any strict caller agree.
	master := []byte("01234567890123456789012345678901")
	strict, err := DeriveKey32(master, ClassAPIKeyVirtualKey)
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	if DeriveSubkey(master, ClassAPIKeyVirtualKey) != strict {
		t.Fatal("DeriveSubkey must match DeriveKey32 for a non-empty master")
	}
}

func TestDeriveSubkey_TotalAndClassSeparated(t *testing.T) {
	// Must NOT panic on an empty master (boot gate, not this function, enforces
	// non-empty), and admin vs vk classes must differ.
	admin := DeriveSubkey(nil, ClassAPIKeyAdmin)
	vk := DeriveSubkey(nil, ClassAPIKeyVirtualKey)
	if admin == vk {
		t.Fatal("admin and vk sub-keys must differ even for an empty master (domain separation)")
	}
	// And for a real secret, admin != vk.
	m := []byte("super-secret-hmac-value")
	if DeriveSubkey(m, ClassAPIKeyAdmin) == DeriveSubkey(m, ClassAPIKeyVirtualKey) {
		t.Fatal("SEC-W2-01: admin-API-key and virtual-key HMAC sub-keys must be distinct")
	}
}

func TestProviderCredentialAAD_Exact(t *testing.T) {
	got := ProviderCredentialAAD("cred_abc", "prov_xyz")
	want := []byte("nexus/cred/v1|cred:cred_abc|provider:prov_xyz")
	if !bytes.Equal(got, want) {
		t.Fatalf("AAD bytes drifted (cross-service [MUST MATCH]).\n got=%q\nwant=%q", got, want)
	}
}

func TestProviderCredentialAAD_DistinctPerIdentity(t *testing.T) {
	// The swap-defeating property: different credential ids → different AAD, so a
	// ciphertext sealed for cred A fails to open under cred B's AAD.
	a := ProviderCredentialAAD("cred_A", "prov")
	b := ProviderCredentialAAD("cred_B", "prov")
	if bytes.Equal(a, b) {
		t.Fatal("different credential ids must produce different AAD — this is what blocks the swap")
	}
	// Same for provider mismatch.
	c := ProviderCredentialAAD("cred_A", "prov2")
	if bytes.Equal(a, c) {
		t.Fatal("different provider ids must produce different AAD")
	}
}

func TestAlertChannelAAD_Exact(t *testing.T) {
	got := AlertChannelAAD("chan_1")
	want := []byte("nexus/alert/v1|channel:chan_1")
	if !bytes.Equal(got, want) {
		t.Fatalf("alert AAD drifted.\n got=%q\nwant=%q", got, want)
	}
}
