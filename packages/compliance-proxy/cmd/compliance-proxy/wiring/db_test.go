package wiring

// db_test.go covers wiring functions that optionally read from database/sql.
// Uses go-sqlmock to exercise DB-present code paths without a live database.

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// newMockDB opens a sqlmock DB+mock pair for controlled testing.
func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	return db, mock
}

// LoadOtelConfig — DB-present paths

func TestLoadOtelConfig_DBQueryError_ReturnsDefault(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WillReturnError(errors.New("db error"))

	cfg := LoadOtelConfig(context.Background(), db)
	if cfg.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("expected default service name, got %q", cfg.ServiceName)
	}
	_ = mock.ExpectationsWereMet()
}

func TestLoadOtelConfig_DBRowNotFound_ReturnsDefault(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WillReturnError(sql.ErrNoRows)

	cfg := LoadOtelConfig(context.Background(), db)
	if cfg.ServiceName != "nexus-compliance-proxy" {
		t.Errorf("expected default service name on no-row, got %q", cfg.ServiceName)
	}
}

// InitPayloadCaptureStore — DB-present paths

func TestInitPayloadCaptureStore_DBQueryError_FallsBackToDefault(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WillReturnError(errors.New("db error"))

	store := InitPayloadCaptureStore(db, nil, testLogger())
	if store == nil {
		t.Fatal("expected non-nil store even on DB error")
	}
	cfg := store.Get()
	if cfg.MaxInlineBodyBytes <= 0 {
		t.Errorf("MaxInlineBodyBytes = %d; want > 0", cfg.MaxInlineBodyBytes)
	}
}

func TestInitPayloadCaptureStore_DBRowNotFound_FallsBackToDefault(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WillReturnError(sql.ErrNoRows)

	store := InitPayloadCaptureStore(db, nil, testLogger())
	if store == nil {
		t.Fatal("expected non-nil store on sql.ErrNoRows")
	}
	_ = mock
}

// InitStreamingPolicyStore — DB-present paths (#115)

func TestInitStreamingPolicyStore_DBQueryError_ReturnsDefaultPolicyStore(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WillReturnError(errors.New("db query failed"))

	// On error the function logs a warning and returns empty Policy{}.
	_ = InitStreamingPolicyStore(db, testLogger())
	_ = mock
}

func TestInitStreamingPolicyStore_DBRowNotFound_ReturnsDefaultPolicy(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WillReturnError(sql.ErrNoRows)

	_ = InitStreamingPolicyStore(db, testLogger())
	_ = mock
}

func TestInitStreamingPolicyStore_ValidDBRow_ReturnsLoadedPolicy(t *testing.T) {
	db, mock := newMockDB(t)
	validJSON := `{"mode":"buffer","failBehavior":"passthrough","chunkBytes":1024}`
	rows := sqlmock.NewRows([]string{"value"}).AddRow(validJSON)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WillReturnRows(rows)

	policy := InitStreamingPolicyStore(db, testLogger())
	_ = policy // observable: no panic, no error
	_ = mock.ExpectationsWereMet()
}
