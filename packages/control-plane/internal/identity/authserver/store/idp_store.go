package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IdPPgxPool is the minimum pgx pool surface IdPStore methods need. The
// concrete *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Mirrors the PgxPool convention
// from packages/control-plane/internal/store/db.go.
type IdPPgxPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IdentityProvider is the auth-server view of an IdP registration. Config is
// decoded from the JSONB column at load time so callers work with native Go
// values.
type IdentityProvider struct {
	ID          string
	Type        string // "local" | "oidc" | "saml"
	Name        string
	Enabled     bool
	Config      map[string]any
	DefaultRole string
	// DefaultControlPlaneAccess seeds NexusUser.canAccessControlPlane for users
	// JIT-provisioned through this IdP.
	DefaultControlPlaneAccess bool
	JITEnabled                bool
}

// IdPStore loads IdentityProvider rows.
type IdPStore struct{ db IdPPgxPool }

// NewIdPStore returns an IdPStore backed by the supplied pool.
func NewIdPStore(db *pgxpool.Pool) *IdPStore { return &IdPStore{db: db} }

// NewIdPStoreWithPool is the test-only constructor accepting any IdPPgxPool
// implementation (notably pgxmock.PgxPoolIface). Production callers must
// use NewIdPStore so the concrete-pool contract is enforced at call sites
// that depend on AcquireFunc / BeginTxFunc.
func NewIdPStoreWithPool(db IdPPgxPool) *IdPStore { return &IdPStore{db: db} }

// ErrIdPNotFound is returned when an IdP lookup resolves to no rows.
var ErrIdPNotFound = errors.New("identity_provider: not found")

// ListEnabled returns all enabled IdPs ordered by name.
func (s *IdPStore) ListEnabled(ctx context.Context) ([]IdentityProvider, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, type, name, enabled, config, "defaultRole", "defaultControlPlaneAccess", "jitEnabled"
		   FROM "IdentityProvider"
		  WHERE enabled = TRUE
		  ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdentityProvider
	for rows.Next() {
		p, err := scanIdP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetByID returns the IdP with the given id or ErrIdPNotFound.
func (s *IdPStore) GetByID(ctx context.Context, id string) (*IdentityProvider, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, type, name, enabled, config, "defaultRole", "defaultControlPlaneAccess", "jitEnabled"
		   FROM "IdentityProvider"
		  WHERE id = $1`, id)
	p, err := scanIdP(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIdPNotFound
		}
		return nil, err
	}
	return p, nil
}

// GetLocal returns the enabled local IdP. Only one local IdP is expected per
// deployment; if more than one exists the earliest-created row wins (stable
// ordering via createdAt ASC).
func (s *IdPStore) GetLocal(ctx context.Context) (*IdentityProvider, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, type, name, enabled, config, "defaultRole", "defaultControlPlaneAccess", "jitEnabled"
		   FROM "IdentityProvider"
		  WHERE type = 'local' AND enabled = TRUE
		  ORDER BY "createdAt" ASC
		  LIMIT 1`)
	p, err := scanIdP(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIdPNotFound
		}
		return nil, err
	}
	return p, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanIdP(r scannable) (*IdentityProvider, error) {
	var p IdentityProvider
	var configJSON []byte
	if err := r.Scan(&p.ID, &p.Type, &p.Name, &p.Enabled, &configJSON, &p.DefaultRole, &p.DefaultControlPlaneAccess, &p.JITEnabled); err != nil {
		return nil, err
	}
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &p.Config); err != nil {
			return nil, err
		}
	}
	return &p, nil
}
