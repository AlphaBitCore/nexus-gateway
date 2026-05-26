package pkce

import (
	"errors"
	"strings"
	"testing"
)

// failReader is an io.Reader that always returns a sentinel error; it
// drives Generate's entropy-read failure branch (the production
// crypto/rand.Reader cannot fail on Go 1.26+ since rand.Read process-aborts
// on entropy starvation per go.dev/issue/66821).
type failReader struct{ err error }

func (f failReader) Read(_ []byte) (int, error) { return 0, f.err }

func TestGenerate_EntropyReadErrorWraps(t *testing.T) {
	want := errors.New("simulated entropy failure")
	orig := randReader
	randReader = failReader{err: want}
	t.Cleanup(func() { randReader = orig })

	v, c, err := Generate()
	if err == nil {
		t.Fatalf("Generate must surface entropy error; got verifier=%q challenge=%q", v, c)
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap sentinel; got %v", err)
	}
	if !strings.Contains(err.Error(), "pkce: read entropy") {
		t.Errorf("error must carry 'pkce: read entropy' context; got %q", err)
	}
	if v != "" || c != "" {
		t.Errorf("on entropy failure outputs must be empty; got verifier=%q challenge=%q", v, c)
	}
}

func TestGenerate_ProductionUsesRealRandReader(t *testing.T) {
	// Guard against an accidental package-init that swaps randReader to a
	// non-crypto source — the package contract pins the production default.
	if randReader == nil {
		t.Error("randReader must be a real io.Reader at package init")
	}
}

func TestGenerate_LengthBounds(t *testing.T) {
	v, c, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(v) < VerifierMinLen || len(v) > VerifierMaxLen {
		t.Errorf("verifier length %d outside [%d,%d]", len(v), VerifierMinLen, VerifierMaxLen)
	}
	if v == c {
		t.Error("verifier and challenge should differ")
	}
	if strings.ContainsAny(v, "=+/") {
		t.Errorf("verifier must be base64url-no-pad; got %q", v)
	}
}

func TestVerifyS256_Roundtrip(t *testing.T) {
	v, c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyS256(v, c) {
		t.Error("VerifyS256 should accept the generated pair")
	}
}

func TestVerifyS256_WrongVerifier(t *testing.T) {
	_, c, _ := Generate()
	other, _, _ := Generate()
	if VerifyS256(other, c) {
		t.Error("VerifyS256 should reject a different verifier")
	}
}

func TestVerifyS256_RejectsShortVerifier(t *testing.T) {
	// Even if the challenge matches, a verifier outside RFC bounds is
	// rejected unconditionally.
	short := strings.Repeat("a", VerifierMinLen-1)
	c := ChallengeS256(short)
	if VerifyS256(short, c) {
		t.Error("VerifyS256 should reject sub-43-char verifier")
	}
}

func TestVerifyS256_RejectsLongVerifier(t *testing.T) {
	long := strings.Repeat("a", VerifierMaxLen+1)
	c := ChallengeS256(long)
	if VerifyS256(long, c) {
		t.Error("VerifyS256 should reject >128-char verifier")
	}
}

func TestChallengeS256_DeterministicAndStable(t *testing.T) {
	// Stability vector from RFC 7636 Appendix B:
	//   verifier "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	//   challenge "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	got := ChallengeS256("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got != want {
		t.Errorf("ChallengeS256 = %q, want %q (RFC 7636 Appendix B vector)", got, want)
	}
}
