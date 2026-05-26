package token_test

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// TestIssueAccess_SignErrorPropagates covers the rare "Signer.Sign fails"
// branch in IssueAccess. The keystore is empty so the signer can't resolve
// an active kid; the error must surface unchanged with empty token + jti.
// Critical: a silently-empty jti would otherwise be stored alongside an
// empty access token, poisoning correlation with the refresh row.
func TestIssueAccess_SignErrorPropagates(t *testing.T) {
	// Empty keystore — no key for Sign to use.
	ks, err := token.OpenKeystore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenKeystore: %v", err)
	}
	signer := token.NewSigner(ks)

	tok, jti, err := token.IssueAccess(signer, token.AccessInput{
		Issuer:   "iss",
		Subject:  "sub",
		Audience: []string{"aud"},
		ClientID: "cid",
		TTL:      time.Hour,
	})
	if err == nil {
		t.Fatal("expected Sign error to surface")
	}
	if tok != "" || jti != "" {
		t.Errorf("on error want empty token+jti; got tok=%q jti=%q", tok, jti)
	}
}
