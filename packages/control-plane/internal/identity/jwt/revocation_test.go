package jwtverifier_test

import (
	"context"
	"testing"

	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
)

func TestAlwaysAllow_NeverRevoked(t *testing.T) {
	t.Parallel()

	c := jwtverifier.AlwaysAllow{}
	revoked, err := c.IsRevoked(context.Background(), &jwtverifier.Claims{JTI: "x"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if revoked {
		t.Fatalf("revoked = true, want false")
	}
}
