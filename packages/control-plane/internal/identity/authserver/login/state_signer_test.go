package login

import (
	"strings"
	"testing"
)

// TestStateSigner_RoundTrip — a cookie produced by sign verifies back to the
// exact state it was minted for. This is the core binding: the value the
// browser carries on the callback resolves to the authctx startOIDC stamped.
func TestStateSigner_RoundTrip(t *testing.T) {
	s := newStateSigner([]byte("0123456789abcdef0123456789abcdef"))
	const state = "authctx-256bit-opaque-token"
	cookie := s.sign(state)
	if !strings.HasSuffix(cookie, "."+state) {
		t.Fatalf("cookie %q does not carry plaintext state suffix", cookie)
	}
	got, err := s.verify(cookie)
	if err != nil {
		t.Fatalf("verify legit cookie: %v", err)
	}
	if got != state {
		t.Fatalf("verify returned %q, want %q", got, state)
	}
}

// TestStateSigner_TamperedSignatureRejected — flipping a byte of the MAC must
// fail verification: an attacker who guesses the authctx but cannot forge the
// MAC (no key) is rejected. This is the property that defeats login-CSRF.
func TestStateSigner_TamperedSignatureRejected(t *testing.T) {
	s := newStateSigner([]byte("0123456789abcdef0123456789abcdef"))
	cookie := s.sign("authctx-x")
	dot := strings.IndexByte(cookie, '.')
	sig := []byte(cookie[:dot])
	// Flip the first hex nibble to a guaranteed-different value.
	if sig[0] == 'a' {
		sig[0] = 'b'
	} else {
		sig[0] = 'a'
	}
	tampered := string(sig) + cookie[dot:]
	if _, err := s.verify(tampered); err == nil {
		t.Fatal("verify accepted a tampered signature")
	}
}

// TestStateSigner_TamperedStateRejected — changing the embedded state while
// keeping the original MAC must fail: an attacker cannot swap in their own
// authctx and reuse a captured MAC.
func TestStateSigner_TamperedStateRejected(t *testing.T) {
	s := newStateSigner([]byte("0123456789abcdef0123456789abcdef"))
	cookie := s.sign("authctx-victim")
	dot := strings.IndexByte(cookie, '.')
	swapped := cookie[:dot] + "." + "authctx-attacker"
	if _, err := s.verify(swapped); err == nil {
		t.Fatal("verify accepted a swapped state with the original MAC")
	}
}

// TestStateSigner_MalformedRejected — a cookie with no '.' separator is
// rejected rather than panicking or returning a partial state.
func TestStateSigner_MalformedRejected(t *testing.T) {
	s := newStateSigner([]byte("0123456789abcdef0123456789abcdef"))
	for _, bad := range []string{"", "no-dot-here", "deadbeef"} {
		if got, err := s.verify(bad); err == nil {
			t.Fatalf("verify(%q) = (%q, nil), want error", bad, got)
		}
	}
}

// TestStateSigner_WrongKeyRejected — a cookie signed under one key must not
// verify under a different key. This is what makes a per-process key safe:
// after a restart the new signer rejects the old cookie.
func TestStateSigner_WrongKeyRejected(t *testing.T) {
	a := newStateSigner([]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	b := newStateSigner([]byte("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"))
	cookie := a.sign("authctx-cross")
	if _, err := b.verify(cookie); err == nil {
		t.Fatal("verify accepted a cookie signed under a different key")
	}
}

// TestNewRandomStateSigner_Independent — two random signers produce different
// MACs for the same state, confirming each call draws a fresh key, and each
// signer round-trips its own cookie.
func TestNewRandomStateSigner_Independent(t *testing.T) {
	s1, err := newRandomStateSigner()
	if err != nil {
		t.Fatalf("newRandomStateSigner: %v", err)
	}
	s2, err := newRandomStateSigner()
	if err != nil {
		t.Fatalf("newRandomStateSigner: %v", err)
	}
	const state = "authctx-rand"
	if s1.sign(state) == s2.sign(state) {
		t.Fatal("two random signers produced identical cookies (key not random)")
	}
	got, err := s1.verify(s1.sign(state))
	if err != nil || got != state {
		t.Fatalf("self round-trip failed: got %q err %v", got, err)
	}
	// Cross-verify must fail.
	if _, err := s2.verify(s1.sign(state)); err == nil {
		t.Fatal("s2 accepted s1's cookie")
	}
}
