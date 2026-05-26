package token_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

func TestSigner_SignAndVerify(t *testing.T) {
	ks, _ := token.OpenKeystore(t.TempDir())
	_, _ = ks.Generate()
	s := token.NewSigner(ks)

	claims := jwt.MapClaims{
		"iss": "https://auth.nexus.ai",
		"sub": "usr_1",
		"aud": []string{"hub"},
		"exp": time.Now().Add(1 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
		"jti": "tok_1",
	}
	signed, err := s.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := jwt.Parse(signed, func(tok *jwt.Token) (any, error) {
		kid := tok.Header["kid"].(string)
		key, ok := ks.ByKID(kid)
		if !ok {
			t.Fatal("unknown kid")
		}
		return &key.Priv.PublicKey, nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("verify failed: %v", err)
	}
}

func TestSigner_Sign_NoKey_Errors(t *testing.T) {
	ks, _ := token.OpenKeystore(t.TempDir())
	s := token.NewSigner(ks)
	if _, err := s.Sign(jwt.MapClaims{}); err == nil {
		t.Fatal("expected error when no active kid")
	}
}
