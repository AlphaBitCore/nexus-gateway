package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

var listThingsCols = []string{
	"id", "type", "name", "version", "address",
	"enrolled_by", "auth_type", "conn_protocol",
	"status", "desired", "reported", "desired_ver", "reported_ver",
	"metadata", "last_seen_at", "enrolled_at",
	"reported_outcomes", "process_started_at",
	"hostname", "primary_ip", "os", "os_version", "physical_id",
	"bound_user_id", "bound_user_display_name", "bound_user_email",
	"override_count", "override_stale_count", "has_killswitch_bypass",
}

// TestListThings_FilterBranches covers every WHERE-clause filter in
// ListThings: Type, Status, Search, HasOverrides=true,
// HasOverrides=false. Each filter increments argIdx; the COUNT
// query + page query must see the same arg slot order so this
// table pins the SQL builder.
func TestListThings_FilterBranches(t *testing.T) {
	t.Run("all filters set", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		hasOverrides := true
		// COUNT query with type + status + search + hasOverrides → 3 args.
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
			WithArgs("agent", "online", "%api%").
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
		// Page query gets the same 3 args + LIMIT + OFFSET.
		mock.ExpectQuery(`FROM thing_with_overrides`).
			WithArgs("agent", "online", "%api%", 50, 0).
			WillReturnRows(pgxmock.NewRows(listThingsCols))

		store := New(mock)
		_, err := store.ListThings(context.Background(), ListThingsParams{
			Type: "agent", Status: "online", Search: "api",
			HasOverrides: &hasOverrides, Page: 1, PageSize: 50,
		})
		if err != nil {
			t.Fatalf("ListThings: %v", err)
		}
	})
	t.Run("HasOverrides=false filter", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		hasOverrides := false
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(`FROM thing_with_overrides`).
			WithArgs(50, 0).
			WillReturnRows(pgxmock.NewRows(listThingsCols))
		store := New(mock)
		_, err := store.ListThings(context.Background(), ListThingsParams{
			HasOverrides: &hasOverrides,
		})
		if err != nil {
			t.Fatalf("ListThings: %v", err)
		}
	})
	t.Run("happy single-row decode", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
		now := time.Now().UTC()
		mock.ExpectQuery(`FROM thing_with_overrides`).
			WithArgs(50, 0).
			WillReturnRows(pgxmock.NewRows(listThingsCols).AddRow(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http",
				"online",
				[]byte(`{"k":"v"}`), []byte(`{}`),
				int64(0), int64(0),
				[]byte(`{}`), &now, now,
				[]byte(`{}`), (*time.Time)(nil),
				"host-1", "10.0.0.1", "darwin", "14.0", "phys-1",
				"u-1", "Alice", "alice@example.com",
				int64(3), int64(1), true,
			))
		store := New(mock)
		got, err := store.ListThings(context.Background(), ListThingsParams{})
		if err != nil {
			t.Fatalf("ListThings: %v", err)
		}
		if got.Total != 1 || len(got.Things) != 1 {
			t.Errorf("got: total=%d, len=%d", got.Total, len(got.Things))
		}
		thing := got.Things[0]
		if thing.OverrideCount != 3 || !thing.HasKillswitchBypass {
			t.Errorf("override aggs not threaded: %+v", thing)
		}
	})
	t.Run("count err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
			WillReturnError(want)
		store := New(mock)
		_, err := store.ListThings(context.Background(), ListThingsParams{})
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "count things") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("page-query err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(0))
		want := errors.New("timeout")
		mock.ExpectQuery(`FROM thing_with_overrides`).
			WithArgs(50, 0).
			WillReturnError(want)
		store := New(mock)
		_, err := store.ListThings(context.Background(), ListThingsParams{})
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "list things") {
			t.Errorf("missing prefix: %v", err)
		}
	})
}

// TestListThings_DecodeBranches covers the 4 decodeJSONB error
// branches in ListThings — same column-by-column pattern as GetThing.
func TestListThings_DecodeBranches(t *testing.T) {
	cases := []struct {
		col  string
		want string
	}{
		{"desired", "decode desired"},
		{"reported", "decode reported"},
		{"metadata", "decode metadata"},
		{"reported_outcomes", "decode reported_outcomes"},
	}
	for _, tc := range cases {
		t.Run(tc.col, func(t *testing.T) {
			mock, _ := pgxmock.NewPool()
			defer mock.Close()
			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
				WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(1))
			now := time.Now().UTC()
			row := []any{
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http",
				"online",
				[]byte(`{}`), []byte(`{}`),
				int64(0), int64(0),
				[]byte(`{}`), &now, now,
				[]byte(`{}`), (*time.Time)(nil),
				"", "", "", "", "",
				"", "", "",
				int64(0), int64(0), false,
			}
			idx := map[string]int{
				"desired": 9, "reported": 10, "metadata": 13, "reported_outcomes": 16,
			}[tc.col]
			row[idx] = []byte("not json")
			mock.ExpectQuery(`FROM thing_with_overrides`).
				WithArgs(50, 0).
				WillReturnRows(pgxmock.NewRows(listThingsCols).AddRow(row...))
			store := New(mock)
			_, err := store.ListThings(context.Background(), ListThingsParams{})
			if err == nil {
				t.Fatalf("expected decode err for %q", tc.col)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("missing prefix %q: %v", tc.want, err)
			}
		})
	}
}
