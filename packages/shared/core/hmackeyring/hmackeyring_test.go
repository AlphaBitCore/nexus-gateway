package hmackeyring

import (
	"bytes"
	"strings"
	"testing"
)

// TestNew_StarMarkerSelectsCurrent: the "*"-marked entry is the current version
// used to hash new keys, decoupled from textual order (an operator can prepend or
// append rotation versions without silently changing which one signs new keys).
func TestNew_StarMarkerSelectsCurrent(t *testing.T) {
	kr, err := New("v1:secret-one,*v2:secret-two,v3:secret-three")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if kr.CurrentVersion() != "v2" {
		t.Errorf("CurrentVersion = %q, want v2 (the *-marked entry)", kr.CurrentVersion())
	}
	ver, secret := kr.Current()
	if ver != "v2" || !bytes.Equal(secret, []byte("secret-two")) {
		t.Errorf("Current = (%q,%q), want (v2, secret-two)", ver, secret)
	}
}

// TestNew_NoMarkerLastWins: with no "*", the last entry is current (mirrors the
// credential keyring fallback — a single-entry or append-only map needs no marker).
func TestNew_NoMarkerLastWins(t *testing.T) {
	kr, err := New("v1:a,v2:b,v3:c")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if kr.CurrentVersion() != "v3" {
		t.Errorf("CurrentVersion = %q, want v3 (last-wins fallback)", kr.CurrentVersion())
	}
}

// TestAll_CurrentFirstThenRest: All() yields the current version FIRST (the
// steady-state one-hash hit), then the remaining versions in map order — the
// try-all-versions admission sequence.
func TestAll_CurrentFirstThenRest(t *testing.T) {
	kr, err := New("v1:a,*v2:b,v3:c")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := kr.All()
	if len(got) != 3 {
		t.Fatalf("All len = %d, want 3", len(got))
	}
	if got[0].Version != "v2" {
		t.Errorf("All[0].Version = %q, want current v2 first", got[0].Version)
	}
	// The remaining two are v1 and v3 in map order (current v2 skipped).
	if got[1].Version != "v1" || got[2].Version != "v3" {
		t.Errorf("All[1..2] = (%q,%q), want (v1,v3)", got[1].Version, got[2].Version)
	}
	if !bytes.Equal(got[0].Secret, []byte("b")) {
		t.Errorf("All[0].Secret = %q, want b", got[0].Secret)
	}
}

// TestSingle_OneVersionV1: the non-rotating ADMIN_KEY_HMAC_SECRET path builds a
// one-entry keyring at version v1 (matching the schema key_version default) with
// the EXACT secret bytes — so a key issued here hashes identically whether the
// operator later lists it in a map as "v1:<same-secret>".
func TestSingle_OneVersionV1(t *testing.T) {
	kr, err := Single("the-only-secret")
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	if kr.CurrentVersion() != "v1" || kr.Len() != 1 {
		t.Errorf("Single keyring = (current %q, len %d), want (v1, 1)", kr.CurrentVersion(), kr.Len())
	}
	ver, secret := kr.Current()
	if ver != "v1" || !bytes.Equal(secret, []byte("the-only-secret")) {
		t.Errorf("Current = (%q,%q), want (v1, the-only-secret)", ver, secret)
	}
	// Single("s") and New("v1:s") must agree byte-for-byte on the v1 secret, so the
	// two boot paths produce the same hash for the same key.
	mapped, err := New("v1:the-only-secret")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, mappedSecret := mapped.Current()
	if !bytes.Equal(secret, mappedSecret) {
		t.Errorf("Single vs map v1 secret differ: %q vs %q", secret, mappedSecret)
	}
}

// TestNew_SecretMayContainColon: the version:secret split is on the FIRST colon,
// so a secret containing colons (e.g. base64 padding is colon-free, but a
// passphrase might use them) round-trips intact.
func TestNew_SecretMayContainColon(t *testing.T) {
	kr, err := New("*v1:a:b:c")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, secret := kr.Current()
	if !bytes.Equal(secret, []byte("a:b:c")) {
		t.Errorf("secret = %q, want a:b:c (split on first colon only)", secret)
	}
}

// TestNew_FailClosed covers every malformed-map rejection: a malformed keyring
// must abort boot, never silently admit under a partial/ambiguous set.
func TestNew_FailClosed(t *testing.T) {
	cases := []struct {
		name   string
		keyMap string
	}{
		{"empty map", ""},
		{"only whitespace/commas", " , , "},
		{"missing colon", "v1abc"},
		{"empty version id", ":secret"},
		{"empty version id after star", "*:secret"},
		{"empty secret", "v1:"},
		{"duplicate version", "v1:a,v1:b"},
		{"two current markers", "*v1:a,*v2:b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.keyMap); err == nil {
				t.Fatalf("New(%q) = nil error, want fail-closed rejection", tc.keyMap)
			}
		})
	}
}

// TestSingle_EmptyRejected: the boot gate requires a non-empty HMAC secret.
func TestSingle_EmptyRejected(t *testing.T) {
	if _, err := Single(""); err == nil {
		t.Fatal("Single(\"\") = nil error, want rejection")
	}
}

// TestNew_MalformedEntryErrorsNeverEchoSecret: parse errors land in boot logs,
// so the error string for a malformed entry must never contain the entry's
// secret material — neither for a bare pasted secret (no colon) nor for an
// entry whose version id is missing (":secret").
func TestNew_MalformedEntryErrorsNeverEchoSecret(t *testing.T) {
	const leaked = "SUPER-SECRET-HMAC-MATERIAL"
	cases := []struct {
		name   string
		keyMap string
	}{
		{"bare secret without colon", leaked},
		{"empty version id", ":" + leaked},
		{"empty version id after star", "*:" + leaked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.keyMap)
			if err == nil {
				t.Fatalf("New(%q) = nil error, want fail-closed rejection", tc.name)
			}
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("error %q echoes the secret material — boot logs would leak it", err.Error())
			}
		})
	}
}

// TestSingle_TrimsWhitespaceToMatchMapEntry: a file-sourced env var often
// carries a trailing newline. Single must trim it exactly like New trims a map
// entry's secret, so migrating the same live value to "*v1:<secret>" hashes
// identically (otherwise every existing key silently 401s at the rotation
// runbook's migration step).
func TestSingle_TrimsWhitespaceToMatchMapEntry(t *testing.T) {
	padded := "  hmac-secret-value\n"

	single, err := Single(padded)
	if err != nil {
		t.Fatalf("Single: %v", err)
	}
	mapped, err := New("*v1:" + padded)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sv, ss := single.Current()
	mv, ms := mapped.Current()
	if sv != mv {
		t.Fatalf("current version: Single=%q New=%q, want identical", sv, mv)
	}
	if !bytes.Equal(ss, ms) {
		t.Fatalf("current secret: Single=%q New=%q, want byte-identical after trim", ss, ms)
	}
}

// TestSingle_AllWhitespaceRejected: trimming must not let an effectively-empty
// secret through the boot gate.
func TestSingle_AllWhitespaceRejected(t *testing.T) {
	if _, err := Single(" \n\t "); err == nil {
		t.Fatal("Single(all-whitespace) = nil error, want rejection")
	}
}

// TestVersions_IdsOnlyInMapOrder: the boot-visibility listing exposes version
// ids in map order and never secret bytes (it feeds operator logs).
func TestVersions_IdsOnlyInMapOrder(t *testing.T) {
	kr, err := New("v1:s1,*v2:s2,v3:s3")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := kr.Versions()
	want := []string{"v1", "v2", "v3"}
	if len(got) != len(want) {
		t.Fatalf("Versions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Versions() = %v, want %v", got, want)
		}
	}
	for _, v := range got {
		if strings.Contains(v, "s1") || strings.Contains(v, "s2") || strings.Contains(v, "s3") {
			t.Fatalf("Versions() leaked secret material: %v", got)
		}
	}
}
