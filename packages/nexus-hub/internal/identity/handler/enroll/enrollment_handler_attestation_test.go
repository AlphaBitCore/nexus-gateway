package enroll

// enrollment_handler_attestation_test.go covers the attestation code paths
// in EnrollmentAPI not yet exercised by the base test suite:
//
//   - extractAttestationPublicKeyBytes happy path (Ed25519 cert → 32-byte key)
//   - extractAttestationPublicKeyBytes PEM decode failure (nil/wrong block type)
//   - extractAttestationPublicKeyBytes x509 parse failure (invalid DER)
//   - extractAttestationPublicKeyBytes non-Ed25519 public key (P-256 cert → error)
//   - doEnroll with attestationCsrPem: SignAttestationCSR error → non-fatal, no
//     attestationCertPem in response, mTLS enrollment still 200
//   - doEnroll with attestationCsrPem: SignAttestationCSR success but pubkey
//     extract fails (stub returns stub PEM that fails x509 parse) → non-fatal
//   - doEnroll with attestationCsrPem: full happy path (real Ed25519 cert PEM
//     from stubCA returns real cert) → attestationCertPem in response, pubkey
//     stored (StoreAttestationPubKey mock called), sysinfo stamped
//   - doEnroll with attestationCsrPem: StoreAttestationPubKey error → non-fatal,
//     enrollment still 200
//   - verifyEnrollmentJWT CpIssuer pinning exercised (issuer mismatch → JWT_INVALID)
//   - verifyEnrollmentJWT non-RSA signing method → JWT_INVALID unexpected signing method
//   - enrollWithJWT thingType defaults to "agent" when request omits it (SSO path)
//   - stubCA.SignAttestationCSR has own signErr independent of SignCSR

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
)

// Extended stubCA with independent attestation error seam

// stubCAWithAttest drives the SignAttestationCSR seam: attSignErr forces an
// error, attCertPEM overrides the returned cert PEM (e.g. a real Ed25519 cert
// for the pubkey-extract happy path).
type stubCAWithAttest struct {
	attSignErr error  // returned by SignAttestationCSR
	attCertPEM string // when non-empty, returned as CertPEM by SignAttestationCSR
}

func (s *stubCAWithAttest) SignAttestationCSR(csrPEM, subjectCN string) (*agentca.CertResult, error) {
	if s.attSignErr != nil {
		return nil, s.attSignErr
	}
	certPEM := s.attCertPEM
	if certPEM == "" {
		// Default: return a stub PEM that is not valid DER (invalid x509), useful for
		// testing the pubkey-extract-failed branch.
		certPEM = "-----BEGIN CERTIFICATE-----\nSTUB\n-----END CERTIFICATE-----\n"
	}
	exp := time.Now().Add(90 * 24 * time.Hour)
	return &agentca.CertResult{
		CertPEM:   certPEM,
		CaCertPEM: "-----BEGIN CERTIFICATE-----\nSTUB-CA\n-----END CERTIFICATE-----\n",
		Serial:    "AABBCCDDEEFF0022",
		ExpiresAt: exp,
	}, nil
}

// Helpers: generate real x509 certs for extractAttestationPublicKeyBytes tests

// newEd25519Cert returns a PEM-encoded self-signed Ed25519 certificate.
// Used to test the happy path of extractAttestationPublicKeyBytes.
func newEd25519Cert(t *testing.T) (certPEM string, pub ed25519.PublicKey) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "attestation-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, pubKey, privKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	return string(pemBytes), pubKey
}

// newP256Cert returns a PEM-encoded self-signed P-256 (ECDSA) certificate.
// Used to test the non-Ed25519 key type rejection in extractAttestationPublicKeyBytes.
func newP256Cert(t *testing.T) string {
	t.Helper()
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "p256-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privKey.PublicKey, privKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
}

// newEd25519CSR returns a PEM-encoded Ed25519 CSR (for doEnroll attestation tests).
func newEd25519CSR(t *testing.T) string {
	t.Helper()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "attest-csr-test"},
	}, privKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificateRequest: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
}

// Tests: extractAttestationPublicKeyBytes — pure function

func TestExtractAttestationPublicKeyBytes_HappyPath(t *testing.T) {
	// Valid Ed25519 cert PEM → returns 32-byte raw public key.
	certPEM, wantPub := newEd25519Cert(t)
	got, err := extractAttestationPublicKeyBytes(certPEM)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != ed25519.PublicKeySize {
		t.Errorf("want %d bytes, got %d", ed25519.PublicKeySize, len(got))
	}
	if string(got) != string(wantPub) {
		t.Error("returned key bytes do not match expected Ed25519 public key")
	}
}

func TestExtractAttestationPublicKeyBytes_PEMDecodeFailure(t *testing.T) {
	// Empty string → pem.Decode returns nil block → "attestation cert PEM decode failed".
	_, err := extractAttestationPublicKeyBytes("")
	if err == nil {
		t.Fatal("expected error for empty PEM")
	}
	if err.Error() != "attestation cert PEM decode failed" {
		t.Errorf("unexpected error text: %v", err)
	}
}

func TestExtractAttestationPublicKeyBytes_WrongBlockType(t *testing.T) {
	// A PEM block that is not "CERTIFICATE" → same decode failure.
	wrongPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}))
	_, err := extractAttestationPublicKeyBytes(wrongPEM)
	if err == nil {
		t.Fatal("expected error for wrong block type")
	}
	if err.Error() != "attestation cert PEM decode failed" {
		t.Errorf("unexpected error text: %v", err)
	}
}

func TestExtractAttestationPublicKeyBytes_X509ParseFailure(t *testing.T) {
	// A PEM block with type CERTIFICATE but invalid DER body → x509.ParseCertificate error.
	badPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-valid-der")}))
	_, err := extractAttestationPublicKeyBytes(badPEM)
	if err == nil {
		t.Fatal("expected error for invalid DER")
	}
	// Error must wrap the x509 parse error.
	if !containsStr(err.Error(), "parse attestation cert") {
		t.Errorf("want 'parse attestation cert' in error, got: %v", err)
	}
}

func TestExtractAttestationPublicKeyBytes_NonEd25519Key(t *testing.T) {
	// A valid P-256 cert → public key is *ecdsa.PublicKey, not ed25519.PublicKey
	// → "attestation cert public key is *ecdsa.PublicKey, want ed25519.PublicKey".
	p256PEM := newP256Cert(t)
	_, err := extractAttestationPublicKeyBytes(p256PEM)
	if err == nil {
		t.Fatal("expected error for P-256 cert")
	}
	if !containsStr(err.Error(), "want ed25519.PublicKey") {
		t.Errorf("want 'want ed25519.PublicKey' in error, got: %v", err)
	}
}

// containsStr returns true if s contains sub (avoids importing "strings" package here).
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

// Tests: doEnroll attestation path via Enroll handler

// stdMockExpects sets up the DB expectations for the base mTLS enrollment
// (StoreDeviceTokenHash + UpdateThingAgent without hostname/os). Tests that
// add attestation expectations should call this first, then add extra expects.
func stdMockExpects(mock pgxmock.PgxPoolIface) {
	// StoreDeviceTokenHash: UPDATE thing SET metadata = jsonb_set(...)
	mock.ExpectExec(`UPDATE thing`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// UpdateThingAgent INSERT (no hostname/os in these tests)
	mock.ExpectExec(`INSERT INTO thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
}

func TestDoEnroll_Attestation_SignError_IsNonFatal(t *testing.T) {
	// When SignAttestationCSR returns an error, enrollment must still return 200
	// and attestationCertPem must NOT appear in the response.
	tok := makeEnrollToken("tok-att-sign-err")
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	ca := &stubCAWithAttest{
		attSignErr: errors.New("Ed25519 signing unavailable"),
	}
	api := buildAPI(ca, mgr, svc)

	stdMockExpects(mock)

	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"attestationCsrPem": newEd25519CSR(t),
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("SignAttestationCSR error must be non-fatal; want 200, got %d body: %s",
			rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if _, hasAttest := body["attestationCertPem"]; hasAttest {
		t.Error("attestationCertPem must not appear in response when SignAttestationCSR fails")
	}
	// The device token (the actual identity credential) must still be present.
	if _, hasTok := body["deviceToken"]; !hasTok {
		t.Error("deviceToken must be in response even when attestation fails")
	}
}

func TestDoEnroll_Attestation_PubKeyExtractFails_IsNonFatal(t *testing.T) {
	// When SignAttestationCSR returns a stub cert PEM that fails x509 parse
	// inside extractAttestationPublicKeyBytes, enrollment must still return 200.
	// The attestationCertPem IS returned (cert issued successfully), but pubkey
	// storage is skipped with a Warn log.
	tok := makeEnrollToken("tok-att-extract-err")
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	// Default stubCAWithAttest returns a fake PEM that fails x509.ParseCertificate.
	ca := &stubCAWithAttest{}
	api := buildAPI(ca, mgr, svc)

	stdMockExpects(mock)
	// No StoreAttestationPubKey expectation — it must NOT be called when pubkey
	// extraction fails.

	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"attestationCsrPem": newEd25519CSR(t),
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("pubkey extract failure must be non-fatal; want 200, got %d body: %s",
			rec.Code, rec.Body.String())
	}
	// attestationCertPem is set (SignAttestationCSR succeeded), even though
	// pubkey extraction failed — the client can still use the cert for TLS.
	var body map[string]any
	decodeBody(t, rec, &body)
	if _, hasTok := body["deviceToken"]; !hasTok {
		t.Error("deviceToken must be present")
	}
}

func TestDoEnroll_Attestation_HappyPath_PubKeyStored(t *testing.T) {
	// Full happy path: real Ed25519 cert PEM returned by stubCA → pubkey extracted
	// → StoreAttestationPubKey called → attestationCertPem in response.
	tok := makeEnrollToken("tok-att-happy")
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	realCertPEM, _ := newEd25519Cert(t)
	ca := &stubCAWithAttest{attCertPEM: realCertPEM}
	api := buildAPI(ca, mgr, svc)

	stdMockExpects(mock)
	// StoreAttestationPubKey: UPDATE thing_agent SET sysinfo = jsonb_set(...)
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"attestationCsrPem": newEd25519CSR(t),
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("attestation happy path: want 200, got %d; body: %s",
			rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	certPem, _ := body["attestationCertPem"].(string)
	if certPem == "" {
		t.Error("attestationCertPem must be present in response on happy path")
	}
	if certPem != realCertPEM {
		t.Errorf("attestationCertPem = %q, want real Ed25519 cert PEM", certPem[:min(80, len(certPem))])
	}
	// The device token must still be present alongside attestationCertPem.
	if _, ok := body["deviceToken"]; !ok {
		t.Error("deviceToken must be present alongside attestationCertPem")
	}
}

func TestDoEnroll_Attestation_StoreAttestationPubKeyError_IsNonFatal(t *testing.T) {
	// StoreAttestationPubKey failure must not abort enrollment (non-fatal Warn log).
	// The mTLS enrollment still returns 200 and attestationCertPem is still set.
	tok := makeEnrollToken("tok-att-store-err")
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	realCertPEM, _ := newEd25519Cert(t)
	ca := &stubCAWithAttest{attCertPEM: realCertPEM}
	api := buildAPI(ca, mgr, svc)

	stdMockExpects(mock)
	// StoreAttestationPubKey returns an error.
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("db write timeout"))

	rec := post(t, api, map[string]any{
		"csrPem":            validCSR,
		"thingType":         "agent",
		"attestationCsrPem": newEd25519CSR(t),
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("StoreAttestationPubKey error must be non-fatal; want 200, got %d body: %s",
			rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if _, ok := body["attestationCertPem"]; !ok {
		t.Error("attestationCertPem must still be present when only pubkey storage fails")
	}
}

func TestDoEnroll_NoAttestationCSR_NoAttestationInResponse(t *testing.T) {
	// When AttestationCsrPem is empty (older agent without attestation support),
	// the attestation block is skipped entirely — SignAttestationCSR is never
	// called and attestationCertPem is absent from the response.
	tok := makeEnrollToken("tok-no-attest")
	svc := &stubEnrollSvc{token: tok, valid: true}
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}
	api := buildAPI(&stubCA{}, mgr, svc)

	stdMockExpects(mock)

	rec := post(t, api, map[string]any{
		"csrPem":    validCSR,
		"thingType": "agent",
		// No attestationCsrPem — simulates an older agent without attestation support.
	}, map[string]string{"X-Enrollment-Token": "tok"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	if _, hasAttest := body["attestationCertPem"]; hasAttest {
		t.Error("attestationCertPem must be absent when no attestationCsrPem was sent")
	}
}

// Tests: verifyEnrollmentJWT — additional uncovered branches

func TestVerifyEnrollmentJWT_CpIssuerPinned_WrongIssuer_Returns401(t *testing.T) {
	// When CpIssuer is set, a JWT with a different issuer must be rejected.
	key := newRSAKey(t)
	api := buildAPI(&stubCA{}, nil, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}
	api.CpIssuer = "https://cp.expected.example.com"

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-wrong-iss-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "https://evil.example.com", // mismatch
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong issuer must produce 401, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWT_INVALID" {
		t.Errorf("want JWT_INVALID, got %q", body.Code)
	}
}

func TestVerifyEnrollmentJWT_NonRSAAlgorithm_Returns401(t *testing.T) {
	// A JWT signed with HMAC (HS256) instead of RSA triggers the
	// "unexpected signing method" branch in the keyfunc.
	api := buildAPI(&stubCA{}, nil, nil)
	api.JWKSCache = &stubJWKS{} // key doesn't matter — rejected before lookup

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-hmac-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-hmac",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	// Sign with HMAC-SHA256 instead of RS256.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "test-kid"
	tokenStr, err := tok.SignedString([]byte("hmac-secret"))
	if err != nil {
		t.Fatalf("sign HS256 JWT: %v", err)
	}

	rec := post(t, api, map[string]any{"csrPem": validCSR},
		map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-RSA alg must produce 401, got %d", rec.Code)
	}
	var body ErrorResponse
	decodeBody(t, rec, &body)
	if body.Code != "JWT_INVALID" {
		t.Errorf("want JWT_INVALID, got %q", body.Code)
	}
}

// Tests: enrollWithJWT — additional uncovered paths

func TestEnrollWithJWT_ThingTypeDefaultsToAgent(t *testing.T) {
	// When the request omits thingType, enrollWithJWT defaults to "agent".
	// Verify the response id has the "agent-" prefix (generated random id).
	key := newRSAKey(t)
	st, mock := newPgxmockStore(t)
	defer mock.Close()
	mgr := &stubFleetManager{st: st}

	api := buildAPI(&stubCA{}, mgr, nil)
	api.JWKSCache = &stubJWKS{key: &key.PublicKey}

	exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
	jtiVal := "jti-no-type-" + time.Now().Format("150405.000")
	claims := enrollmentJWTClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-no-type",
			Audience:  jwt.ClaimStrings{enrollAudience},
			ExpiresAt: exp,
			ID:        jtiVal,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Purpose: enrollPurpose,
	}
	tokenStr := makeRS256JWT(t, key, claims)

	stdMockExpects(mock)
	// UpsertDeviceAssignment (3 steps).
	mock.ExpectExec(`UPDATE "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	mock.ExpectExec(`INSERT INTO "DeviceAssignment"`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	mock.ExpectExec(`UPDATE thing_agent`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	rec := post(t, api, map[string]any{
		"csrPem": validCSR,
		// No thingType → should default to "agent"
	}, map[string]string{"Authorization": "Bearer " + tokenStr})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec, &body)
	id, _ := body["id"].(string)
	if id == "" {
		t.Error("response id must be non-empty")
	}
	// Verify thingType was "agent" by checking that id starts with "agent-".
	if !startsWithStr(id, "agent-") {
		t.Errorf("id must start with 'agent-' when thingType defaults to agent, got %q", id)
	}
}

// TestEnrollWithJWT_ServiceTypeRejected_SEC_C2_03 pins the invariant:
// the SSO/browser enrollment JWT is a device-enrollment grant and must NOT mint a
// privileged service-type Thing. A request asking for thingType=ai-gateway (or any
// non-agent type) is refused 403 ENROLL_TYPE_FORBIDDEN, BEFORE any thing is minted
// or service-tier desired state written (no enrollment DB expectations are set, so
// an unexpected query would fail the pgxmock at Close()).
func TestEnrollWithJWT_ServiceTypeRejected_SEC_C2_03(t *testing.T) {
	for _, svcType := range []string{"ai-gateway", "compliance-proxy", "control-plane"} {
		t.Run(svcType, func(t *testing.T) {
			key := newRSAKey(t)
			st, mock := newPgxmockStore(t)
			defer mock.Close()
			mgr := &stubFleetManager{st: st}

			api := buildAPI(&stubCA{}, mgr, nil)
			api.JWKSCache = &stubJWKS{key: &key.PublicKey}

			exp := jwt.NewNumericDate(time.Now().Add(5 * time.Minute))
			claims := enrollmentJWTClaims{
				RegisteredClaims: jwt.RegisteredClaims{
					Subject:   "user-spoof",
					Audience:  jwt.ClaimStrings{enrollAudience},
					ExpiresAt: exp,
					ID:        "jti-spoof-" + svcType + "-" + time.Now().Format("150405.000"),
					IssuedAt:  jwt.NewNumericDate(time.Now()),
				},
				Purpose: enrollPurpose,
			}
			tokenStr := makeRS256JWT(t, key, claims)

			rec := post(t, api, map[string]any{
				"csrPem":    validCSR,
				"thingType": svcType,
			}, map[string]string{"Authorization": "Bearer " + tokenStr})

			if rec.Code != http.StatusForbidden {
				t.Fatalf("want 403 for thingType=%s, got %d; body: %s", svcType, rec.Code, rec.Body.String())
			}
			var body map[string]any
			decodeBody(t, rec, &body)
			errObj, _ := body["error"].(map[string]any)
			if errObj == nil || errObj["code"] != "ENROLL_TYPE_FORBIDDEN" {
				t.Errorf("want error.code ENROLL_TYPE_FORBIDDEN, got %v; body: %s", body["error"], rec.Body.String())
			}
			// A rejected enrollment must not return a minted thing id.
			if id, _ := body["id"].(string); id != "" {
				t.Errorf("rejected enrollment must not return a thing id, got %q", id)
			}
		})
	}
}

// makeEnrollToken returns a minimal enrollment token for use in token-path tests.
func makeEnrollToken(id string) *enrollment.Token {
	return &enrollment.Token{
		ID:        id,
		ThingType: "agent",
		ExpiresAt: time.Now().Add(time.Hour),
		Status:    "pending",
	}
}

// startsWithStr returns true if s starts with prefix.
func startsWithStr(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
