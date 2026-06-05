package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func newIdPMock(t *testing.T) (pgxmock.PgxPoolIface, *store.IdPStore) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, store.NewIdPStoreWithPool(mock)
}

// idpRowCols mirrors the column list returned by every IdPStore SELECT so
// rows added via pgxmock decode straight through scanIdP.
var idpRowCols = []string{
	"id", "type", "name", "enabled", "config", "defaultRole", "defaultControlPlaneAccess", "jitEnabled",
}

// TestIdPStore_ListEnabled_HappyPath asserts the rows decoder unpacks the
// JSONB config column into native Go maps without losing nested values.
func TestIdPStore_ListEnabled_HappyPath(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	cfg := []byte(`{"issuer":"https://idp.example","clientId":"abc"}`)
	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WillReturnRows(pgxmock.NewRows(idpRowCols).
			AddRow("idp_1", "oidc", "Okta", true, cfg, "developer", false, true).
			AddRow("idp_2", "local", "Local", true, []byte(`{}`), "viewer", false, false))

	out, err := s.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows; got %d", len(out))
	}
	if out[0].ID != "idp_1" || out[0].Type != "oidc" || !out[0].JITEnabled {
		t.Fatalf("row[0]: %+v", out[0])
	}
	if out[0].Config["issuer"] != "https://idp.example" {
		t.Fatalf("config json not decoded: %v", out[0].Config)
	}
	if out[1].DefaultRole != "viewer" || out[1].JITEnabled {
		t.Fatalf("row[1]: %+v", out[1])
	}
}

// TestIdPStore_ListEnabled_QueryError asserts a Query-level failure
// (connection drop, syntax error) is surfaced verbatim.
func TestIdPStore_ListEnabled_QueryError(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()
	boom := errors.New("query failed")

	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WillReturnError(boom)

	out, err := s.ListEnabled(ctx)
	if out != nil {
		t.Fatalf("on err result must be nil; got %v", out)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected query err to surface; got %v", err)
	}
}

// TestIdPStore_ListEnabled_ScanError asserts a row-level scan failure
// propagates so callers don't silently get a truncated list.
func TestIdPStore_ListEnabled_ScanError(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	// AddRow returns a non-bool where bool is expected → row.Scan fails
	// when the iterator hits this row.
	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WillReturnRows(pgxmock.NewRows(idpRowCols).
			AddRow("idp_bad", "oidc", "Bad", "not-a-bool", []byte(`{}`), "viewer", false, true))

	out, err := s.ListEnabled(ctx)
	if err == nil {
		t.Fatalf("expected scan err; got nil and out=%v", out)
	}
}

// TestIdPStore_ListEnabled_InvalidConfigJSON asserts that an
// undecodable JSONB blob in `config` is reported as an error so a
// malformed admin write is loud rather than silently dropping fields.
func TestIdPStore_ListEnabled_InvalidConfigJSON(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WillReturnRows(pgxmock.NewRows(idpRowCols).
			AddRow("idp_x", "oidc", "X", true, []byte(`{not-json`), "viewer", false, true))

	out, err := s.ListEnabled(ctx)
	if err == nil {
		t.Fatalf("expected json decode err; got nil and out=%v", out)
	}
}

// TestIdPStore_GetByID_HappyPath asserts single-row lookup decodes
// the JSONB config column into the typed struct.
func TestIdPStore_GetByID_HappyPath(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WithArgs("idp_1").
		WillReturnRows(pgxmock.NewRows(idpRowCols).
			AddRow("idp_1", "oidc", "Okta", true, []byte(`{"audience":"a1"}`), "viewer", false, true))

	p, err := s.GetByID(ctx, "idp_1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if p.ID != "idp_1" || p.Config["audience"] != "a1" {
		t.Fatalf("unexpected idp row: %+v", p)
	}
}

// TestIdPStore_GetByID_NotFound asserts pgx.ErrNoRows -> ErrIdPNotFound.
func TestIdPStore_GetByID_NotFound(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	p, err := s.GetByID(ctx, "missing")
	if p != nil {
		t.Fatalf("idp should be nil on not-found; got %+v", p)
	}
	if !errors.Is(err, store.ErrIdPNotFound) {
		t.Fatalf("expected ErrIdPNotFound; got %v", err)
	}
}

// TestIdPStore_GetByID_GenericError asserts generic scan err passes
// through unwrapped (no sentinel substitution).
func TestIdPStore_GetByID_GenericError(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()
	boom := errors.New("scan boom")

	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WithArgs("idp_err").
		WillReturnError(boom)

	p, err := s.GetByID(ctx, "idp_err")
	if p != nil {
		t.Fatalf("idp should be nil on err; got %+v", p)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected generic err passthrough; got %v", err)
	}
	if errors.Is(err, store.ErrIdPNotFound) {
		t.Fatal("generic error must not be mapped to ErrIdPNotFound")
	}
}

// TestIdPStore_GetLocal_HappyPath asserts the WHERE type='local' lookup
// returns the local IdP row.
func TestIdPStore_GetLocal_HappyPath(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`WHERE type = 'local' AND enabled = TRUE`).
		WillReturnRows(pgxmock.NewRows(idpRowCols).
			AddRow("idp_local", "local", "Local", true, []byte(`{}`), "viewer", false, false))

	p, err := s.GetLocal(ctx)
	if err != nil {
		t.Fatalf("GetLocal: %v", err)
	}
	if p.Type != "local" || !p.Enabled {
		t.Fatalf("unexpected local row: %+v", p)
	}
}

// TestIdPStore_GetLocal_NotFound asserts ErrIdPNotFound sentinel.
func TestIdPStore_GetLocal_NotFound(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`WHERE type = 'local' AND enabled = TRUE`).
		WillReturnError(pgx.ErrNoRows)

	p, err := s.GetLocal(ctx)
	if p != nil {
		t.Fatalf("idp should be nil; got %+v", p)
	}
	if !errors.Is(err, store.ErrIdPNotFound) {
		t.Fatalf("expected ErrIdPNotFound; got %v", err)
	}
}

// TestIdPStore_GetLocal_GenericError asserts non-ErrNoRows surfaces verbatim.
func TestIdPStore_GetLocal_GenericError(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()
	boom := errors.New("io")

	mock.ExpectQuery(`WHERE type = 'local' AND enabled = TRUE`).
		WillReturnError(boom)

	if _, err := s.GetLocal(ctx); !errors.Is(err, boom) {
		t.Fatalf("expected generic err; got %v", err)
	}
}

// TestIdPStore_EmptyJSONBPassthrough asserts that an empty JSONB blob
// (zero bytes, not "{}") is left as nil maps in the struct — pgx
// returns []byte{} for SQL NULL, which the decoder must accept.
func TestIdPStore_EmptyJSONBPassthrough(t *testing.T) {
	mock, s := newIdPMock(t)
	ctx := context.Background()

	mock.ExpectQuery(`SELECT id, type, name, enabled, config`).
		WithArgs("idp_empty").
		WillReturnRows(pgxmock.NewRows(idpRowCols).
			AddRow("idp_empty", "oidc", "Empty", true, []byte{}, "viewer", false, false))

	p, err := s.GetByID(ctx, "idp_empty")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if p.Config != nil {
		t.Fatalf("empty config should leave Config nil; got %v", p.Config)
	}
}
