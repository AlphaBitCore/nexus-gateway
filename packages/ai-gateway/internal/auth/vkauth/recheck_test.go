package vkauth

// recheck_test.go — the long-session by-hash seam: AuthenticateWithHash must
// return the MATCHED (DB-stored) hash, and RecheckByHash must distinguish a
// definitive negative (sever) from an indeterminate failure (fail open).

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

func strPtr(s string) *string { return &s }

// TestAuthenticateWithHash_ReturnsMatchedHash pins the seam's core promise:
// the returned hash is exactly the DB-stored hash that matched, so a later
// RecheckByHash resolves the SAME row without retaining the raw token — even
// when the match happened under an OLDER keyring version.
func TestAuthenticateWithHash_ReturnsMatchedHash(t *testing.T) {
	token := "nvk_live_session_token_1"
	oldHash := vkHashFor("old-secret", token)
	lookup := &fakeLookup{byHash: map[string]*store.VirtualKey{
		oldHash: {ID: "vk-1", Name: "sess", Enabled: true, VKStatus: strPtr("active")},
	}}
	// Current version first; the token only matches under the OLD version,
	// so the matched hash must be the old-version hash, not the current one.
	a := NewAuthenticator(lookup, mustKeyringMap("*v2:new-secret,v1:old-secret"), quietLogger())

	r, _ := http.NewRequest(http.MethodGet, "/v1/realtime?model=m", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	meta, hash, err := a.AuthenticateWithHash(context.Background(), r)
	if err != nil {
		t.Fatalf("AuthenticateWithHash: %v", err)
	}
	if meta.ID != "vk-1" {
		t.Errorf("meta.ID = %q, want vk-1", meta.ID)
	}
	if hash != oldHash {
		t.Errorf("matched hash = %q, want the old-version hash %q", hash, oldHash)
	}

	// Plain Authenticate must keep its shipped contract (same meta, no hash).
	meta2, err := a.Authenticate(context.Background(), r)
	if err != nil || meta2.ID != "vk-1" {
		t.Fatalf("Authenticate = (%v, %v), want vk-1", meta2, err)
	}
}

// TestAuthenticateWithHash_FailureArms: every refusal returns an EMPTY hash —
// a caller must never retain a hash for a session that was not admitted.
func TestAuthenticateWithHash_FailureArms(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	token := "nvk_live_session_token_2"
	hash := vkHashFor("s", token)

	cases := []struct {
		name    string
		vk      *store.VirtualKey
		wantErr error
	}{
		{"disabled", &store.VirtualKey{ID: "vk-d", Enabled: false}, ErrDisabled},
		{"expired", &store.VirtualKey{ID: "vk-e", Enabled: true, ExpiresAt: &expired}, ErrExpired},
		{"revoked status", &store.VirtualKey{ID: "vk-r", Enabled: true, VKStatus: strPtr("revoked")}, ErrDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAuthenticator(&fakeLookup{byHash: map[string]*store.VirtualKey{hash: tc.vk}},
				mustKeyring("s"), quietLogger())
			r, _ := http.NewRequest(http.MethodGet, "/v1/realtime", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			meta, gotHash, err := a.AuthenticateWithHash(context.Background(), r)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if meta != nil || gotHash != "" {
				t.Errorf("refusal leaked (meta=%v, hash=%q); both must be empty", meta, gotHash)
			}
		})
	}

	t.Run("missing token", func(t *testing.T) {
		a := NewAuthenticator(&fakeLookup{}, mustKeyring("s"), quietLogger())
		r, _ := http.NewRequest(http.MethodGet, "/v1/realtime", nil)
		_, gotHash, err := a.AuthenticateWithHash(context.Background(), r)
		if !errors.Is(err, ErrMissing) || gotHash != "" {
			t.Fatalf("= (%q, %v), want empty hash + ErrMissing", gotHash, err)
		}
	})
}

// TestRecheckByHash covers the sever-vs-fail-open contract on every arm.
func TestRecheckByHash(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	active := &store.VirtualKey{ID: "vk-1", Enabled: true, VKStatus: strPtr("active")}

	cases := []struct {
		name        string
		vk          *store.VirtualKey // nil = row absent (fakeLookup → pgx.ErrNoRows)
		lookupErr   error
		wantOutcome RecheckOutcome
		wantErr     bool
	}{
		{"active row continues", active, nil, RecheckActive, false},
		{"empty status treated active", &store.VirtualKey{ID: "vk-1", Enabled: true}, nil, RecheckActive, false},
		{"row deleted is definitive", nil, nil, RecheckRevoked, false},
		{"disabled is definitive", &store.VirtualKey{ID: "vk-1", Enabled: false}, nil, RecheckRevoked, false},
		{"expired is definitive", &store.VirtualKey{ID: "vk-1", Enabled: true, ExpiresAt: &expired}, nil, RecheckRevoked, false},
		{"revoked status is definitive", &store.VirtualKey{ID: "vk-1", Enabled: true, VKStatus: strPtr("revoked")}, nil, RecheckRevoked, false},
		{"pending status is definitive", &store.VirtualKey{ID: "vk-1", Enabled: true, VKStatus: strPtr("pending")}, nil, RecheckRevoked, false},
		// The load-bearing distinction: a backend blip is NOT a revocation —
		// the caller fails open and retries; severing here would mass-kill
		// every live session on a transient Redis/DB hiccup.
		{"lookup error is indeterminate", nil, errors.New("connection refused"), RecheckActive, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := &fakeLookup{byHash: map[string]*store.VirtualKey{}, err: tc.lookupErr}
			if tc.vk != nil {
				lookup.byHash["h1"] = tc.vk
			}
			a := NewAuthenticator(lookup, mustKeyring("s"), quietLogger())
			outcome, err := a.RecheckByHash(context.Background(), "h1")
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestRecheckByHash_NilRow covers a lookup surface (the cache layer) that
// reports a miss as (nil, nil) rather than pgx.ErrNoRows — still definitive.
func TestRecheckByHash_NilRow(t *testing.T) {
	a := NewAuthenticator(nilRowLookup{}, mustKeyring("s"), quietLogger())
	outcome, err := a.RecheckByHash(context.Background(), "whatever")
	if outcome != RecheckRevoked || err != nil {
		t.Fatalf("= (%v, %v), want (RecheckRevoked, nil)", outcome, err)
	}
}

type nilRowLookup struct{}

func (nilRowLookup) GetVirtualKeyByHash(_ context.Context, _ string) (*store.VirtualKey, error) {
	return nil, nil
}
