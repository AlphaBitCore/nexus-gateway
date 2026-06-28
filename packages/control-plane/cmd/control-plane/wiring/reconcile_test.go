package wiring

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"testing"

	"github.com/jackc/pgx/v5"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// TestLoadConfigTemplate_RowExists verifies the happy path: a template row is
// found and its JSON state is returned unchanged.
func TestLoadConfigTemplate_RowExists(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	want := json.RawMessage(`{"enabled":true}`)
	mock.ExpectQuery(`SELECT state FROM thing_config_template`).
		WithArgs("compliance-proxy", configkey.Killswitch).
		WillReturnRows(pgxmock.NewRows([]string{"state"}).AddRow(want))

	got, err := loadConfigTemplate(context.Background(), mock, "compliance-proxy", configkey.Killswitch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestLoadConfigTemplate_ErrNoRows returns empty object to keep reconciler safe.
func TestLoadConfigTemplate_ErrNoRows_ReturnsEmptyObject(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT state FROM thing_config_template`).
		WithArgs("agent", configkey.Killswitch).
		WillReturnError(pgx.ErrNoRows)

	got, err := loadConfigTemplate(context.Background(), mock, "agent", configkey.Killswitch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("got %s, want {}", got)
	}
}

// TestLoadConfigTemplate_EmptyState_ReturnsEmptyObject ensures a NULL/empty
// state column is treated as the safe default.
func TestLoadConfigTemplate_EmptyState_ReturnsEmptyObject(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Return an empty JSON blob (zero-length) — treated as no config.
	mock.ExpectQuery(`SELECT state FROM thing_config_template`).
		WithArgs("ai-gateway", configkey.GatewayPassthrough).
		WillReturnRows(pgxmock.NewRows([]string{"state"}).AddRow(json.RawMessage("")))

	got, err := loadConfigTemplate(context.Background(), mock, "ai-gateway", configkey.GatewayPassthrough)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("got %s, want {}", got)
	}
}

// TestLoadConfigTemplate_DBError_ReturnsError verifies real DB errors are
// propagated rather than silently dropped.
func TestLoadConfigTemplate_DBError_ReturnsError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	sentinel := errors.New("db connection reset")
	mock.ExpectQuery(`SELECT state FROM thing_config_template`).
		WithArgs("ai-gateway", configkey.Cache).
		WillReturnError(sentinel)

	_, err = loadConfigTemplate(context.Background(), mock, "ai-gateway", configkey.Cache)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// TestLoadKillswitchTemplate_DelegatesToLoadConfigTemplate verifies that
// loadKillswitchTemplate is a thin wrapper over loadConfigTemplate for the
// Killswitch config key.
func TestLoadKillswitchTemplate_DelegatesToLoadConfigTemplate(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	want := json.RawMessage(`{"enabled":false}`)
	mock.ExpectQuery(`SELECT state FROM thing_config_template`).
		WithArgs("compliance-proxy", configkey.Killswitch).
		WillReturnRows(pgxmock.NewRows([]string{"state"}).AddRow(want))

	got, err := loadKillswitchTemplate(context.Background(), mock, "compliance-proxy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestInitReconciler_NilDB_DoesNothing ensures passing nil db is a safe no-op.
func TestInitReconciler_NilDB_DoesNothing(t *testing.T) {
	// Must not panic.
	InitReconciler(context.Background(), nil, nil, silentLogger())
}

// TestInitReconciler_WithDB_StartsLoop verifies that when db is non-nil the
// reconciler goroutine is started without panicking. We cancel the context
// immediately to stop the loop cleanly.
func TestInitReconciler_WithDB_StartsLoop(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	db := store.NewWithPgxPool(mock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the goroutine exits quickly

	// Must not panic even when context is already cancelled.
	InitReconciler(ctx, db, nil, silentLogger())
}
