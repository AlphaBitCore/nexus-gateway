package overrides

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// TestNormalizeOverrideListFilter pins the clamping ranges for
// pagination: Limit < 1 → 50, Limit > 500 → 500, Offset < 0 → 0.
func TestNormalizeOverrideListFilter(t *testing.T) {
	cases := []struct {
		name   string
		in     ListOverridesFilter
		wantL  int
		wantOf int
	}{
		{"zero Limit defaults to 50", ListOverridesFilter{}, 50, 0},
		{"negative Limit defaults to 50", ListOverridesFilter{Limit: -5}, 50, 0},
		{"Limit over 500 clamps", ListOverridesFilter{Limit: 1000}, 500, 0},
		{"valid Limit kept", ListOverridesFilter{Limit: 75}, 75, 0},
		{"negative Offset clamps to 0", ListOverridesFilter{Limit: 10, Offset: -3}, 10, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.in
			normalizeOverrideListFilter(&f)
			if f.Limit != tc.wantL || f.Offset != tc.wantOf {
				t.Errorf("got L=%d O=%d, want L=%d O=%d", f.Limit, f.Offset, tc.wantL, tc.wantOf)
			}
		})
	}
}

// TestGetOverride_GenericErrorWraps covers the generic-err wrap
// branch in GetOverride (not the ErrNoRows path which is already
// tested as TestStore_GetOverride_NotFound).
func TestGetOverride_GenericErrorWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("connection refused")
	mock.ExpectQuery(`FROM thing_config_override`).
		WithArgs("thing-1", "key").
		WillReturnError(want)
	store := New(mock)
	_, err := store.GetOverride(context.Background(), "thing-1", "key")
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "get override thing-1/key") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestUpsertOverride_EmptyStateRejected covers the
// `len(stateBytes) == 0` guard — the spec says state cannot be
// empty; the per-method guard is the last line of defense after
// NewOverrideState's validation.
func TestUpsertOverride_EmptyStateRejected(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	mock.ExpectRollback()
	tx, _ := mock.Begin(context.Background())
	store := New(mock)
	err := store.UpsertOverride(context.Background(), tx, ThingConfigOverride{
		ThingID: "thing-1", ConfigKey: "key",
		// State is the zero value → Bytes() returns nil → empty.
	})
	if err == nil {
		t.Fatal("expected empty-state err")
	}
	if !strings.Contains(err.Error(), "state is empty") {
		t.Errorf("missing prefix: %v", err)
	}
	_ = tx.Rollback(context.Background())
}

// TestUpsertOverride_ExecErrorWraps covers the exec err wrap.
func TestUpsertOverride_ExecErrorWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	want := errors.New("constraint violation")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs("thing-1", "key", pgxmock.AnyArg(), int64(1),
			"actor", pgxmock.AnyArg(), pgxmock.AnyArg(), false).
		WillReturnError(want)
	mock.ExpectRollback()

	state, err := NewOverrideState([]byte(`{"x":"y"}`))
	if err != nil {
		t.Fatal(err)
	}

	tx, _ := mock.Begin(context.Background())
	store := New(mock)
	reason := "reason"
	err = store.UpsertOverride(context.Background(), tx, ThingConfigOverride{
		ThingID: "thing-1", ConfigKey: "key", State: state,
		TemplateVerAtSet: 1, SetBy: "actor", Reason: &reason,
	})
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "upsert override thing-1/key") {
		t.Errorf("missing prefix: %v", err)
	}
	_ = tx.Rollback(context.Background())
}

// TestDeleteOverride_ExecErrorWraps.
func TestDeleteOverride_ExecErrorWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectBegin()
	want := errors.New("planner err")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("thing-1", "key").
		WillReturnError(want)
	mock.ExpectRollback()
	tx, _ := mock.Begin(context.Background())
	store := New(mock)
	_, err := store.DeleteOverride(context.Background(), tx, "thing-1", "key")
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "delete override thing-1/key") {
		t.Errorf("missing prefix: %v", err)
	}
	_ = tx.Rollback(context.Background())
}

// TestListOverridesByThing_QueryErrorWraps.
func TestListOverridesByThing_QueryErrorWraps(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("planner err")
	mock.ExpectQuery(`FROM thing_config_override tco`).
		WithArgs("thing-1").
		WillReturnError(want)
	store := New(mock)
	_, err := store.ListOverridesByThing(context.Background(), "thing-1")
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
	if !strings.Contains(err.Error(), "list overrides by thing thing-1") {
		t.Errorf("missing prefix: %v", err)
	}
}

// TestListExpiredOverrides_QueryError covers the err path.
func TestListExpiredOverrides_QueryError(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	want := errors.New("planner err")
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE expires_at IS NOT NULL`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(want)
	store := New(mock)
	_, err := store.ListExpiredOverrides(context.Background(), time.Now())
	if !errors.Is(err, want) {
		t.Errorf("must wrap; got: %v", err)
	}
}
