package keymap

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keyderive"
)

// hexKey returns a random 64-hex-char (32-byte) master key string.
func hexKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// TestParse_StripsStarAndSelectsCurrent is the F-0390 golden: the leading "*"
// marks the current id AND is stripped from the stored id, and a no-marker map
// falls back to last-entry-wins. These two are the exact rules every consumer
// (CP MultiVault mint side, ai-gw MultiDecryptor open side, hmackeyring) depends
// on; a drift here is what produced the cross-service "unknown key ID" outage.
func TestParse_StripsStarAndSelectsCurrent(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		wantEntries  map[string]string
		wantOrder    []string
		wantCurrent  string
		wantExplicit bool
	}{
		{
			name:         "star marks and is stripped",
			env:          "v1:AA,*v2:BB",
			wantEntries:  map[string]string{"v1": "AA", "v2": "BB"},
			wantOrder:    []string{"v1", "v2"},
			wantCurrent:  "v2", // stamped id is "v2", NOT "*v2"
			wantExplicit: true,
		},
		{
			name:         "star on first entry wins regardless of position",
			env:          "*v1:AA,v2:BB,v3:CC",
			wantEntries:  map[string]string{"v1": "AA", "v2": "BB", "v3": "CC"},
			wantOrder:    []string{"v1", "v2", "v3"},
			wantCurrent:  "v1",
			wantExplicit: true,
		},
		{
			name:         "no marker falls back to last-entry-wins",
			env:          "v1:AA,v2:BB",
			wantEntries:  map[string]string{"v1": "AA", "v2": "BB"},
			wantOrder:    []string{"v1", "v2"},
			wantCurrent:  "v2",
			wantExplicit: false,
		},
		{
			name:         "single no-star entry is current",
			env:          "v1:AA",
			wantEntries:  map[string]string{"v1": "AA"},
			wantOrder:    []string{"v1"},
			wantCurrent:  "v1",
			wantExplicit: false,
		},
		{
			name:         "value may contain colons (split on first only)",
			env:          "v1:base64:secret==",
			wantEntries:  map[string]string{"v1": "base64:secret=="},
			wantOrder:    []string{"v1"},
			wantCurrent:  "v1",
			wantExplicit: false,
		},
		{
			name:         "blank entries and surrounding whitespace tolerated",
			env:          "  , v1 : AA , ,*v2 : BB ,",
			wantEntries:  map[string]string{"v1": "AA", "v2": "BB"},
			wantOrder:    []string{"v1", "v2"},
			wantCurrent:  "v2",
			wantExplicit: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries, current, order, explicit, err := Parse(tc.env, nil)
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tc.env, err)
			}
			if !reflect.DeepEqual(entries, tc.wantEntries) {
				t.Errorf("entries: got %v, want %v", entries, tc.wantEntries)
			}
			if !reflect.DeepEqual(order, tc.wantOrder) {
				t.Errorf("order: got %v, want %v", order, tc.wantOrder)
			}
			if current != tc.wantCurrent {
				t.Errorf("currentID: got %q, want %q", current, tc.wantCurrent)
			}
			if explicit != tc.wantExplicit {
				t.Errorf("currentExplicit: got %v, want %v", explicit, tc.wantExplicit)
			}
			// The stamped/stored current id must never retain the "*".
			if strings.Contains(current, "*") {
				t.Fatalf("F-0390 regression: current id %q still carries '*'", current)
			}
		})
	}
}

// TestParse_FailClosed pins every malformed-map rejection: a malformed keyring
// must abort, never silently admit under a partial / ambiguous set.
func TestParse_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"empty map", ""},
		{"only blanks", " , , "},
		{"missing colon", "v1abc"},
		{"empty id", ":secret"},
		{"empty id after star", "*:secret"},
		{"duplicate id", "v1:a,v1:b"},
		{"duplicate id one starred", "*v1:a,v1:b"},
		{"two current markers", "*v1:a,*v2:b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, _, err := Parse(tc.env, nil); err == nil {
				t.Fatalf("Parse(%q) = nil error, want fail-closed rejection", tc.env)
			}
		})
	}
}

// TestParse_ValidatorRejection confirms the caller's value-shape validator is
// invoked per entry and its error aborts the parse with id context.
func TestParse_ValidatorRejection(t *testing.T) {
	wantBad := "short"
	_, _, _, _, err := Parse("v1:ok,v2:short", func(_, value string) error {
		if value == "short" {
			return errShort
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected validator rejection")
	}
	if !strings.Contains(err.Error(), "v2") || !strings.Contains(err.Error(), wantBad) {
		t.Fatalf("error should carry id+value context, got: %v", err)
	}
}

var errShort = errTestShort{}

type errTestShort struct{}

func (errTestShort) Error() string { return "value too short" }

// TestParse_CredentialKeyMapMustMatch is the cross-side [MUST MATCH] regression
// for F-0364/F-0390 expressed at the shared-parser level: the SAME
// CREDENTIAL_KEY_MAP="v1:<hex>,*v2:<hex>" must yield, on BOTH the Control Plane
// mint side and the AI Gateway open side, the SAME stamped/stored id "v2" with
// the "*" stripped — and a ciphertext sealed under the "v2" key (CP encrypt with
// current) must open under the "v2"-keyed entry (gateway decrypt). Both real
// constructors (crypto.NewMultiVault / creddecrypt.NewMultiDecryptor) now route
// through this exact Parse, and both derive the AEAD key via the identical
// keyderive.ClassProviderCredential HKDF; this test reconstructs that shared
// derivation so the round-trip provably exercises the fixed "*"-strip without a
// cross-module dependency edge. Before the fix the gateway keyed the same entry
// under "*v2", so this lookup by "v2" missed → "unknown key ID" on every
// credential decrypt.
func TestParse_CredentialKeyMapMustMatch(t *testing.T) {
	k1, k2 := hexKey(t), hexKey(t)
	env := "v1:" + k1 + ",*v2:" + k2

	// CP MINT SIDE: parse + select current (the "*"-marked id).
	cpEntries, cpCurrent, _, cpExplicit, err := Parse(env, nil)
	if err != nil {
		t.Fatalf("CP-side parse: %v", err)
	}
	if cpCurrent != "v2" {
		t.Fatalf("CP stamps current id %q, want v2 (the '*'-marked, stripped id)", cpCurrent)
	}
	if !cpExplicit {
		t.Fatal("CP current should be explicit ('*'-marked)")
	}

	// GATEWAY OPEN SIDE: parse the SAME env independently.
	gwEntries, _, _, _, err := Parse(env, nil)
	if err != nil {
		t.Fatalf("gateway-side parse: %v", err)
	}
	// [MUST MATCH]: the gateway must hold a key under exactly the id the CP
	// stamps. This is the lookup that 404'd before the "*"-strip fix.
	gwKeyHex, ok := gwEntries[cpCurrent]
	if !ok {
		t.Fatalf("F-0390 regression: gateway has no key under CP-stamped id %q; gateway ids = %v", cpCurrent, keysOf(gwEntries))
	}
	if cpEntries[cpCurrent] != gwKeyHex {
		t.Fatal("CP and gateway resolved different key material for the current id")
	}

	// Round-trip: seal under the CP-current key, open under the gateway entry
	// for the same stamped id, using the SAME ClassProviderCredential HKDF both
	// real constructors use.
	plaintext := "sk-upstream-provider-key"
	ct, iv, tag := sealUnderMaster(t, cpEntries[cpCurrent], plaintext)
	got := openUnderMaster(t, gwKeyHex, ct, iv, tag)
	if got != plaintext {
		t.Fatalf("round-trip under stamped id %q: got %q, want %q", cpCurrent, got, plaintext)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sealUnderMaster mirrors crypto.Vault.Encrypt: HKDF the provider-credential
// sub-key from the 64-hex master, then AES-256-GCM seal.
func sealUnderMaster(t *testing.T, masterHex, plaintext string) (ctHex, ivHex, tagHex string) {
	t.Helper()
	gcm := gcmFor(t, masterHex)
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	ct := sealed[:len(sealed)-16]
	tag := sealed[len(sealed)-16:]
	return hex.EncodeToString(ct), hex.EncodeToString(iv), hex.EncodeToString(tag)
}

// openUnderMaster mirrors creddecrypt.Decryptor.Decrypt: same HKDF + GCM open.
func openUnderMaster(t *testing.T, masterHex, ctHex, ivHex, tagHex string) string {
	t.Helper()
	gcm := gcmFor(t, masterHex)
	ct, _ := hex.DecodeString(ctHex)
	iv, _ := hex.DecodeString(ivHex)
	tag, _ := hex.DecodeString(tagHex)
	sealed := append(append([]byte{}, ct...), tag...)
	pt, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		t.Fatalf("GCM open: %v", err)
	}
	return string(pt)
}

func gcmFor(t *testing.T, masterHex string) cipher.AEAD {
	t.Helper()
	master, err := hex.DecodeString(masterHex)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := keyderive.DeriveKey32(master, keyderive.ClassProviderCredential)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(sub[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return gcm
}
