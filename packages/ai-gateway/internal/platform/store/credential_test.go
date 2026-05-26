package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// newMockDB returns a pgxmock pool wired into a *DB via NewWithPgxPool.
// Single test-helper shared by every test in this package.
func newMockDB(t *testing.T) (pgxmock.PgxPoolIface, *DB) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	return mock, NewWithPgxPool(mock)
}

var credentialTestColumns = []string{
	"id", "name", "providerId", "encryptedKey", "encryptionIv", "encryptionTag",
	"encryption_key_id", "enabled", "rotationState", "selectionWeight", "status",
	"reliabilityOverrides",
}

func makeCredentialRow(id string) []any {
	return []any{
		id, "cred-name", "prov-1", "enc-key", "iv", "tag",
		"v1", true, "none", 100, "active",
		json.RawMessage(`{}`),
	}
}

func TestGetCredentialByID(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Credential" WHERE id = \$1`).
			WithArgs("c1").
			WillReturnRows(pgxmock.NewRows(credentialTestColumns).AddRow(makeCredentialRow("c1")...))
		got, err := db.GetCredentialByID(context.Background(), "c1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got == nil || got.ID != "c1" || got.ProviderID != "prov-1" || !got.Enabled {
			t.Errorf("unexpected: %+v", got)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("scan boom")
		mock.ExpectQuery(`FROM "Credential" WHERE id = \$1`).
			WithArgs("x").
			WillReturnError(want)
		_, err := db.GetCredentialByID(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get credential by id") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

func TestGetCredentialForProvider(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Credential"`).
			WithArgs("prov-1").
			WillReturnRows(pgxmock.NewRows(credentialTestColumns).AddRow(makeCredentialRow("c1")...))
		got, err := db.GetCredentialForProvider(context.Background(), "prov-1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got == nil || got.ID != "c1" {
			t.Errorf("unexpected: %+v", got)
		}
	})

	t.Run("err propagates", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Credential"`).
			WithArgs("p").
			WillReturnError(want)
		_, err := db.GetCredentialForProvider(context.Background(), "p")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
	})
}

func TestListEnabledForProvider(t *testing.T) {
	t.Run("happy two rows", func(t *testing.T) {
		mock, db := newMockDB(t)
		mock.ExpectQuery(`FROM "Credential"`).
			WithArgs("p1").
			WillReturnRows(pgxmock.NewRows(credentialTestColumns).
				AddRow(makeCredentialRow("c1")...).
				AddRow(makeCredentialRow("c2")...))
		got, err := db.ListEnabledForProvider(context.Background(), "p1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 2 || got[0].ID != "c1" || got[1].ID != "c2" {
			t.Errorf("unexpected: %+v", got)
		}
	})

	t.Run("query err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM "Credential"`).
			WithArgs("p").
			WillReturnError(want)
		_, err := db.ListEnabledForProvider(context.Background(), "p")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "list enabled credentials for provider") {
			t.Errorf("missing prefix: %v", err)
		}
	})

	t.Run("scan err wraps", func(t *testing.T) {
		mock, db := newMockDB(t)
		// Wrong column count to force scan err.
		mock.ExpectQuery(`FROM "Credential"`).
			WithArgs("p1").
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("c1"))
		_, err := db.ListEnabledForProvider(context.Background(), "p1")
		if err == nil || !strings.Contains(err.Error(), "scan credential") {
			t.Errorf("expected scan err; got: %v", err)
		}
	})
}

// TestListCredentialsForProvider exercises the credentials.Source
// adapter — it just delegates to ListEnabledForProvider.
func TestListCredentialsForProvider(t *testing.T) {
	mock, db := newMockDB(t)
	mock.ExpectQuery(`FROM "Credential"`).
		WithArgs("p1").
		WillReturnRows(pgxmock.NewRows(credentialTestColumns).AddRow(makeCredentialRow("c1")...))
	got, err := db.ListCredentialsForProvider(context.Background(), "p1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c1" {
		t.Errorf("unexpected: %+v", got)
	}
}
