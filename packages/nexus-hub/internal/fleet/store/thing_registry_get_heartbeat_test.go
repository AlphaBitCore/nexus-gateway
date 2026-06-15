package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

var getThingCols = []string{
	"id", "type", "name", "version", "address",
	"enrolled_by", "auth_type", "conn_protocol",
	"status", "desired", "reported", "desired_ver", "reported_ver",
	"metadata", "last_seen_at", "enrolled_at",
	"reported_outcomes", "process_started_at",
	"hostname", "primary_ip", "os", "os_version", "physical_id",
	"u_id", "u_displayName", "u_email", "metrics_url",
}

// TestGetThing covers happy / not-found / generic err wrap +
// JSONB decode error path (one of four decodeJSONB calls — malformed
// `desired` blob).
func TestGetThing(t *testing.T) {
	t.Run("happy with all fields populated", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		now := time.Now().UTC()
		mock.ExpectQuery(`FROM thing t\s+LEFT JOIN "DeviceAssignment"`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows(getThingCols).AddRow(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http",
				"online",
				[]byte(`{"k":"v"}`), []byte(`{"x":"y"}`),
				int64(3), int64(2),
				[]byte(`{"label":"prod"}`), &now, now,
				[]byte(`{}`), &now,
				"host-1", "10.0.0.1", "darwin", "14.0", "phys-1",
				"u-1", "Alice", "alice@example.com", "http://metrics",
			))

		store := New(mock)
		got, err := store.GetThing(context.Background(), "thing-1")
		if err != nil {
			t.Fatalf("GetThing: %v", err)
		}
		if got.ID != "thing-1" || got.BoundUserID != "u-1" || got.MetricsURL != "http://metrics" {
			t.Errorf("thing: %+v", got)
		}
		// JSONB decode threaded into typed maps.
		if got.Desired["k"] != "v" || got.Reported["x"] != "y" {
			t.Errorf("decoded: desired=%+v reported=%+v", got.Desired, got.Reported)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("missing").
			WillReturnError(pgx.ErrNoRows)
		store := New(mock)
		_, err := store.GetThing(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("generic err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("x").
			WillReturnError(want)
		store := New(mock)
		_, err := store.GetThing(context.Background(), "x")
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "get thing") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("malformed desired JSONB surfaces decode err", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		now := time.Now().UTC()
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows(getThingCols).AddRow(
				"thing-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http",
				"online",
				[]byte("not json"), // malformed desired
				[]byte(`{}`), int64(1), int64(1),
				[]byte(`{}`), &now, now,
				[]byte(`{}`), (*time.Time)(nil),
				"", "", "", "", "",
				"", "", "", "",
			))

		store := New(mock)
		_, err := store.GetThing(context.Background(), "thing-1")
		if err == nil {
			t.Fatal("expected decode err")
		}
		if !strings.Contains(err.Error(), "decode desired") {
			t.Errorf("missing decode-desired prefix: %v", err)
		}
	})
}

// TestHeartbeat covers happy desired-changed path + happy versions-
// match (no desired returned) + not-found + exec err wrap +
// staticInfo column extraction.
func TestHeartbeat(t *testing.T) {
	t.Run("happy with version mismatch returns desired", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`UPDATE thing\s+SET status\s+=`).
			WithArgs("thing-1", "online",
				pgxmock.AnyArg(), // metaJSON
				"host-extracted", // hostname from staticInfo
				"10.0.0.1",       // primaryIp
				"darwin",         // os
				"14.0",           // osVersion
			).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver", "desired"}).
				AddRow(int64(5), []byte(`{"k":"v"}`)),
			)

		store := New(mock)
		got, err := store.Heartbeat(context.Background(), "thing-1", "online",
			map[string]any{"staticInfo": map[string]any{
				"hostname":  "host-extracted",
				"primaryIp": "10.0.0.1",
				"os":        "darwin",
				"osVersion": "14.0",
			}}, 2) // reportedVer=2, desiredVer=5 → mismatch → fetch desired
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if got.DesiredVer != 5 {
			t.Errorf("desiredVer: got %d, want 5", got.DesiredVer)
		}
		if got.Desired["k"] != "v" {
			t.Errorf("Desired not decoded: %+v", got.Desired)
		}
	})
	t.Run("versions match → no desired returned", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`UPDATE thing`).
			WithArgs("thing-1", "online",
				pgxmock.AnyArg(), nil, nil, nil, nil, // no staticInfo
			).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver", "desired"}).
				AddRow(int64(7), []byte(`{"k":"v"}`)),
			)

		store := New(mock)
		got, err := store.Heartbeat(context.Background(), "thing-1", "online", nil, 7)
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if got.DesiredVer != 7 {
			t.Errorf("desiredVer: %d", got.DesiredVer)
		}
		// versions match → Desired left as zero map (decode skipped).
		if got.Desired != nil {
			t.Errorf("Desired should NOT be decoded when versions match; got: %+v", got.Desired)
		}
	})
	t.Run("not found", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`UPDATE thing`).
			WithArgs("missing", "online", pgxmock.AnyArg(),
				nil, nil, nil, nil).
			WillReturnError(pgx.ErrNoRows)
		store := New(mock)
		_, err := store.Heartbeat(context.Background(), "missing", "online", nil, 0)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound; got: %v", err)
		}
	})
	t.Run("exec err wraps", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		want := errors.New("planner err")
		mock.ExpectQuery(`UPDATE thing`).
			WithArgs("thing-1", "online", pgxmock.AnyArg(),
				nil, nil, nil, nil).
			WillReturnError(want)
		store := New(mock)
		_, err := store.Heartbeat(context.Background(), "thing-1", "online", nil, 0)
		if !errors.Is(err, want) {
			t.Errorf("must wrap; got: %v", err)
		}
		if !strings.Contains(err.Error(), "heartbeat") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	t.Run("malformed desired surfaces decode err on mismatch", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`UPDATE thing`).
			WithArgs("thing-1", "online", pgxmock.AnyArg(),
				nil, nil, nil, nil).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver", "desired"}).
				AddRow(int64(5), []byte("not json")),
			)
		store := New(mock)
		_, err := store.Heartbeat(context.Background(), "thing-1", "online", nil, 2)
		if err == nil {
			t.Fatal("expected decode err on mismatch")
		}
		if !strings.Contains(err.Error(), "decode desired") {
			t.Errorf("missing prefix: %v", err)
		}
	})
	// F-0208: the HTTP heartbeat must only promote offline→online and otherwise
	// leave the status untouched — writing $2 unconditionally clobbered
	// drift→online (premature drift clear) and revoked→online (revocation
	// bypass). The SQL-text expectation fails if anyone reverts the gate to the
	// old unconditional `SET status = $2`. The offline→online edge must also
	// re-stamp process_started_at and reset reported_outcomes, mirroring
	// RefreshLiveness.
	t.Run("status gate: only offline→online, drift/revoked untouched", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`SET status\s+= CASE WHEN status = 'offline' THEN \$2 ELSE status END`).
			WithArgs("thing-1", "online", pgxmock.AnyArg(),
				nil, nil, nil, nil).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver", "desired"}).
				AddRow(int64(2), []byte(`{}`)),
			)
		store := New(mock)
		if _, err := store.Heartbeat(context.Background(), "thing-1", "online", nil, 2); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	})
	t.Run("status gate: offline→online edge re-stamps process_started_at + resets outcomes", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		mock.ExpectQuery(`process_started_at = CASE WHEN status = 'offline' THEN NOW\(\) ELSE process_started_at END,\s+reported_outcomes\s+= CASE WHEN status = 'offline' THEN '\{\}'::jsonb ELSE reported_outcomes END`).
			WithArgs("thing-1", "online", pgxmock.AnyArg(),
				nil, nil, nil, nil).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver", "desired"}).
				AddRow(int64(2), []byte(`{}`)),
			)
		store := New(mock)
		if _, err := store.Heartbeat(context.Background(), "thing-1", "online", nil, 2); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	})
}

// Sanity — keep json import live for any future need.
var _ = json.Valid
