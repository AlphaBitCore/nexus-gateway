// Tests for the userstore package.
package userstore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestGetNexusUser_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM nexus_user`).
		WithArgs("user-1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "email"}).
			AddRow("user-1", "Alice", "alice@example.com"))

	s := New(mock)
	u, err := s.GetNexusUser(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GetNexusUser: %v", err)
	}
	if u.ID != "user-1" || u.Name != "Alice" || u.Email != "alice@example.com" {
		t.Errorf("got %+v, want {user-1, Alice, alice@example.com}", u)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestGetNexusUser_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM nexus_user`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	s := New(mock)
	_, err := s.GetNexusUser(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetNexusUser_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("db error")
	mock.ExpectQuery(`FROM nexus_user`).
		WithArgs("u1").
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.GetNexusUser(context.Background(), "u1")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

func TestGetOrganization_HappyPath(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM organization`).
		WithArgs("org-1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).
			AddRow("org-1", "Acme Corp"))

	s := New(mock)
	o, err := s.GetOrganization(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}
	if o.ID != "org-1" || o.Name != "Acme Corp" {
		t.Errorf("got %+v, want {org-1, Acme Corp}", o)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestGetOrganization_NotFound(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`FROM organization`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	s := New(mock)
	_, err := s.GetOrganization(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetOrganization_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	sentinel := errors.New("db error")
	mock.ExpectQuery(`FROM organization`).
		WithArgs("o1").
		WillReturnError(sentinel)

	s := New(mock)
	_, err := s.GetOrganization(context.Background(), "o1")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

// TestDecodeJSONB exercises the package-level decodeJSONB helper.
func TestDecodeJSONB_EmptyInput(t *testing.T) {
	var out map[string]any
	if err := decodeJSONB(nil, &out, "col"); err != nil {
		t.Errorf("empty input should return nil, got %v", err)
	}
	if out != nil {
		t.Errorf("expected nil map after empty decodeJSONB")
	}
}

func TestDecodeJSONB_ValidJSON(t *testing.T) {
	var out map[string]any
	if err := decodeJSONB([]byte(`{"key":"val"}`), &out, "col"); err != nil {
		t.Errorf("valid JSON decode: %v", err)
	}
	if out["key"] != "val" {
		t.Errorf("decoded value = %v, want val", out["key"])
	}
}

func TestDecodeJSONB_InvalidJSON(t *testing.T) {
	var out map[string]any
	err := decodeJSONB([]byte(`{not json}`), &out, "col")
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}
