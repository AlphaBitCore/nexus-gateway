package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func newClientMock(t *testing.T) (pgxmock.PgxPoolIface, *store.ClientStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewClientStoreWithPool(mock)
}

// clientRowCols matches the SELECT in ClientStore.GetByID column-for-column.
var clientRowCols = []string{
	"id", "name", "type", "redirectUris", "allowedScopes", "requirePkce",
	"accessTtlSeconds", "refreshTtlSeconds", "clientSecretHash",
}

// TestClientStore_GetByID_HappyPath asserts: when the row exists, the
// returned OAuthClient has every column copied through, including the
// optional clientSecretHash pointer.
func TestClientStore_GetByID_HappyPath(t *testing.T) {
	mock, s := newClientMock(t)
	ctx := context.Background()
	hash := "argon2id$hash"

	mock.ExpectQuery(`SELECT id, name, type, "redirectUris"`).
		WithArgs("client-1").
		WillReturnRows(pgxmock.NewRows(clientRowCols).AddRow(
			"client-1", "Agent Desktop", "confidential",
			[]string{"http://127.0.0.1:*/callback"},
			[]string{"traffic:write", "shadow:read"},
			true, 3600, 86400, &hash,
		))

	c, err := s.GetByID(ctx, "client-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if c.ID != "client-1" || c.Type != "confidential" || !c.RequirePKCE {
		t.Fatalf("unexpected row: %+v", c)
	}
	if len(c.AllowedScopes) != 2 || c.AllowedScopes[0] != "traffic:write" {
		t.Fatalf("scopes not round-tripped: %v", c.AllowedScopes)
	}
	if c.AccessTTLSeconds != 3600 || c.RefreshTTLSeconds != 86400 {
		t.Fatalf("TTL values not round-tripped: %+v", c)
	}
	if c.ClientSecretHash == nil || *c.ClientSecretHash != hash {
		t.Fatalf("secret hash not round-tripped: %v", c.ClientSecretHash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestClientStore_GetByID_NotFound asserts pgx.ErrNoRows is mapped to the
// caller-friendly sentinel ErrClientNotFound, not the raw pgx error.
func TestClientStore_GetByID_NotFound(t *testing.T) {
	mock, s := newClientMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, name, type`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	c, err := s.GetByID(ctx, "missing")
	if c != nil {
		t.Fatalf("client should be nil on not-found; got %+v", c)
	}
	if !errors.Is(err, store.ErrClientNotFound) {
		t.Fatalf("expected ErrClientNotFound; got %v", err)
	}
}

// TestRedirectAllowed_MatchLoopback_NonLoopbackHost asserts the
// matchLoopback path (called for every pattern even non-loopback ones)
// correctly rejects a pattern whose host parses but isn't 127.0.0.1
// or ::1 — guards against a non-loopback pattern with a `:*` typo
// silently allowing any port.
func TestRedirectAllowed_MatchLoopback_NonLoopbackHost(t *testing.T) {
	c := store.OAuthClient{
		RedirectURIs: []string{"http://example.com:*/callback"}, // non-loopback host with port wildcard
	}
	if store.RedirectAllowed(c, "http://example.com:8080/callback") {
		t.Fatal("non-loopback host must NEVER match via the wildcard path")
	}
}

// TestRedirectAllowed_MatchLoopback_QueryStringMismatch asserts the
// raw-query equality check rejects a candidate that adds query
// parameters not present in the registered pattern — required by
// RFC 8252 to prevent open-redirect via querystring injection.
func TestRedirectAllowed_MatchLoopback_QueryStringMismatch(t *testing.T) {
	c := store.OAuthClient{
		RedirectURIs: []string{"http://127.0.0.1:*/callback?expected=1"},
	}
	if store.RedirectAllowed(c, "http://127.0.0.1:5000/callback?expected=2") {
		t.Fatal("mismatched query string must fail loopback match")
	}
}

// TestRedirectAllowed_MatchLoopback_NoPortWildcardExactPortMatch
// asserts that when the pattern has no `:*` wildcard but is still a
// loopback URL, the matcher falls through to exact port equality
// (the final `return pu.Port() == cPort` branch).
func TestRedirectAllowed_MatchLoopback_NoPortWildcardExactPortMatch(t *testing.T) {
	c := store.OAuthClient{
		RedirectURIs: []string{"http://127.0.0.1:8080/callback"},
	}
	// Same as registered → matches via exact-equality in RedirectAllowed.
	if !store.RedirectAllowed(c, "http://127.0.0.1:8080/callback") {
		t.Fatal("exact loopback URL must match")
	}
	// Different concrete port → must not match (no wildcard).
	if store.RedirectAllowed(c, "http://127.0.0.1:9090/callback") {
		t.Fatal("differing port without wildcard must not match")
	}
}

// TestClientStore_GetByID_GenericScanError asserts non-ErrNoRows scan
// failures are surfaced verbatim (no sentinel substitution) so callers see
// the underlying DB error in logs.
func TestClientStore_GetByID_GenericScanError(t *testing.T) {
	mock, s := newClientMock(t)
	ctx := context.Background()

	boom := errors.New("connection reset by peer")
	mock.ExpectQuery(`SELECT id, name, type`).
		WithArgs("client-2").
		WillReturnError(boom)

	c, err := s.GetByID(ctx, "client-2")
	if c != nil {
		t.Fatalf("client should be nil on generic error; got %+v", c)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected generic error passthrough; got %v", err)
	}
	if errors.Is(err, store.ErrClientNotFound) {
		t.Fatal("generic error must not be mapped to ErrClientNotFound")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("generic error must not look like a not-found: %v", err)
	}
}
