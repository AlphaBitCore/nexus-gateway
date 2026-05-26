package tlsbump

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestAttestationFields_SignatureInputCanonical(t *testing.T) {
	f := AttestationFields{
		Version: "v1",
		TS:      1716100000,
		Nonce:   "ab12cd34ef56789012345678901234ab",
		Hash:    "sha256:" + EmptyBodySHA256Hex,
		AgentID: "550e8400-e29b-41d4-a716-446655440000",
	}
	want := "v1\nts=1716100000\nnonce=ab12cd34ef56789012345678901234ab\nhash=sha256:" +
		EmptyBodySHA256Hex + "\nagent_id=550e8400-e29b-41d4-a716-446655440000\n"
	if got := string(f.SignatureInput()); got != want {
		t.Errorf("SignatureInput mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestAttestationFields_FormatHeader(t *testing.T) {
	f := AttestationFields{
		TS:        1,
		Nonce:     "00000000000000000000000000000000",
		Hash:      "sha256:" + EmptyBodySHA256Hex,
		AgentID:   "uuid-1",
		Signature: "AAAA",
	}
	got := f.FormatHeader()
	if !strings.HasPrefix(got, "v1;ts=1;nonce=00000000000000000000000000000000;hash=sha256:") {
		t.Errorf("FormatHeader prefix wrong: %q", got)
	}
	if !strings.HasSuffix(got, ";agent_id=uuid-1;sig=AAAA") {
		t.Errorf("FormatHeader suffix wrong: %q", got)
	}
}

func TestParseAttestationHeader_HappyPath(t *testing.T) {
	hdr := "v1;ts=1716100000;nonce=ab12cd34ef56789012345678901234ab" +
		";hash=sha256:" + EmptyBodySHA256Hex +
		";agent_id=550e8400-e29b-41d4-a716-446655440000" +
		";sig=AAAABBBBCCCCDDDD"
	got, err := ParseAttestationHeader(hdr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.TS != 1716100000 {
		t.Errorf("TS = %d", got.TS)
	}
	if got.Nonce != "ab12cd34ef56789012345678901234ab" {
		t.Errorf("Nonce = %q", got.Nonce)
	}
	if got.AgentID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("AgentID = %q", got.AgentID)
	}
}

func TestParseAttestationHeader_RejectMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"too few fields", "v1;ts=1"},
		{"bad version", "v2;ts=1;nonce=00000000000000000000000000000000;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=x;sig=AAAA"},
		{"missing equals", "v1;ts=1;nonce=00000000000000000000000000000000;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=x;noequals"},
		{"bad ts", "v1;ts=notnumber;nonce=00000000000000000000000000000000;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=x;sig=AAAA"},
		{"short nonce", "v1;ts=1;nonce=abcd;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=x;sig=AAAA"},
		{"non-hex nonce", "v1;ts=1;nonce=zz000000000000000000000000000000;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=x;sig=AAAA"},
		{"bad hash prefix", "v1;ts=1;nonce=00000000000000000000000000000000;hash=md5:" + EmptyBodySHA256Hex + ";agent_id=x;sig=AAAA"},
		{"short hash", "v1;ts=1;nonce=00000000000000000000000000000000;hash=sha256:abcd;agent_id=x;sig=AAAA"},
		{"non-hex hash", "v1;ts=1;nonce=00000000000000000000000000000000;hash=sha256:zz00000000000000000000000000000000000000000000000000000000000000;agent_id=x;sig=AAAA"},
		{"empty agent_id", "v1;ts=1;nonce=00000000000000000000000000000000;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=;sig=AAAA"},
		{"non-b64 sig", "v1;ts=1;nonce=00000000000000000000000000000000;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=x;sig=!!!notb64!!!"},
		{"missing required (no sig)", "v1;ts=1;nonce=00000000000000000000000000000000;hash=sha256:" + EmptyBodySHA256Hex + ";agent_id=x;notreq=zz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseAttestationHeader(c.raw)
			if err == nil {
				t.Errorf("expected error for %q", c.raw)
			}
		})
	}
}

func TestParseAttestationHeader_TolerateUnknownField(t *testing.T) {
	// A v1 reader seeing a future v2-added field must accept the header
	// as long as every required v1 field is present. This is the forward-
	// compat contract from the architecture doc.
	hdr := "v1;ts=1;nonce=00000000000000000000000000000000" +
		";hash=sha256:" + EmptyBodySHA256Hex +
		";agent_id=x;sig=AAAA;future=42"
	if _, err := ParseAttestationHeader(hdr); err != nil {
		t.Fatalf("v2 forward-compat broken: %v", err)
	}
}

func TestHashHelpers(t *testing.T) {
	if HashEmptyBody() != "sha256:"+EmptyBodySHA256Hex {
		t.Errorf("HashEmptyBody = %q", HashEmptyBody())
	}
	got := HashBody([]byte("hello"))
	want := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("HashBody(\"hello\") = %q; want %q", got, want)
	}
}

// TestSignatureRoundTrip wires the helpers end-to-end with a real
// Ed25519 keypair so we know the canonical form is consistent between
// signer and a verifier that parses then re-serialises.
func TestSignatureRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	fields := AttestationFields{
		Version: "v1",
		TS:      1716100000,
		Nonce:   "0123456789abcdef0123456789abcdef",
		Hash:    HashEmptyBody(),
		AgentID: "550e8400-e29b-41d4-a716-446655440000",
	}
	sig := ed25519.Sign(priv, fields.SignatureInput())
	fields.Signature = base64.RawURLEncoding.EncodeToString(sig)

	header := fields.FormatHeader()
	parsed, err := ParseAttestationHeader(header)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(parsed.Signature)
	if err != nil {
		t.Fatalf("sig decode: %v", err)
	}
	if !ed25519.Verify(pub, parsed.SignatureInput(), rawSig) {
		t.Fatal("Ed25519 verify failed — signer/verifier canonical form drifted")
	}
}

// TestEmptyBodyHashMatchesSpec pins the documented EmptyBodySHA256Hex
// constant so a refactor can't silently change the wire commitment.
func TestEmptyBodyHashMatchesSpec(t *testing.T) {
	got := HashBody([]byte{})
	want := HashEmptyBody()
	if got != want {
		t.Errorf("EmptyBodySHA256Hex drifted: HashBody(\"\") = %q; HashEmptyBody = %q", got, want)
	}
	// Also assert the hex encoding is exactly 64 chars (sanity).
	if h := want[len("sha256:"):]; len(h) != 64 {
		t.Errorf("hash hex length = %d; want 64", len(h))
	}
	if _, err := hex.DecodeString(want[len("sha256:"):]); err != nil {
		t.Errorf("emptyBody hash not valid hex: %v", err)
	}
}
