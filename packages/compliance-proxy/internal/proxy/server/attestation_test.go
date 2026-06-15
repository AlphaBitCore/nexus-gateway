package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/tlsbump"
)

func newAttestationTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// signWith builds a wire-format X-Nexus-Attestation header value
// signed by the supplied Ed25519 keypair. ts is unix-seconds; pass
// time.Now().Unix() for the happy path, an out-of-window value to
// test the expired branch.
func signWith(t *testing.T, priv ed25519.PrivateKey, agentID string, ts int64, nonce string) string {
	t.Helper()
	if nonce == "" {
		nonce = "0123456789abcdef0123456789abcdef"
	}
	fields := tlsbump.AttestationFields{
		Version: tlsbump.AttestationHeaderVersion,
		TS:      ts,
		Nonce:   nonce,
		Hash:    tlsbump.HashEmptyBody(),
		AgentID: agentID,
	}
	sig := ed25519.Sign(priv, fields.SignatureInput())
	fields.Signature = base64.RawURLEncoding.EncodeToString(sig)
	return fields.FormatHeader()
}

func newVerifierWithLoader(t *testing.T, loader tlsbump.AttestationKeyLoader, window time.Duration) *AttestationVerifier {
	t.Helper()
	keys := tlsbump.NewAttestationKeyCacheWith(loader, newAttestationTestLogger(),
		time.Minute, time.Minute, 100)
	replay := tlsbump.NewAttestationReplayCache()
	return NewAttestationVerifierWith(keys, replay, window, true, newAttestationTestLogger())
}

func TestVerifier_DisabledReturnsDisabled(t *testing.T) {
	v := NewAttestationVerifier(nil, nil, false, newAttestationTestLogger())
	res := v.Verify(context.Background(), "irrelevant")
	if res.Outcome != AttestationOutcomeDisabled {
		t.Errorf("Outcome = %q; want disabled", res.Outcome)
	}
}

func TestVerifier_NilReceiverDisabled(t *testing.T) {
	var v *AttestationVerifier
	if v.Enabled() {
		t.Error("nil verifier must be disabled")
	}
	res := v.Verify(context.Background(), "header")
	if res.Outcome != AttestationOutcomeDisabled {
		t.Errorf("Outcome = %q; want disabled", res.Outcome)
	}
}

func TestVerifier_MissingHeader(t *testing.T) {
	v := newVerifierWithLoader(t, func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
		t.Fatal("loader must not be called on missing header")
		return tlsbump.AttestationKey{}, nil
	}, 5*time.Minute)
	res := v.Verify(context.Background(), "")
	if res.Outcome != AttestationOutcomeMissing {
		t.Errorf("Outcome = %q; want missing", res.Outcome)
	}
}

func TestVerifier_Valid_HappyPath(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const agentID = "550e8400-e29b-41d4-a716-446655440000"
	hdr := signWith(t, priv, agentID, time.Now().Unix(), "")

	v := newVerifierWithLoader(t,
		func(_ context.Context, id string) (tlsbump.AttestationKey, error) {
			if id != agentID {
				t.Errorf("loader called with %q; want %q", id, agentID)
			}
			return tlsbump.AttestationKey{Key: pub}, nil
		},
		5*time.Minute)
	res := v.Verify(context.Background(), hdr)
	if res.Outcome != AttestationOutcomeValid {
		t.Errorf("Outcome = %q; want valid", res.Outcome)
	}
	if res.AgentID != agentID {
		t.Errorf("AgentID = %q; want %q", res.AgentID, agentID)
	}
}

// TestVerifier_CertExpired_RejectsOtherwiseValid is the SEC-M4-01 keystone: a
// header that is in-window, correctly signed, and from a known agent must STILL
// be rejected (outcome=expired → MITM fallback) when the agent's attestation
// cert NotAfter has passed. Without the fix this returned valid → full
// compliance bypass forever.
func TestVerifier_CertExpired_RejectsOtherwiseValid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const agentID = "550e8400-e29b-41d4-a716-446655440000"
	hdr := signWith(t, priv, agentID, time.Now().Unix(), "") // in replay window

	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: pub, CertExpiresAt: time.Now().Add(-1 * time.Hour)}, nil
		},
		5*time.Minute)
	res := v.Verify(context.Background(), hdr)
	if res.Outcome != AttestationOutcomeExpired {
		t.Errorf("Outcome = %q; want expired (cert lapsed)", res.Outcome)
	}
	if res.AgentID != agentID {
		t.Errorf("AgentID must still be carried on expired: %q", res.AgentID)
	}
}

// TestVerifier_CertNotYetExpired_Valid: a key whose cert is still in date passes.
func TestVerifier_CertNotYetExpired_Valid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const agentID = "550e8400-e29b-41d4-a716-446655440000"
	hdr := signWith(t, priv, agentID, time.Now().Unix(), "")
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: pub, CertExpiresAt: time.Now().Add(89 * 24 * time.Hour)}, nil
		},
		5*time.Minute)
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeValid {
		t.Errorf("Outcome = %q; want valid (cert still in date)", got)
	}
}

// TestVerifier_CertExpiryZero_FailOpenValid: a legacy stamp with no expiry on
// record is treated as non-expiring (fail-open), so the key still verifies.
func TestVerifier_CertExpiryZero_FailOpenValid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const agentID = "550e8400-e29b-41d4-a716-446655440000"
	hdr := signWith(t, priv, agentID, time.Now().Unix(), "")
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: pub}, nil // zero CertExpiresAt
		},
		5*time.Minute)
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeValid {
		t.Errorf("Outcome = %q; want valid (zero expiry = fail-open)", got)
	}
}

func TestVerifier_MalformedHeader_InvalidSig(t *testing.T) {
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			t.Fatal("loader must not be called on parse failure")
			return tlsbump.AttestationKey{}, nil
		},
		5*time.Minute)
	res := v.Verify(context.Background(), "garbage")
	if res.Outcome != AttestationOutcomeInvalidSig {
		t.Errorf("Outcome = %q; want invalid_sig", res.Outcome)
	}
}

func TestVerifier_Expired_PastWindow(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	const agentID = "agent-1"
	// 1 hour old
	hdr := signWith(t, priv, agentID, time.Now().Add(-1*time.Hour).Unix(), "")

	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: pub}, nil
		},
		5*time.Minute)
	res := v.Verify(context.Background(), hdr)
	if res.Outcome != AttestationOutcomeExpired {
		t.Errorf("Outcome = %q; want expired", res.Outcome)
	}
	if res.AgentID != agentID {
		t.Errorf("AgentID should still be carried on expired: %q", res.AgentID)
	}
}

func TestVerifier_Expired_FutureSkew(t *testing.T) {
	// Symmetric: a CONNECT with ts +1h in the future is also expired.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	hdr := signWith(t, priv, "agent", time.Now().Add(time.Hour).Unix(), "")

	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: pub}, nil
		},
		5*time.Minute)
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeExpired {
		t.Errorf("Outcome = %q; want expired", got)
	}
}

func TestVerifier_UnknownAgent(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hdr := signWith(t, priv, "stranger", time.Now().Unix(), "")
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{}, tlsbump.ErrUnknownAgent
		},
		5*time.Minute)
	res := v.Verify(context.Background(), hdr)
	if res.Outcome != AttestationOutcomeUnknownAgent {
		t.Errorf("Outcome = %q; want unknown_agent", res.Outcome)
	}
}

func TestVerifier_LoaderError_FailsAsUnknownAgent(t *testing.T) {
	// Generic loader error (e.g. Hub down) — verifier must surface as
	// unknown_agent so the CP MITM-fallback engages instead of leaking
	// a 5xx to the client.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hdr := signWith(t, priv, "a", time.Now().Unix(), "")
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{}, errors.New("hub timeout")
		},
		5*time.Minute)
	res := v.Verify(context.Background(), hdr)
	if res.Outcome != AttestationOutcomeUnknownAgent {
		t.Errorf("Outcome = %q; want unknown_agent", res.Outcome)
	}
	if res.Reason == "" {
		t.Error("Reason should carry loader-error detail")
	}
}

func TestVerifier_WrongKey_InvalidSig(t *testing.T) {
	// Agent A signs but Hub returns Agent B's key — must reject.
	_, agentA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, _, _ := ed25519.GenerateKey(rand.Reader)
	hdr := signWith(t, agentA, "a", time.Now().Unix(), "")
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: pubB}, nil
		},
		5*time.Minute)
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeInvalidSig {
		t.Errorf("Outcome = %q; want invalid_sig", got)
	}
}

func TestVerifier_WrongKeySize_InvalidSig(t *testing.T) {
	// Pathological: loader returns a key of the wrong byte size (e.g.
	// truncated by corrupted base64 upstream). Verifier must reject
	// before invoking ed25519.Verify so the panic-on-wrong-size in
	// stdlib never fires.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hdr := signWith(t, priv, "a", time.Now().Unix(), "")
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: ed25519.PublicKey{0x01, 0x02}}, nil
		},
		5*time.Minute)
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeInvalidSig {
		t.Errorf("Outcome = %q; want invalid_sig", got)
	}
}

func TestVerifier_Replayed_SecondCallRejected(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	hdr := signWith(t, priv, "a", time.Now().Unix(), "deadbeef00000000000000000000beef")
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			return tlsbump.AttestationKey{Key: pub}, nil
		},
		5*time.Minute)
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeValid {
		t.Fatalf("first Verify Outcome = %q; want valid", got)
	}
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeReplayed {
		t.Errorf("second Verify Outcome = %q; want replayed", got)
	}
}

func TestVerifier_ToggleFlipsAtRuntime(t *testing.T) {
	v := NewAttestationVerifier(nil, nil, true, newAttestationTestLogger())
	if !v.Enabled() {
		t.Fatal("verifier should start enabled")
	}
	v.SetEnabled(false)
	if v.Enabled() {
		t.Error("verifier should be disabled after SetEnabled(false)")
	}
	v.SetEnabled(true)
	if !v.Enabled() {
		t.Error("verifier should re-enable after SetEnabled(true)")
	}
}

func TestVerifier_NilSetEnabledSafe(t *testing.T) {
	var v *AttestationVerifier
	v.SetEnabled(true) // must not panic
}

// TestVerifier_BadSignatureBase64 catches the path where the wire-
// format parsing succeeds (parser does its own b64 validation) but a
// theoretical version drift could let a non-b64 sig through. The
// parser already rejects this — this test pins that.
func TestVerifier_BadSignatureBase64_ViaParser(t *testing.T) {
	// Build a header with a deliberately-invalid sig field by hand.
	const hdr = "v1;ts=1716100000;nonce=00000000000000000000000000000000" +
		";hash=sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" +
		";agent_id=a;sig=!!notb64!!"
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	_ = priv
	v := newVerifierWithLoader(t,
		func(_ context.Context, _ string) (tlsbump.AttestationKey, error) {
			t.Fatal("loader must not be called on parse failure")
			return tlsbump.AttestationKey{}, nil
		},
		5*time.Minute)
	if got := v.Verify(context.Background(), hdr).Outcome; got != AttestationOutcomeInvalidSig {
		t.Errorf("Outcome = %q; want invalid_sig", got)
	}
}
