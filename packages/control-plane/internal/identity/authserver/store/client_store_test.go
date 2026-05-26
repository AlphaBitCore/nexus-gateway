package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

func TestClientStore_GetByID(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewClientStore(pool)
	ctx := context.Background()
	id := "test-client-" + time.Now().Format("150405.000000000")

	_, err := pool.Exec(ctx,
		`INSERT INTO "OAuthClient"(id,name,type,"redirectUris","allowedScopes","requirePkce","updatedAt")
		 VALUES ($1,$2,'public',$3,$4,TRUE,NOW())`,
		id, "Agent Desktop",
		[]string{"http://127.0.0.1:*/callback"},
		[]string{"traffic:write", "shadow:read"},
	)
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "OAuthClient" WHERE id=$1`, id)
	})

	c, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if c.Type != "public" || !c.RequirePKCE || len(c.RedirectURIs) != 1 {
		t.Fatalf("unexpected client row: %+v", c)
	}
	if len(c.AllowedScopes) != 2 {
		t.Fatalf("allowed scopes round-trip mismatch: %v", c.AllowedScopes)
	}

	if _, err := s.GetByID(ctx, "nope-"+id); !errors.Is(err, store.ErrClientNotFound) {
		t.Fatalf("expected ErrClientNotFound, got %v", err)
	}
}

func TestClientStore_RedirectAllowed_LoopbackWildcard(t *testing.T) {
	cases := []struct {
		name      string
		patterns  []string
		candidate string
		want      bool
	}{
		{
			name:      "ipv4 port wildcard matches concrete port",
			patterns:  []string{"http://127.0.0.1:*/callback"},
			candidate: "http://127.0.0.1:54321/callback",
			want:      true,
		},
		{
			name:      "off-host redirect is denied",
			patterns:  []string{"http://127.0.0.1:*/callback"},
			candidate: "http://evil.example/callback",
			want:      false,
		},
		{
			name:      "path must match exactly",
			patterns:  []string{"http://127.0.0.1:*/callback"},
			candidate: "http://127.0.0.1:54321/other",
			want:      false,
		},
		{
			name:      "ipv6 ::1 port wildcard matches concrete port",
			patterns:  []string{"http://[::1]:*/callback"},
			candidate: "http://[::1]:53821/callback",
			want:      true,
		},
		{
			name:      "ipv4 pattern does not match ipv6 candidate",
			patterns:  []string{"http://127.0.0.1:*/callback"},
			candidate: "http://[::1]:53821/callback",
			want:      false,
		},
		{
			name:      "wildcard in path is not interpreted as port wildcard",
			patterns:  []string{"http://127.0.0.1/*"},
			candidate: "http://127.0.0.1:8080/anything",
			want:      false,
		},
		{
			name:      "port wildcard requires non-empty candidate port",
			patterns:  []string{"http://127.0.0.1:*/callback"},
			candidate: "http://127.0.0.1/callback",
			want:      false,
		},
		{
			name:      "port wildcard rejects non-numeric candidate port",
			patterns:  []string{"http://127.0.0.1:*/callback"},
			candidate: "http://127.0.0.1:abc/callback",
			want:      false,
		},
		{
			name:      "exact https match succeeds",
			patterns:  []string{"https://cp.nexus.ai/callback"},
			candidate: "https://cp.nexus.ai/callback",
			want:      true,
		},
		{
			name:      "exact match rejects sneaked querystring",
			patterns:  []string{"https://cp.nexus.ai/callback"},
			candidate: "https://cp.nexus.ai/callback?x=1",
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := store.OAuthClient{RedirectURIs: tc.patterns}
			got := store.RedirectAllowed(c, tc.candidate)
			if got != tc.want {
				t.Fatalf("RedirectAllowed(%v, %q) = %v, want %v", tc.patterns, tc.candidate, got, tc.want)
			}
		})
	}
}
