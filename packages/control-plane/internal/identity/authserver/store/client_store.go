// Package store implements data-access helpers for the auth server. Each store
// wraps *pgxpool.Pool and exposes the minimum methods the OAuth flows need.
package store

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClientPgxPool is the minimum pgx pool surface ClientStore methods need.
// The concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention from
// packages/control-plane/internal/store/db.go and the sibling
// authserver/revocation Store.
type ClientPgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OAuthClient mirrors the OAuthClient row. The admin surface needs the
// timestamp fields (created/updated/lastRotated) so they ride alongside the
// runtime fields used by the OAuth flows.
type OAuthClient struct {
	ID                  string
	Name                string
	Type                string // "public" | "confidential"
	RedirectURIs        []string
	AllowedScopes       []string
	RequirePKCE         bool
	AccessTTLSeconds    int
	RefreshTTLSeconds   int
	ClientSecretHash    *string
	LastSecretRotatedAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
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

// ErrClientIDExists is returned when Create receives an id already in use.
var ErrClientIDExists = errors.New("oauth_client: id already exists")

// clientColumns is the column projection used by every SELECT, in the order
// the OAuthClient struct expects when scanning. Centralising it prevents
// drift between GetByID / List / Create / Update / RotateSecret.
const clientColumns = `id, name, type, "redirectUris", "allowedScopes",
		"requirePkce", "accessTtlSeconds", "refreshTtlSeconds",
		"clientSecretHash", "lastSecretRotatedAt", "createdAt", "updatedAt"`

// scanClient reads one row in the clientColumns order.
func scanClient(row pgx.Row, c *OAuthClient) error {
	return row.Scan(
		&c.ID, &c.Name, &c.Type,
		&c.RedirectURIs, &c.AllowedScopes, &c.RequirePKCE,
		&c.AccessTTLSeconds, &c.RefreshTTLSeconds,
		&c.ClientSecretHash, &c.LastSecretRotatedAt,
		&c.CreatedAt, &c.UpdatedAt,
	)
}

// GetByID returns the registered client or ErrClientNotFound.
func (s *ClientStore) GetByID(ctx context.Context, id string) (*OAuthClient, error) {
	row := s.db.QueryRow(ctx,
		`SELECT `+clientColumns+`
		   FROM "OAuthClient"
		  WHERE id = $1`, id)
	var c OAuthClient
	if err := scanClient(row, &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	return &c, nil
}

// List returns every registered client, oldest-first by createdAt.
func (s *ClientStore) List(ctx context.Context) ([]*OAuthClient, error) {
	rows, err := s.db.Query(ctx,
		`SELECT `+clientColumns+`
		   FROM "OAuthClient"
		  ORDER BY "createdAt" ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*OAuthClient, 0)
	for rows.Next() {
		var c OAuthClient
		if err := scanClient(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateInput captures every field needed to insert a new OAuthClient row.
// SecretHash is nil for public clients and a bcrypt hash for confidential.
type CreateInput struct {
	ID                string
	Name              string
	Type              string
	RedirectURIs      []string
	AllowedScopes     []string
	RequirePKCE       bool
	AccessTTLSeconds  int
	RefreshTTLSeconds int
	SecretHash        *string
}

// Create inserts a new OAuthClient row and returns the persisted record.
// Returns ErrClientIDExists when the id collides with an existing row.
func (s *ClientStore) Create(ctx context.Context, in CreateInput) (*OAuthClient, error) {
	row := s.db.QueryRow(ctx,
		`INSERT INTO "OAuthClient"
		    (id, name, type, "redirectUris", "allowedScopes", "requirePkce",
		     "accessTtlSeconds", "refreshTtlSeconds", "clientSecretHash",
		     "createdAt", "updatedAt")
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
		 RETURNING `+clientColumns,
		in.ID, in.Name, in.Type, in.RedirectURIs, in.AllowedScopes,
		in.RequirePKCE, in.AccessTTLSeconds, in.RefreshTTLSeconds, in.SecretHash)
	var c OAuthClient
	if err := scanClient(row, &c); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrClientIDExists
		}
		return nil, err
	}
	return &c, nil
}

// UpdateInput captures the mutable subset of the OAuthClient row. Nil pointers
// mean "leave this field untouched" — this gives the PATCH handler partial-
// update semantics without requiring a per-field SQL builder.
type UpdateInput struct {
	Name              *string
	RedirectURIs      *[]string
	AllowedScopes     *[]string
	RequirePKCE       *bool
	AccessTTLSeconds  *int
	RefreshTTLSeconds *int
}

// Update applies a partial update to an OAuthClient row. Returns the refreshed
// record or ErrClientNotFound if the id does not exist.
func (s *ClientStore) Update(ctx context.Context, id string, in UpdateInput) (*OAuthClient, error) {
	row := s.db.QueryRow(ctx,
		`UPDATE "OAuthClient"
		    SET name              = COALESCE($2, name),
		        "redirectUris"    = COALESCE($3, "redirectUris"),
		        "allowedScopes"   = COALESCE($4, "allowedScopes"),
		        "requirePkce"     = COALESCE($5, "requirePkce"),
		        "accessTtlSeconds" = COALESCE($6, "accessTtlSeconds"),
		        "refreshTtlSeconds" = COALESCE($7, "refreshTtlSeconds"),
		        "updatedAt"       = NOW()
		  WHERE id = $1
		  RETURNING `+clientColumns,
		id, in.Name, in.RedirectURIs, in.AllowedScopes, in.RequirePKCE,
		in.AccessTTLSeconds, in.RefreshTTLSeconds)
	var c OAuthClient
	if err := scanClient(row, &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	return &c, nil
}

// Delete removes the OAuthClient row by id. Dependent RefreshToken rows are
// cascade-deleted by the FK (see 20260612000000_oauth_client_admin migration).
// Returns ErrClientNotFound if no row matched.
func (s *ClientStore) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM "OAuthClient" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrClientNotFound
	}
	return nil
}

// RotateSecret atomically swaps clientSecretHash and stamps lastSecretRotatedAt.
// Returns ErrClientNotFound if the id does not exist.
func (s *ClientStore) RotateSecret(ctx context.Context, id string, newHash []byte) (*OAuthClient, error) {
	row := s.db.QueryRow(ctx,
		`UPDATE "OAuthClient"
		    SET "clientSecretHash"    = $2,
		        "lastSecretRotatedAt" = NOW(),
		        "updatedAt"           = NOW()
		  WHERE id = $1
		  RETURNING `+clientColumns,
		id, string(newHash))
	var c OAuthClient
	if err := scanClient(row, &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	return &c, nil
}

// CountActiveRefreshTokens returns the number of refresh tokens that are
// still redeemable for this client — not yet consumed and not yet expired.
func (s *ClientStore) CountActiveRefreshTokens(ctx context.Context, clientID string) (int, error) {
	row := s.db.QueryRow(ctx,
		`SELECT COUNT(*)
		   FROM "RefreshToken"
		  WHERE "clientId" = $1
		    AND "usedAt" IS NULL
		    AND "expiresAt" > NOW()`, clientID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
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
// Accepts IPv4 (127.0.0.1), IPv6 (::1), and the localhost hostname. A literal
// "*" in the port position matches any numeric port on the candidate. RFC 8252
// recommends IP literals over localhost, but tooling that registers a
// localhost callback is common enough that we honor it too; the candidate host
// must still equal the registered host (a localhost pattern matches only
// localhost candidates, never 127.0.0.1, and vice versa).
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
	if pHost != "127.0.0.1" && pHost != "::1" && pHost != "localhost" {
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

// ValidRedirectURIPattern reports whether raw is acceptable as a *registered*
// redirect URI. It is the registration-time counterpart to matchLoopback (the
// authorize-time matcher): a pattern this accepts is one that exact-match or
// matchLoopback can later honor, so the admin CRUD must accept exactly this
// set — no more, no less. The control-plane-ui form mirrors this rule.
//
// Accepted:
//   - any https:// URL (fixed host/port; the loopback wildcard is meaningless
//     for https, so a "https://h:*/cb" pattern is rejected — it could never match);
//   - http:// loopback (localhost, 127.0.0.1, ::1) with a fixed port, or with
//     the RFC 8252 §7.3 ":*" port wildcard. matchLoopback honors all three
//     loopback hosts for the wildcard, so registration accepts them too.
func ValidRedirectURIPattern(raw string) bool {
	if raw == "" {
		return false
	}
	// url.Parse rejects ":*" as an invalid port, so substitute a placeholder
	// port before parsing (same trick as matchLoopback).
	hasPortWildcard := false
	parseTarget := raw
	if m := portWildcardRe.FindStringSubmatchIndex(raw); m != nil {
		hasPortWildcard = true
		parseTarget = raw[:m[3]] + ":0" + raw[m[4]:]
	}
	u, err := url.Parse(parseTarget)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return !hasPortWildcard
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}
