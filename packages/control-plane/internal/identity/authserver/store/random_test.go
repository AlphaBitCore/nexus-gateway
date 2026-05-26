package store_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func TestRandomOpaqueToken_NonEmpty(t *testing.T) {
	tok := store.RandomOpaqueToken(32)
	if tok == "" {
		t.Fatal("expected non-empty token")
	}
}

func TestRandomOpaqueToken_URLSafe(t *testing.T) {
	tok := store.RandomOpaqueToken(32)
	// RawURLEncoding never emits '+', '/' or '='.
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token contains non-URL-safe characters: %q", tok)
	}
	if _, err := base64.RawURLEncoding.DecodeString(tok); err != nil {
		t.Fatalf("token is not valid base64url: %v", err)
	}
}

func TestRandomOpaqueToken_Length(t *testing.T) {
	// base64 RawURLEncoding expands n bytes to ceil(n*4/3) characters.
	cases := []struct {
		n      int
		wantLn int
	}{
		{n: 1, wantLn: base64.RawURLEncoding.EncodedLen(1)},
		{n: 16, wantLn: base64.RawURLEncoding.EncodedLen(16)},
		{n: 32, wantLn: base64.RawURLEncoding.EncodedLen(32)},
		{n: 64, wantLn: base64.RawURLEncoding.EncodedLen(64)},
	}
	for _, tc := range cases {
		got := store.RandomOpaqueToken(tc.n)
		if len(got) != tc.wantLn {
			t.Fatalf("n=%d: got len=%d, want %d (%q)", tc.n, len(got), tc.wantLn, got)
		}
	}
}

func TestRandomOpaqueToken_Unique(t *testing.T) {
	// Collisions are astronomically unlikely with 32 bytes of entropy; any
	// duplicate here almost certainly means the RNG is broken.
	seen := make(map[string]struct{}, 128)
	for range 128 {
		tok := store.RandomOpaqueToken(32)
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token emitted: %q", tok)
		}
		seen[tok] = struct{}{}
	}
}
