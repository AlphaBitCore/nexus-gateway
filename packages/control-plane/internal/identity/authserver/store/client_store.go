// Package store implements data-access helpers for the auth server. Each store
// wraps *pgxpool.Pool and exposes the minimum methods the OAuth flows need.
package store

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClientPgxPool is the minimum pgx pool surface ClientStore methods need.
// The concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention from
// packages/control-plane/internal/store/db.go and the sibling
// authserver/revocation Store.
type ClientPgxPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OAuthClient mirrors the subset of the OAuthClient row used by the auth
// server. Mutation fields (createdAt/updatedAt) are intentionally omitted.
type OAuthClient struct {
	ID                string
	Name              string
	Type              string // "public" | "confidential"
	RedirectURIs      []string
	AllowedScopes     []string
	RequirePKCE       bool
	AccessTTLSeconds  int
	RefreshTTLSeconds int
	ClientSecretHash  *string
}

// ClientStore loads OAuth client registrations.
type ClientStore struct{ db ClientPgxPool }

// NewClientStore returns a ClientStore backed by the supplied pool.
func NewClientStore(db *pgxpool.Pool) *ClientStore { return &ClientStore{db: db} }

// NewClientStoreWithPool is the test-only constructor accepting any
// ClientPgxPool implementation (notably pgxmock.PgxPoolIface). Production
// callers must use NewClientStore.
func NewClientStoreWithPool(db ClientPgxPool) *ClientStore { return &ClientStore{db: db} }

// ErrClientNotFound is returned when the client id does not exist.
var ErrClientNotFound = errors.New("oauth_client: not found")

// GetByID returns the registered client or ErrClientNotFound.
func (s *ClientStore) GetByID(ctx context.Context, id string) (*OAuthClient, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, name, type, "redirectUris", "allowedScopes", "requirePkce",
		        "accessTtlSeconds", "refreshTtlSeconds", "clientSecretHash"
		   FROM "OAuthClient"
		  WHERE id = $1`, id)
	var c OAuthClient
	if err := row.Scan(
		&c.ID, &c.Name, &c.Type,
		&c.RedirectURIs, &c.AllowedScopes, &c.RequirePKCE,
		&c.AccessTTLSeconds, &c.RefreshTTLSeconds, &c.ClientSecretHash,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	return &c, nil
}

// RedirectAllowed reports whether candidate is permitted by the client's
// registered redirect URIs. Exact matches are accepted verbatim; otherwise the
// registered pattern is evaluated against matchLoopback, which implements
// RFC 8252 §7.3 loopback redirect matching with a port-position wildcard.
func RedirectAllowed(c OAuthClient, candidate string) bool {
	for _, pat := range c.RedirectURIs {
		if pat == candidate {
			return true
		}
		if matchLoopback(pat, candidate) {
			return true
		}
	}
	return false
}

// portWildcardRe matches a ":*" port wildcard that sits in the URL port
// position (immediately after the host, before "/" or end of string). This
// ensures a "*" appearing elsewhere in the pattern (e.g. in the path) is not
// mistaken for the port wildcard.
var portWildcardRe = regexp.MustCompile(`^(https?://(?:\[[^\]]+\]|[^/:\[]+)):\*(/|$)`)

// matchLoopback implements RFC 8252 §7.3 loopback redirect matching.
// Accepts IPv4 (127.0.0.1) and IPv6 (::1) loopback. A literal "*" in the
// port position matches any numeric port on the candidate.
func matchLoopback(pattern, candidate string) bool {
	// url.Parse rejects ":*" as an invalid port, so substitute a placeholder
	// port in the pattern and remember that the pattern had a wildcard.
	hasPortWildcard := false
	parsePattern := pattern
	if m := portWildcardRe.FindStringSubmatchIndex(pattern); m != nil {
		hasPortWildcard = true
		parsePattern = pattern[:m[3]] + ":0" + pattern[m[4]:]
	}
	pu, err := url.Parse(parsePattern)
	if err != nil {
		return false
	}
	cu, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	if pu.Scheme != "http" || cu.Scheme != "http" {
		return false
	}
	pHost := pu.Hostname()
	if pHost != "127.0.0.1" && pHost != "::1" {
		return false
	}
	if pu.Hostname() != cu.Hostname() {
		return false
	}
	if pu.Path != cu.Path {
		return false
	}
	if pu.RawQuery != cu.RawQuery {
		return false
	}
	cPort := cu.Port()
	if hasPortWildcard {
		if cPort == "" {
			return false
		}
		if _, err := strconv.Atoi(cPort); err != nil {
			return false
		}
		return true
	}
	return pu.Port() == cPort
}
