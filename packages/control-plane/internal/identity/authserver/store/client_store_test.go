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
			name:      "localhost port wildcard matches concrete port",
			patterns:  []string{"http://localhost:*/callback"},
			candidate: "http://localhost:54321/callback",
			want:      true,
		},
		{
			name:      "localhost pattern does not match 127.0.0.1 candidate",
			patterns:  []string{"http://localhost:*/callback"},
			candidate: "http://127.0.0.1:54321/callback",
			want:      false,
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

func TestClientStore_ValidRedirectURIPattern(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty rejected", raw: "", want: false},
		{name: "https any host", raw: "https://cp.nexus.ai/callback", want: true},
		{name: "http localhost fixed port", raw: "http://localhost:8080/cb", want: true},
		{name: "http 127.0.0.1 fixed port", raw: "http://127.0.0.1:9000/cb", want: true},
		{name: "http ipv6 loopback fixed port", raw: "http://[::1]:9000/cb", want: true},
		// The bug this fix targets: the tui CLI client's loopback port wildcard.
		{name: "http 127.0.0.1 port wildcard", raw: "http://127.0.0.1:*/callback", want: true},
		{name: "http ipv6 loopback port wildcard", raw: "http://[::1]:*/callback", want: true},
		// localhost+wildcard is honored by matchLoopback, so registration accepts it.
		{name: "http localhost port wildcard", raw: "http://localhost:*/cb", want: true},
		// https with a port wildcard is meaningless and can never match.
		{name: "https port wildcard rejected", raw: "https://cp.nexus.ai:*/cb", want: false},
		{name: "http non-loopback host rejected", raw: "http://evil.example/cb", want: false},
		{name: "http localhost lookalike rejected", raw: "http://localhost.evil.com/cb", want: false},
		{name: "unparseable rejected", raw: "://bad", want: false},
		{name: "non-http scheme rejected", raw: "ftp://127.0.0.1/cb", want: false},
		// A "*" outside the port position must not be read as a port wildcard.
		{name: "path wildcard not a port wildcard", raw: "http://127.0.0.1/*", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.ValidRedirectURIPattern(tc.raw); got != tc.want {
				t.Fatalf("ValidRedirectURIPattern(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
