package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// TestClientStore_List_HappyPath asserts List returns every row in
// createdAt order with the new timestamp fields populated.
func TestClientStore_List_HappyPath(t *testing.T) {
	mock, s := newClientMock(t)
	ctx := context.Background()

	rows := pgxmock.NewRows(clientRowCols)
	rows = addClientRow(rows, "older", "Older", "public", nil, nil)
	rows = addClientRow(rows, "newer", "Newer", "confidential", strPtrTest("h"), &clientNow)
	mock.ExpectQuery(`SELECT id, name, type, "redirectUris"`).WillReturnRows(rows)

	out, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].ID != "older" || out[1].ID != "newer" {
		t.Fatalf("order broken: %s, %s", out[0].ID, out[1].ID)
	}
	if out[0].LastSecretRotatedAt != nil {
		t.Fatalf("first row should have nil lastSecretRotatedAt")
	}
	if out[1].LastSecretRotatedAt == nil || !out[1].LastSecretRotatedAt.Equal(clientNow) {
		t.Fatalf("second row lastSecretRotatedAt not round-tripped")
	}
}

func TestClientStore_List_EmptyResultReturnsEmptySlice(t *testing.T) {
	mock, s := newClientMock(t)
	mock.ExpectQuery(`SELECT id, name, type`).WillReturnRows(pgxmock.NewRows(clientRowCols))
	out, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if out == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("len=%d, want 0", len(out))
	}
}

func TestClientStore_List_QueryErrorPropagates(t *testing.T) {
	mock, s := newClientMock(t)
	boom := errors.New("query boom")
	mock.ExpectQuery(`SELECT id, name, type`).WillReturnError(boom)
	if _, err := s.List(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

func TestClientStore_List_RowScanErrorPropagates(t *testing.T) {
	mock, s := newClientMock(t)
	// Return a row whose column count doesn't match — Scan must fail and
	// the error must bubble.
	mock.ExpectQuery(`SELECT id, name, type`).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("one"))
	if _, err := s.List(context.Background()); err == nil {
		t.Fatal("expected scan error, got nil")
	}
}

// TestClientStore_Create_HappyPath asserts the INSERT goes through with
// every CreateInput field bound and the returned row is rehydrated.
func TestClientStore_Create_HappyPath(t *testing.T) {
	mock, s := newClientMock(t)
	hash := "h"
	in := store.CreateInput{
		ID: "new-c", Name: "New", Type: "confidential",
		RedirectURIs:      []string{"https://x/cb"},
		AllowedScopes:     []string{"openid"},
		AccessTTLSeconds:  3600,
		RefreshTTLSeconds: 86400,
		SecretHash:        &hash,
	}
	mock.ExpectQuery(`INSERT INTO "OAuthClient"`).
		WithArgs(in.ID, in.Name, in.Type, in.RedirectURIs, in.AllowedScopes,
			in.AccessTTLSeconds, in.RefreshTTLSeconds, in.SecretHash).
		WillReturnRows(addClientRow(
			pgxmock.NewRows(clientRowCols),
			"new-c", "New", "confidential", &hash, nil,
		))

	got, err := s.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != "new-c" || got.Type != "confidential" {
		t.Fatalf("returned row mismatch: %+v", got)
	}
	if got.ClientSecretHash == nil || *got.ClientSecretHash != hash {
		t.Fatalf("hash not round-tripped")
	}
}

// TestClientStore_Create_DuplicateIDMapsToSentinel asserts the 23505 unique-
// violation gets mapped to ErrClientIDExists so the handler can return 409.
func TestClientStore_Create_DuplicateIDMapsToSentinel(t *testing.T) {
	mock, s := newClientMock(t)
	in := store.CreateInput{ID: "dup"}
	mock.ExpectQuery(`INSERT INTO "OAuthClient"`).
		WithArgs(in.ID, in.Name, in.Type, in.RedirectURIs, in.AllowedScopes,
			in.AccessTTLSeconds, in.RefreshTTLSeconds, in.SecretHash).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	_, err := s.Create(context.Background(), in)
	if !errors.Is(err, store.ErrClientIDExists) {
		t.Fatalf("err=%v, want ErrClientIDExists", err)
	}
}

func TestClientStore_Create_GenericErrorPropagates(t *testing.T) {
	mock, s := newClientMock(t)
	in := store.CreateInput{ID: "x"}
	boom := errors.New("create boom")
	mock.ExpectQuery(`INSERT INTO "OAuthClient"`).
		WithArgs(in.ID, in.Name, in.Type, in.RedirectURIs, in.AllowedScopes,
			in.AccessTTLSeconds, in.RefreshTTLSeconds, in.SecretHash).
		WillReturnError(boom)
	if _, err := s.Create(context.Background(), in); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

// TestClientStore_Update_HappyPath asserts the partial-update SQL fires with
// every COALESCE arg and the refreshed row is returned.
func TestClientStore_Update_HappyPath(t *testing.T) {
	mock, s := newClientMock(t)
	newName := "Renamed"
	in := store.UpdateInput{Name: &newName}
	mock.ExpectQuery(`UPDATE "OAuthClient"`).
		WithArgs("c1", &newName, (*[]string)(nil), (*[]string)(nil),
			(*int)(nil), (*int)(nil)).
		WillReturnRows(addClientRow(
			pgxmock.NewRows(clientRowCols),
			"c1", "Renamed", "public", nil, nil,
		))

	got, err := s.Update(context.Background(), "c1", in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "Renamed" {
		t.Fatalf("Name=%q, want Renamed", got.Name)
	}
}

func TestClientStore_Update_NotFound(t *testing.T) {
	mock, s := newClientMock(t)
	mock.ExpectQuery(`UPDATE "OAuthClient"`).
		WithArgs("missing", (*string)(nil), (*[]string)(nil), (*[]string)(nil),
			(*int)(nil), (*int)(nil)).
		WillReturnError(pgx.ErrNoRows)
	_, err := s.Update(context.Background(), "missing", store.UpdateInput{})
	if !errors.Is(err, store.ErrClientNotFound) {
		t.Fatalf("err=%v, want ErrClientNotFound", err)
	}
}

func TestClientStore_Update_GenericErrorPropagates(t *testing.T) {
	mock, s := newClientMock(t)
	boom := errors.New("upd boom")
	mock.ExpectQuery(`UPDATE "OAuthClient"`).
		WithArgs("c", (*string)(nil), (*[]string)(nil), (*[]string)(nil),
			(*int)(nil), (*int)(nil)).
		WillReturnError(boom)
	if _, err := s.Update(context.Background(), "c", store.UpdateInput{}); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

func TestClientStore_Delete_HappyPath(t *testing.T) {
	mock, s := newClientMock(t)
	mock.ExpectExec(`DELETE FROM "OAuthClient"`).
		WithArgs("c1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := s.Delete(context.Background(), "c1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestClientStore_Delete_NotFound(t *testing.T) {
	mock, s := newClientMock(t)
	mock.ExpectExec(`DELETE FROM "OAuthClient"`).
		WithArgs("missing").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	if err := s.Delete(context.Background(), "missing"); !errors.Is(err, store.ErrClientNotFound) {
		t.Fatalf("err=%v, want ErrClientNotFound", err)
	}
}

func TestClientStore_Delete_ExecErrorPropagates(t *testing.T) {
	mock, s := newClientMock(t)
	boom := errors.New("del boom")
	mock.ExpectExec(`DELETE FROM "OAuthClient"`).WithArgs("c").WillReturnError(boom)
	if err := s.Delete(context.Background(), "c"); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

func TestClientStore_RotateSecret_HappyPath(t *testing.T) {
	mock, s := newClientMock(t)
	newHash := []byte("freshhash")
	mock.ExpectQuery(`UPDATE "OAuthClient"`).
		WithArgs("c1", string(newHash)).
		WillReturnRows(addClientRow(
			pgxmock.NewRows(clientRowCols),
			"c1", "N", "confidential",
			strPtrTest("freshhash"), &clientNow,
		))
	got, err := s.RotateSecret(context.Background(), "c1", newHash)
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if got.ClientSecretHash == nil || *got.ClientSecretHash != "freshhash" {
		t.Fatalf("hash not rotated through")
	}
	if got.LastSecretRotatedAt == nil {
		t.Fatal("lastSecretRotatedAt should be set on rotation")
	}
}

func TestClientStore_RotateSecret_NotFound(t *testing.T) {
	mock, s := newClientMock(t)
	mock.ExpectQuery(`UPDATE "OAuthClient"`).
		WithArgs("missing", "h").
		WillReturnError(pgx.ErrNoRows)
	_, err := s.RotateSecret(context.Background(), "missing", []byte("h"))
	if !errors.Is(err, store.ErrClientNotFound) {
		t.Fatalf("err=%v, want ErrClientNotFound", err)
	}
}

func TestClientStore_RotateSecret_GenericErrorPropagates(t *testing.T) {
	mock, s := newClientMock(t)
	boom := errors.New("rot boom")
	mock.ExpectQuery(`UPDATE "OAuthClient"`).
		WithArgs("c", "h").
		WillReturnError(boom)
	if _, err := s.RotateSecret(context.Background(), "c", []byte("h")); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

func TestClientStore_CountActiveRefreshTokens_HappyPath(t *testing.T) {
	mock, s := newClientMock(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM "RefreshToken"`).
		WithArgs("c1").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(3))
	got, err := s.CountActiveRefreshTokens(context.Background(), "c1")
	if err != nil {
		t.Fatalf("CountActiveRefreshTokens: %v", err)
	}
	if got != 3 {
		t.Fatalf("count=%d, want 3", got)
	}
}

func TestClientStore_CountActiveRefreshTokens_ScanError(t *testing.T) {
	mock, s := newClientMock(t)
	boom := errors.New("count boom")
	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM "RefreshToken"`).
		WithArgs("c").
		WillReturnError(boom)
	if _, err := s.CountActiveRefreshTokens(context.Background(), "c"); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

func strPtrTest(s string) *string { return &s }
