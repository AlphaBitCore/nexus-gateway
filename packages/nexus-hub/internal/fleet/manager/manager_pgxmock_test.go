package manager

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// manager_pgxmock_test.go drives Manager DB-bound flows through pgxmock + the
// PgxPool seam (Manager.pool / Store.NewWithPgxPool). No real Postgres / Redis
// is touched. Per binding [[tests-only-own-data]] the tests own zero real rows.
//
// The structure mirrors store-level pgxmock tests: each top-level Test* exercises
// one Manager method, with t.Run() subtests covering the named behavior branches
// (happy / not-found / DB-err / wire-shape).

// silentLogger returns a *slog.Logger that drops every record. Used by every
// test below — none of them assert on log output, and slog.Default()'s text
// handler would otherwise spam stderr during go test runs.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newPgxmockManager wires a Manager with a pgxmock-backed store + injected
// tx pool. The same mock instance is used for non-tx store calls (via
// store.NewWithPgxPool) AND tx-bound Manager calls (via the txPool seam),
// so a single ExpectXxx chain on the returned mock covers both paths.
func newPgxmockManager(t *testing.T) (*Manager, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-test", silentLogger())
	return mgr, mock
}

// getThingCols mirrors the column list returned by store.GetThing's SELECT
// (thing_registry.go:455). Tests that need GetThing to succeed assemble a
// pgxmock.Rows with this column order.
var getThingMgrCols = []string{
	"id", "type", "name", "version", "address",
	"enrolled_by", "auth_type", "conn_protocol",
	"status", "desired", "reported", "desired_ver", "reported_ver",
	"metadata", "last_seen_at", "enrolled_at",
	"reported_outcomes", "process_started_at",
	"hostname", "primary_ip", "os", "os_version", "physical_id",
	"u_id", "u_displayName", "u_email", "metrics_url",
}

// minimalGetThingRow returns a pgxmock.Rows representing one Thing with the
// supplied id/type and an arbitrary desired map. The remaining columns get
// neutral defaults so the row scans cleanly through store.decodeJSONB.
func minimalGetThingRow(id, ttype string, desired map[string]any, desiredVer int64) *pgxmock.Rows {
	now := time.Now().UTC()
	desiredJSON, _ := json.Marshal(desired)
	return pgxmock.NewRows(getThingMgrCols).AddRow(
		id, ttype, id, "1.0", "addr",
		"sso", "bearer", "http",
		"online",
		desiredJSON, []byte(`{}`),
		desiredVer, int64(0),
		[]byte(`{}`), &now, now,
		[]byte(`{}`), &now,
		"host-1", "10.0.0.1", "darwin", "14.0", "",
		"", "", "", "",
	)
}

func TestManager_MarkOffline(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET status = 'offline'`).
			WithArgs("thing-1").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		// MarkOffline returns no error; we assert it does not panic and the
		// expectations are met. Behavior is "best-effort log on store err".
		mgr.MarkOffline(context.Background(), "thing-1")
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
	t.Run("store err is swallowed (warn-only)", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET status = 'offline'`).
			WithArgs("thing-x").
			WillReturnError(errors.New("planner err"))
		// Must not panic and must not propagate — production contract is
		// "log and move on" (WebSocket disconnect path).
		mgr.MarkOffline(context.Background(), "thing-x")
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
}

func TestManager_TouchLiveness(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing\s+SET last_seen_at`).
			WithArgs("thing-1").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		mgr.TouchLiveness(context.Background(), "thing-1")
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
	t.Run("store err is swallowed (debug-only)", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing\s+SET last_seen_at`).
			WithArgs("thing-x").
			WillReturnError(errors.New("planner err"))
		mgr.TouchLiveness(context.Background(), "thing-x")
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
}

func TestManager_Deregister(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE thing SET status = 'offline'`).
			WithArgs("thing-1").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		if err := mgr.Deregister(context.Background(), "thing-1"); err != nil {
			t.Fatalf("Deregister: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
}

func TestManager_GetThingDetail(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("thing-1").
		WillReturnRows(minimalGetThingRow("thing-1", "agent", map[string]any{"k": "v"}, 1))
	got, err := mgr.GetThingDetail(context.Background(), "thing-1")
	if err != nil {
		t.Fatalf("GetThingDetail: %v", err)
	}
	if got.ID != "thing-1" {
		t.Errorf("ID = %q, want thing-1", got.ID)
	}
}

func TestManager_GetThingManagementURL(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		// store.GetThingManagementURL SELECT — match by the LEFT JOIN shape.
		mgmtURL := "http://10.0.0.1/manage"
		mock.ExpectQuery(`FROM thing t\s+LEFT JOIN thing_service`).
			WithArgs("thing-1").
			WillReturnRows(pgxmock.NewRows([]string{"management_url"}).AddRow(&mgmtURL))
		url, err := mgr.GetThingManagementURL(context.Background(), "thing-1")
		if err != nil {
			t.Fatalf("GetThingManagementURL: %v", err)
		}
		if url != "http://10.0.0.1/manage" {
			t.Errorf("url = %q", url)
		}
	})
}

func TestManager_ListThings(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	// Count query first.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM thing_with_overrides`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(2))
	// List query second. We construct two rows with the exact 29-column shape
	// of ListThings' SELECT (ThingWithOverrideAgg).
	now := time.Now().UTC()
	listCols := []string{
		"id", "type", "name", "version", "address",
		"enrolled_by", "auth_type", "conn_protocol",
		"status", "desired", "reported", "desired_ver", "reported_ver",
		"metadata", "last_seen_at", "enrolled_at",
		"reported_outcomes", "process_started_at",
		"hostname", "primary_ip", "os", "os_version", "physical_id",
		"bound_user_id", "bound_user_display_name", "bound_user_email",
		"override_count", "override_stale_count", "has_killswitch_bypass",
	}
	rows := pgxmock.NewRows(listCols).
		AddRow(
			"t-1", "agent", "host-1", "1.0", "addr",
			"sso", "bearer", "http",
			"online", []byte(`{}`), []byte(`{}`), int64(1), int64(1),
			[]byte(`{}`), &now, now,
			[]byte(`{}`), &now,
			"", "", "", "", "",
			"", "", "",
			int64(0), int64(0), false,
		).
		AddRow(
			"t-2", "agent", "host-2", "1.0", "addr",
			"sso", "bearer", "http",
			"online", []byte(`{}`), []byte(`{}`), int64(2), int64(2),
			[]byte(`{}`), &now, now,
			[]byte(`{}`), &now,
			"", "", "", "", "",
			"", "", "",
			int64(1), int64(0), false,
		)
	mock.ExpectQuery(`FROM thing_with_overrides`).
		WithArgs("agent", 50, 0).
		WillReturnRows(rows)

	res, err := mgr.ListThings(context.Background(), store.ListThingsParams{Type: "agent"})
	if err != nil {
		t.Fatalf("ListThings: %v", err)
	}
	if res.Total != 2 || len(res.Things) != 2 {
		t.Errorf("Total=%d, len=%d, want 2 each", res.Total, len(res.Things))
	}
}

func TestManager_GetDriftedThings(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	now := time.Now().UTC()
	// ListDriftedThings now selects 7 columns: id, type, status, desired_ver,
	// reported_ver, last_seen_at, out_of_sync_keys (text[]).
	mock.ExpectQuery(`FROM thing\s+WHERE status = 'drift'`).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "type", "status", "desired_ver", "reported_ver", "last_seen_at", "out_of_sync_keys",
		}).AddRow("t-1", "agent", "drift", int64(5), int64(3), &now, []string{"hooks", "routing_rules"}))
	got, err := mgr.GetDriftedThings(context.Background())
	if err != nil {
		t.Fatalf("GetDriftedThings: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t-1" {
		t.Errorf("got: %+v", got)
	}
	if len(got[0].OutOfSyncKeys) != 2 || got[0].OutOfSyncKeys[0] != "hooks" {
		t.Errorf("OutOfSyncKeys = %v, want [hooks routing_rules]", got[0].OutOfSyncKeys)
	}
}

// TestManager_HandleHeartbeat covers the happy path (UPDATE thing → RETURNING)
// plus the wrap-on-err path. The goroutines spawned for trust_level /
// device-assignment IP refresh hold their own mock expectations behind a
// dedicated mock — testing them is split into the direct refresh / updateTrustLevel
// tests below.
func TestManager_HandleHeartbeat(t *testing.T) {
	t.Run("happy returns desired when behind", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		// No tx pool injection — HandleHeartbeat does not Begin a tx.
		mgr := New(st, nil, nil, nil, "hub-test", silentLogger())

		// Heartbeat UPDATE; first row response carries new desired_ver + desired.
		mock.ExpectQuery(`UPDATE thing\s+SET status\s*= CASE`).
			WithArgs("thing-1", "online", pgxmock.AnyArg(), nil, nil, nil, nil).
			WillReturnRows(pgxmock.NewRows([]string{"desired_ver", "desired"}).
				AddRow(int64(5), []byte(`{"hooks":{"e":true}}`)))
		// The trust_level goroutine fires immediately — pre-program a "no thing_agent"
		// path so it ends quickly without touching unrelated tables. Subsequent
		// calls happen on a brand-new goroutine that the test isn't synchronised
		// against; use pgxmock's expect-and-allow-extra contract by attaching a
		// matching expectation for the GetThingAgentForTrustLevel SELECT that
		// returns ErrNoRows.
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("thing-1").
			WillReturnError(pgx.ErrNoRows)

		res, err := mgr.HandleHeartbeat(context.Background(), HeartbeatRequest{
			ID: "thing-1", Status: "online", ReportedVer: 4,
		})
		if err != nil {
			t.Fatalf("HandleHeartbeat: %v", err)
		}
		if !res.Ack || res.DesiredVer != 5 {
			t.Errorf("res = %+v, want Ack=true DesiredVer=5", res)
		}
		if res.Desired["hooks"] == nil {
			t.Errorf("Desired missing hooks; got: %v", res.Desired)
		}
		// Let the trust_level goroutine drain so pgxmock expectations stick.
		time.Sleep(50 * time.Millisecond)
	})
	t.Run("not found surfaces wrapped error", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := New(st, nil, nil, nil, "hub-test", silentLogger())

		mock.ExpectQuery(`UPDATE thing\s+SET status\s*= CASE`).
			WithArgs("missing", "online", pgxmock.AnyArg(), nil, nil, nil, nil).
			WillReturnError(pgx.ErrNoRows)
		_, err := mgr.HandleHeartbeat(context.Background(), HeartbeatRequest{ID: "missing", Status: "online"})
		if err == nil {
			t.Fatal("expected error for not-found")
		}
		if !strings.Contains(err.Error(), "heartbeat") {
			t.Errorf("err lacks wrap context: %v", err)
		}
	})
}

func TestManager_RefreshDeviceAssignmentIP(t *testing.T) {
	t.Run("changed=true logs debug", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE "DeviceAssignment"\s+SET ip_address`).
			WithArgs("thing-1", "10.0.0.2").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		mgr.refreshDeviceAssignmentIP(context.Background(), "thing-1", "10.0.0.2")
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
	t.Run("no-op when ip already current (changed=false)", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE "DeviceAssignment"\s+SET ip_address`).
			WithArgs("thing-1", "10.0.0.2").
			WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
		mgr.refreshDeviceAssignmentIP(context.Background(), "thing-1", "10.0.0.2")
	})
	t.Run("store err logs warn (does not panic)", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectExec(`UPDATE "DeviceAssignment"\s+SET ip_address`).
			WithArgs("thing-1", "10.0.0.2").
			WillReturnError(errors.New("planner err"))
		mgr.refreshDeviceAssignmentIP(context.Background(), "thing-1", "10.0.0.2")
	})
}

func TestManager_UpdateTrustLevel(t *testing.T) {
	t.Run("non-agent: skip silently on ErrNotFound", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("svc-1").
			WillReturnError(pgx.ErrNoRows)
		mgr.updateTrustLevel(context.Background(), "svc-1", "online", "")
	})
	t.Run("agent: full compute writes trust_level and caches shadow", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		// thing_agent SELECT.
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"thing_id", "version", "cert_expires_at"}).
				AddRow("agent-1", "1.2.3", (*time.Time)(nil)))
		// HasActiveDeviceAssignment EXISTS.
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		// UPDATE thing_agent trust_level.
		mock.ExpectExec(`UPDATE thing_agent SET trust_level`).
			WithArgs("agent-1", 3). // hasAssignment=true + minVersion="" → 3
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		mgr.updateTrustLevel(context.Background(), "agent-1", "online", "")
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
	t.Run("HasActiveDeviceAssignment err swallowed", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"thing_id", "version", "cert_expires_at"}).
				AddRow("agent-1", "1.2.3", (*time.Time)(nil)))
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("agent-1").
			WillReturnError(errors.New("planner err"))
		mgr.updateTrustLevel(context.Background(), "agent-1", "online", "")
	})
	t.Run("GetThingAgent generic err swallowed (warn-only)", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-x").
			WillReturnError(errors.New("planner err"))
		mgr.updateTrustLevel(context.Background(), "agent-x", "online", "")
	})
	t.Run("update trust_level err swallowed (warn-only)", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"thing_id", "version", "cert_expires_at"}).
				AddRow("agent-1", "1.2.3", (*time.Time)(nil)))
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		mock.ExpectExec(`UPDATE thing_agent SET trust_level`).
			WithArgs("agent-1", 3).
			WillReturnError(errors.New("planner err"))
		mgr.updateTrustLevel(context.Background(), "agent-1", "online", "")
	})
}

func TestManager_ComputeAndStoreTrustLevel(t *testing.T) {
	t.Run("non-agent returns 0 on ErrNotFound", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("svc-1").
			WillReturnError(pgx.ErrNoRows)
		level := mgr.ComputeAndStoreTrustLevel(context.Background(), "svc-1", "online", "")
		if level != 0 {
			t.Errorf("level = %d, want 0", level)
		}
	})
	t.Run("agent happy: returns computed level", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"thing_id", "version", "cert_expires_at"}).
				AddRow("agent-1", "1.2.3", (*time.Time)(nil)))
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
		mock.ExpectExec(`UPDATE thing_agent SET trust_level`).
			WithArgs("agent-1", 3).
			WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
		level := mgr.ComputeAndStoreTrustLevel(context.Background(), "agent-1", "online", "")
		if level != 3 {
			t.Errorf("level = %d, want 3", level)
		}
	})
	t.Run("GetThingAgent generic err returns 0", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-x").
			WillReturnError(errors.New("planner err"))
		level := mgr.ComputeAndStoreTrustLevel(context.Background(), "agent-x", "online", "")
		if level != 0 {
			t.Errorf("level = %d, want 0 on err", level)
		}
	})
	t.Run("HasActiveDeviceAssignment err returns 0", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"thing_id", "version", "cert_expires_at"}).
				AddRow("agent-1", "1.2.3", (*time.Time)(nil)))
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("agent-1").
			WillReturnError(errors.New("planner err"))
		level := mgr.ComputeAndStoreTrustLevel(context.Background(), "agent-1", "online", "")
		if level != 0 {
			t.Errorf("level = %d, want 0 on assignment err", level)
		}
	})
	t.Run("update trust_level err still returns computed level", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing_agent ta\s+JOIN thing t`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"thing_id", "version", "cert_expires_at"}).
				AddRow("agent-1", "1.2.3", (*time.Time)(nil)))
		mock.ExpectQuery(`SELECT EXISTS`).
			WithArgs("agent-1").
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectExec(`UPDATE thing_agent SET trust_level`).
			WithArgs("agent-1", 1). // no assignment → 1
			WillReturnError(errors.New("planner err"))
		level := mgr.ComputeAndStoreTrustLevel(context.Background(), "agent-1", "online", "")
		if level != 1 {
			t.Errorf("level = %d, want 1 (computed even when UPDATE failed)", level)
		}
	})
}

// TestManager_RegisterThing_FirstTime exercises the ErrNotFound branch on
// TouchThingSession that flips to the enrollment UPSERT.
func TestManager_RegisterThing_FirstTime(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()

	// GetConfigTemplates returns one template.
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":true}`), int64(3), now, "alice"))
	// TouchThingSession returns rows-affected=0 → ErrNotFound.
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("a-1", "1.0", "addr", "host-a").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))
	// Enrollment UPSERT (UpsertThingEnrollmentWithDesiredVer) takes 13 args.
	// arg 3 = p.Name = "host-a" (request set it directly).
	mock.ExpectExec(`INSERT INTO thing\s*\(`).
		WithArgs(
			"a-1", "agent", "host-a", "1.0", "addr",
			"", "bearer", "http", "online",
			pgxmock.AnyArg(), // metadata JSON
			pgxmock.AnyArg(), // desired JSON
			int64(3),         // desired_ver
			"phys-1",
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// UpsertThingService for non-agent? No — type is "agent" so it skips.
	// GetThing → return desired with the same template-shape.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("a-1").
		WillReturnRows(minimalGetThingRow("a-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 3))

	resp, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "a-1", Type: "agent", Name: "host-a", Version: "1.0", Address: "addr", PhysicalID: "phys-1",
	})
	if err != nil {
		t.Fatalf("RegisterThing: %v", err)
	}
	if resp.DesiredVer != 3 {
		t.Errorf("DesiredVer = %d, want 3", resp.DesiredVer)
	}
	if _, ok := resp.Desired["hooks"]; !ok {
		t.Errorf("Desired missing hooks; got: %v", resp.Desired)
	}
	// thingclient contract: each entry is {state, version}.
	hooksEntry, ok := resp.Desired["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks entry not a map: %T", resp.Desired["hooks"])
	}
	if hooksEntry["version"].(int64) != 3 {
		t.Errorf("hooks.version = %v, want 3", hooksEntry["version"])
	}
}

// TestManager_RegisterThing_TouchOK exercises the touch-succeeds path that
// reuses the existing thing row + skips the enrollment UPSERT.
func TestManager_RegisterThing_TouchOK(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("ai-gateway").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("ai-gateway", "routing", []byte(`{"r":1}`), int64(7), now, "alice"))
	mock.ExpectExec(`UPDATE thing SET\s+version`).
		WithArgs("gw-1", "2.0", "10.1:80", "gw-host").
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// UpsertThingService for non-agent type fires (4 args).
	mock.ExpectExec(`INSERT INTO thing_service`).
		WithArgs("gw-1", "primary", "http://gw-1/metrics", "http://gw-1/manage").
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("gw-1").
		WillReturnRows(minimalGetThingRow("gw-1", "ai-gateway", map[string]any{"routing": map[string]any{"r": 1}}, 7))

	resp, err := mgr.RegisterThing(context.Background(), RegisterRequest{
		ID: "gw-1", Type: "ai-gateway", Name: "gw-host", Version: "2.0", Address: "10.1:80",
		MetricsURL: "http://gw-1/metrics", ManagementURL: "http://gw-1/manage", Role: "primary",
	})
	if err != nil {
		t.Fatalf("RegisterThing: %v", err)
	}
	if resp.DesiredVer != 7 {
		t.Errorf("DesiredVer = %d, want 7", resp.DesiredVer)
	}
}

// TestManager_RegisterThing_TouchErr surfaces wrapped errors from TouchSession.
func TestManager_RegisterThing_TouchErr(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()

	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}))
	mock.ExpectExec(`UPDATE thing SET`).
		WithArgs("a-x", "", "", "").
		WillReturnError(errors.New("planner err"))
	_, err := mgr.RegisterThing(context.Background(), RegisterRequest{ID: "a-x", Type: "agent"})
	if err == nil || !strings.Contains(err.Error(), "touch thing") {
		t.Errorf("err = %v, want touch-thing wrap", err)
	}
}

// TestManager_RegisterThing_TemplatesErr surfaces wrapped errors from GetConfigTemplates.
func TestManager_RegisterThing_TemplatesErr(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectQuery(`FROM thing_config_template`).
		WithArgs("agent").
		WillReturnError(errors.New("planner err"))
	_, err := mgr.RegisterThing(context.Background(), RegisterRequest{ID: "a-1", Type: "agent"})
	if err == nil || !strings.Contains(err.Error(), "load templates") {
		t.Errorf("err = %v, want load-templates wrap", err)
	}
}

func TestManager_HandleShadowReport_Normal(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectExec(`UPDATE thing\s+SET reported`).
		WithArgs("thing-1", []byte(`{"k":"v"}`), int64(3), []byte(`{}`)).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	err := mgr.HandleShadowReport(context.Background(), ShadowReportRequest{
		ID:          "thing-1",
		Reported:    map[string]any{"k": "v"},
		ReportedVer: 3,
	})
	if err != nil {
		t.Fatalf("HandleShadowReport: %v", err)
	}
}

func TestManager_HandleShadowReport_StoreErr(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectExec(`UPDATE thing\s+SET reported`).
		WithArgs("thing-1", pgxmock.AnyArg(), int64(3), pgxmock.AnyArg()).
		WillReturnError(errors.New("planner err"))
	err := mgr.HandleShadowReport(context.Background(), ShadowReportRequest{
		ID: "thing-1", Reported: map[string]any{"k": "v"}, ReportedVer: 3,
	})
	if err == nil || !strings.Contains(err.Error(), "shadow report") {
		t.Errorf("err = %v, want shadow-report wrap", err)
	}
}

// TestManager_HandleShadowReport_BreakGlass_StateMissingKey covers the
// outer break-glass path. break-glass reconciliation is non-fatal: even if
// the inner handler errors, HandleShadowReport returns nil.
func TestManager_HandleShadowReport_BreakGlass_NonFatal(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	// UpdateShadowReport succeeds (4 args: id, reportedJSON, reportedVer, outcomesJSON).
	mock.ExpectExec(`UPDATE thing\s+SET reported`).
		WithArgs("thing-1", pgxmock.AnyArg(), int64(5), []byte(`{}`)).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// handleBreakGlassReport runs but keyVersions is empty → returns
	// "missing keyVersions" error which is logged + swallowed.
	err := mgr.HandleShadowReport(context.Background(), ShadowReportRequest{
		ID:          "thing-1",
		Reported:    map[string]any{"ks": map[string]any{"enabled": true}},
		ReportedVer: 5,
		Reason:      "break_glass",
	})
	// Outer error must be nil — break-glass failures don't fail the report.
	if err != nil {
		t.Errorf("err = %v, want nil (break-glass failures are non-fatal)", err)
	}
}

// TestManager_HandleShadowReport_BreakGlass_DeniedWritesAudit locks the rule that
// a break-glass report carrying a key OUTSIDE the {killswitch, exemptions}
// writable allowlist is rejected, and the rejection must leave a durable
// break_glass_denied config_change_event (so a node probing the privileged
// reconciliation surface is visible to audit/SIEM), while still being non-fatal
// to the shadow report.
func TestManager_HandleShadowReport_BreakGlass_DeniedWritesAudit(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	// 1. UpdateShadowReport succeeds.
	mock.ExpectExec(`UPDATE thing\s+SET reported`).
		WithArgs("thing-1", pgxmock.AnyArg(), int64(7), pgxmock.AnyArg()).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))
	// 2. emitBreakGlassDenied looks up the thing type (best-effort — error is
	//    tolerated, thing_type falls back to "").
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("thing-1").
		WillReturnError(errors.New("lookup skipped"))
	// 3. The denied event is written in its own transaction.
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs("", "credentialKeys", "break_glass_denied", "break-glass:unknown", "break-glass",
			pgxmock.AnyArg(), int64(7), "", false).
		WillReturnResult(pgconn.NewCommandTag("INSERT 1"))
	mock.ExpectCommit()

	err := mgr.HandleShadowReport(context.Background(), ShadowReportRequest{
		ID:          "thing-1",
		Reported:    map[string]any{"credentialKeys": map[string]any{"x": 1}},
		ReportedVer: 7,
		Reason:      "break_glass",
		KeyVersions: map[string]int64{"credentialKeys": 7}, // NOT in the writable allowlist
	})
	if err != nil {
		t.Fatalf("HandleShadowReport: %v (break-glass denial must be non-fatal)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("break_glass_denied audit event not written as expected: %v", err)
	}
}

func TestDecodeOutcomes(t *testing.T) {
	t.Run("empty returns nil", func(t *testing.T) {
		if got := decodeOutcomes(nil); got != nil {
			t.Errorf("nil → %v, want nil", got)
		}
		if got := decodeOutcomes(map[string]json.RawMessage{}); got != nil {
			t.Errorf("empty → %v, want nil", got)
		}
	})
	t.Run("decodes known entries", func(t *testing.T) {
		raw := map[string]json.RawMessage{
			"hooks": json.RawMessage(`{"appliedVersion":3}`),
		}
		got := decodeOutcomes(raw)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		entry := got["hooks"]
		if entry.AppliedVersion == nil || *entry.AppliedVersion != 3 {
			t.Errorf("AppliedVersion = %v, want 3", entry.AppliedVersion)
		}
	})
	t.Run("malformed entries are dropped without failing the whole map", func(t *testing.T) {
		raw := map[string]json.RawMessage{
			"good": json.RawMessage(`{"appliedVersion":2}`),
			"bad":  json.RawMessage(`not json`),
		}
		got := decodeOutcomes(raw)
		if len(got) != 1 {
			t.Errorf("len = %d, want 1 (bad entry dropped)", len(got))
		}
		if _, ok := got["bad"]; ok {
			t.Error("bad entry must be dropped")
		}
	})
}

func TestManager_GetShadowComparison(t *testing.T) {
	t.Run("happy: keys union of desired+reported", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		now := time.Now().UTC()
		// Build a row where desired={"a":true,"b":"x"}, reported={"a":false,"c":1}.
		desiredJSON, _ := json.Marshal(map[string]any{"a": true, "b": "x"})
		reportedJSON, _ := json.Marshal(map[string]any{"a": false, "c": 1})
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(pgxmock.NewRows(getThingMgrCols).AddRow(
				"t-1", "agent", "host", "1.0", "addr",
				"sso", "bearer", "http",
				"online",
				desiredJSON, reportedJSON,
				int64(5), int64(3),
				[]byte(`{}`), &now, now,
				[]byte(`{}`), &now,
				"", "", "", "", "",
				"", "", "", "",
			))
		got, err := mgr.GetShadowComparison(context.Background(), "t-1")
		if err != nil {
			t.Fatalf("GetShadowComparison: %v", err)
		}
		if got.ThingID != "t-1" || got.ThingType != "agent" {
			t.Errorf("id/type wrong: %+v", got)
		}
		if got.Synced {
			t.Error("Synced must be false (3 < 5)")
		}
		if len(got.Keys) != 3 {
			t.Errorf("Keys len = %d, want 3 (a + b + c)", len(got.Keys))
		}
		// a: desired=true, reported=false → not synced.
		if got.Keys["a"].Synced {
			t.Error("key a should be drifted")
		}
		// b: desired only → InDesired true.
		if got.Keys["b"].Reported != nil {
			t.Errorf("key b reported should be nil, got %v", got.Keys["b"].Reported)
		}
		if !got.Keys["b"].InDesired {
			t.Error("key b (desired-only) must have InDesired=true")
		}
		// c: reported only → InDesired false (the auto-heal must ignore it).
		if got.Keys["c"].Desired != nil {
			t.Errorf("key c desired should be nil, got %v", got.Keys["c"].Desired)
		}
		if got.Keys["c"].InDesired {
			t.Error("key c (reported-only) must have InDesired=false")
		}
		// a: present in desired → InDesired true.
		if !got.Keys["a"].InDesired {
			t.Error("key a (in desired) must have InDesired=true")
		}
	})
	t.Run("store err propagates", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("missing").
			WillReturnError(pgx.ErrNoRows)
		_, err := mgr.GetShadowComparison(context.Background(), "missing")
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want store.ErrNotFound", err)
		}
	})
}

func TestManager_RePushConfig(t *testing.T) {
	t.Run("happy: GetThing then fan out", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
		mgr := NewWithPool(st, mock, nil, nil, ws, "hub-test", silentLogger())

		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		if err := mgr.RePushConfig(context.Background(), "t-1", "agent"); err != nil {
			t.Fatalf("RePushConfig: %v", err)
		}
		ws.mu.Lock()
		sendCount := len(ws.sendCalls)
		ws.mu.Unlock()
		if sendCount != 1 {
			t.Errorf("WS Send = %d, want 1", sendCount)
		}
	})
	t.Run("GetThing err propagates", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("missing").
			WillReturnError(pgx.ErrNoRows)
		err := mgr.RePushConfig(context.Background(), "missing", "agent")
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestManager_RePushConfigKey(t *testing.T) {
	t.Run("requires non-empty thingID + configKey", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		if err := mgr.RePushConfigKey(context.Background(), "", "k"); err == nil {
			t.Error("expected err for empty thingID")
		}
		if err := mgr.RePushConfigKey(context.Background(), "t", ""); err == nil {
			t.Error("expected err for empty configKey")
		}
	})
	t.Run("happy: GetThing + WS Send", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
		mgr := NewWithPool(st, mock, nil, nil, ws, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		if err := mgr.RePushConfigKey(context.Background(), "t-1", "hooks"); err != nil {
			t.Fatalf("RePushConfigKey: %v", err)
		}
	})
	t.Run("GetThing err propagates", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("missing").
			WillReturnError(errors.New("planner err"))
		err := mgr.RePushConfigKey(context.Background(), "missing", "k")
		if err == nil {
			t.Error("expected propagated err")
		}
	})
}

func TestManager_ForceResyncAll(t *testing.T) {
	t.Run("requires non-empty thingID", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		if _, err := mgr.ForceResyncAll(context.Background(), ""); err == nil {
			t.Error("expected err for empty thingID")
		}
	})
	t.Run("happy fan-out bumps desired_ver then pushes every key", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
		mgr := NewWithPool(st, mock, nil, nil, ws, "hub-test", silentLogger())
		desired := map[string]any{
			"k1": map[string]any{"v": 1},
			"k2": map[string]any{"v": 2},
		}
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", desired, 9))
		// The version bump is the reliable-delivery guarantee — it MUST
		// take the advisory lock and run inside a tx like every other per-Thing
		// desired writer.
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		expectWriteDesiredAndBumpVer(mock, "t-1", 10)
		mock.ExpectCommit()
		res, err := mgr.ForceResyncAll(context.Background(), "t-1")
		if err != nil {
			t.Fatalf("ForceResyncAll: %v", err)
		}
		if res.Pushed != 2 || len(res.Failed) != 0 {
			t.Errorf("res = %+v, want Pushed=2 Failed=[]", res)
		}
	})
	t.Run("no delivery path counts as Pushed (heartbeat pull covers it after bump)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		// No WS connection, no MQ: the immediate push fails with
		// ErrNoDeliveryPath, but the desired_ver bump guarantees the HTTP
		// heartbeat pull will deliver — so it is honestly counted as Pushed,
		// not Failed.
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		desired := map[string]any{"k1": map[string]any{"v": 1}}
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", desired, 1))
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		expectWriteDesiredAndBumpVer(mock, "t-1", 2)
		mock.ExpectCommit()
		res, err := mgr.ForceResyncAll(context.Background(), "t-1")
		if err != nil {
			t.Fatalf("ForceResyncAll: %v", err)
		}
		if res.Pushed != 1 || len(res.Failed) != 0 {
			t.Errorf("res = %+v, want Pushed=1 Failed=[]", res)
		}
	})
	t.Run("empty desired skips the bump and pushes nothing", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 1))
		// No ExpectBegin: an empty desired map has nothing to deliver, so
		// bumpDesiredVerForResync returns before opening a tx.
		res, err := mgr.ForceResyncAll(context.Background(), "t-1")
		if err != nil {
			t.Fatalf("ForceResyncAll: %v", err)
		}
		if res.Pushed != 0 || len(res.Failed) != 0 {
			t.Errorf("res = %+v, want Pushed=0 Failed=[]", res)
		}
	})
	t.Run("GetThing err propagates", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("missing").
			WillReturnError(errors.New("planner err"))
		_, err := mgr.ForceResyncAll(context.Background(), "missing")
		if err == nil {
			t.Error("expected err")
		}
	})
	t.Run("bump tx begin error aborts before any push", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"k1": map[string]any{"v": 1}}, 1))
		mock.ExpectBegin().WillReturnError(errors.New("conn lost"))
		if _, err := mgr.ForceResyncAll(context.Background(), "t-1"); err == nil {
			t.Error("expected bump tx error")
		}
	})
	t.Run("real push error (not no-delivery-path) goes into Failed", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		// MQ publish fails for the single key → publish-hub-signal wrap (not
		// ErrNoDeliveryPath) → recorded in Failed, not counted as Pushed.
		mq := &mockMQProducer{publishErr: errors.New("nats down")}
		mgr := NewWithPool(st, mock, nil, mq, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"k1": map[string]any{"v": 1}}, 1))
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		expectWriteDesiredAndBumpVer(mock, "t-1", 2)
		mock.ExpectCommit()
		res, err := mgr.ForceResyncAll(context.Background(), "t-1")
		if err != nil {
			t.Fatalf("ForceResyncAll: %v", err)
		}
		if res.Pushed != 0 || len(res.Failed) != 1 {
			t.Errorf("res = %+v, want Pushed=0 Failed[1]", res)
		}
		if res.Failed[0].ConfigKey != "k1" {
			t.Errorf("Failed[0].ConfigKey = %q, want k1", res.Failed[0].ConfigKey)
		}
	})
}

func TestManager_ForceResyncKey(t *testing.T) {
	t.Run("requires non-empty thingID + configKey", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		if err := mgr.ForceResyncKey(context.Background(), "", "k"); err == nil {
			t.Error("expected err for empty thingID")
		}
		if err := mgr.ForceResyncKey(context.Background(), "t", ""); err == nil {
			t.Error("expected err for empty configKey")
		}
	})
	t.Run("key not in desired returns ErrConfigKeyNotInDesired before any bump", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"other": 1}, 1))
		// No ExpectBegin: the missing-key check runs before bumpDesiredVerForResync.
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); !errors.Is(err, ErrConfigKeyNotInDesired) {
			t.Errorf("err = %v, want ErrConfigKeyNotInDesired", err)
		}
	})
	t.Run("happy: bumps desired_ver then pushes the key over WS", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
		mgr := NewWithPool(st, mock, nil, nil, ws, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		expectWriteDesiredAndBumpVer(mock, "t-1", 5)
		mock.ExpectCommit()
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); err != nil {
			t.Fatalf("ForceResyncKey: %v", err)
		}
	})
	t.Run("no delivery path is swallowed (bump guarantees heartbeat delivery)", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		expectWriteDesiredAndBumpVer(mock, "t-1", 5)
		mock.ExpectCommit()
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); err != nil {
			t.Fatalf("ForceResyncKey: want nil (ErrNoDeliveryPath swallowed), got %v", err)
		}
	})
	t.Run("GetThing err propagates", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("missing").
			WillReturnError(errors.New("planner err"))
		if err := mgr.ForceResyncKey(context.Background(), "missing", "k"); err == nil {
			t.Error("expected propagated err")
		}
	})
	t.Run("bump tx begin error aborts", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		mock.ExpectBegin().WillReturnError(errors.New("conn lost"))
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); err == nil {
			t.Error("expected bump tx error")
		}
	})
	t.Run("advisory lock error aborts", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		mock.ExpectBegin()
		mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
			WithArgs("agent").
			WillReturnError(errors.New("lock timeout"))
		mock.ExpectRollback()
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); err == nil {
			t.Error("expected advisory lock error")
		}
	})
	t.Run("write desired error aborts", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
			WithArgs("t-1", pgxmock.AnyArg()).
			WillReturnError(errors.New("planner err"))
		mock.ExpectRollback()
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); err == nil {
			t.Error("expected write desired error")
		}
	})
	t.Run("commit error aborts", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		expectWriteDesiredAndBumpVer(mock, "t-1", 5)
		mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); err == nil {
			t.Error("expected commit error")
		}
	})
	t.Run("real push error (not no-delivery-path) propagates", func(t *testing.T) {
		mock, _ := pgxmock.NewPool()
		defer mock.Close()
		st := store.NewWithPgxPool(mock)
		// MQ publish fails → rePushConfigKeyForThing returns a publish-hub-signal
		// wrap, NOT ErrNoDeliveryPath, so ForceResyncKey must surface it.
		mq := &mockMQProducer{publishErr: errors.New("nats down")}
		mgr := NewWithPool(st, mock, nil, mq, &mockWSPool{}, "hub-test", silentLogger())
		mock.ExpectQuery(`FROM thing t`).
			WithArgs("t-1").
			WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 4))
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "agent")
		expectWriteDesiredAndBumpVer(mock, "t-1", 5)
		mock.ExpectCommit()
		if err := mgr.ForceResyncKey(context.Background(), "t-1", "hooks"); err == nil ||
			!strings.Contains(err.Error(), "publish hub signal") {
			t.Errorf("err = %v, want publish-hub-signal wrap", err)
		}
	})
}

// TestRePushConfigForThing_MQError covers the error-wrap when the MQ publish
// fails on the non-WS branch. Drift repair flagging this lets the caller
// surface delivery-path issues.
func TestRePushConfigForThing_MQError(t *testing.T) {
	thing := &store.Thing{
		ID:         "t-1",
		Type:       "agent",
		Desired:    map[string]any{"hooks": map[string]any{"e": true}},
		DesiredVer: 1,
	}
	ws := &mockWSPool{} // not connected
	mq := &mockMQProducer{publishErr: errors.New("mq down")}
	mgr := &Manager{
		logger: silentLogger(),
		ws:     ws,
		mq:     mq,
		hubID:  "hub-1",
	}
	err := mgr.rePushConfigForThing(context.Background(), "agent", thing)
	if err == nil || !strings.Contains(err.Error(), "publish hub signal") {
		t.Errorf("err = %v, want publish-hub-signal wrap", err)
	}
}

// TestRePushConfigKeyForThing_MQError covers the per-key MQ error wrap.
func TestRePushConfigKeyForThing_MQError(t *testing.T) {
	thing := &store.Thing{
		ID:         "t-1",
		Type:       "agent",
		Desired:    map[string]any{"hooks": map[string]any{"e": true}},
		DesiredVer: 1,
	}
	ws := &mockWSPool{} // not connected
	mq := &mockMQProducer{publishErr: errors.New("mq down")}
	mgr := &Manager{
		logger: silentLogger(),
		ws:     ws,
		mq:     mq,
		hubID:  "hub-1",
	}
	err := mgr.rePushConfigKeyForThing(context.Background(), thing, "hooks")
	if err == nil || !strings.Contains(err.Error(), "publish hub signal") {
		t.Errorf("err = %v, want publish-hub-signal wrap", err)
	}
}

// TestManager_UpdateConfig_Happy walks the 6-step UpdateConfig flow against
// pgxmock: Begin → UpsertConfigTemplate → UpdateDesiredForType → InsertConfigChangeEvent
// → Commit, then post-commit WS broadcast.
func TestManager_UpdateConfig_Happy(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{broadcastCount: 3}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	// Step 1: UpsertConfigTemplate → returns newVer=5.
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "hooks", pgxmock.AnyArg(), "actor-1").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(5)))
	// Step 2: UpdateDesiredForType → returns one row with desired_ver=42.
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "hooks", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).
			AddRow("t-1", int64(42)))
	// thingType="agent" is NOT nexus-hub, so UpdateDesiredForType emits
	// NO per-Thing pg_notify — config reaches agents via the WS broadcast below.
	// (No ExpectExec(pg_notify); an emit here would surface as an unmatched SQL.)
	// Step 4: InsertConfigChangeEvent (9 args).
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs("agent", "hooks", "update", "actor-1", "alice", pgxmock.AnyArg(), int64(5), "10.0.0.1", false).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	resp, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent",
		ConfigKey: "hooks",
		State:     map[string]any{"e": true},
		Action:    "update",
		ActorID:   "actor-1",
		ActorName: "alice",
		SourceIP:  "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !resp.OK || resp.Version != 5 || resp.ThingDesiredVer != 42 {
		t.Errorf("resp = %+v, want OK=true Version=5 ThingDesiredVer=42", resp)
	}
	if resp.ThingsNotified != 3 {
		t.Errorf("ThingsNotified = %d, want 3", resp.ThingsNotified)
	}
}

// TestManager_UpdateConfig_NoThings covers the affected=0 branch — the
// template moved but no Thing of that type exists. ThingDesiredVer stays 0,
// no broadcast fires.
func TestManager_UpdateConfig_NoThings(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{broadcastCount: 99}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "hooks", pgxmock.AnyArg(), "actor-1").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(1)))
	// Update returns 0 rows — no notify, no broadcast.
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "hooks", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}))
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs("agent", "hooks", "update", "actor-1", "", pgxmock.AnyArg(), int64(1), "", false).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit()

	resp, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "hooks", State: map[string]any{"e": true},
		Action: "update", ActorID: "actor-1",
	})
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if resp.ThingsOnline != 0 || resp.ThingsNotified != 0 || resp.ThingDesiredVer != 0 {
		t.Errorf("resp = %+v, want zeroed broadcast counters", resp)
	}
}

// TestManager_UpdateConfig_BeginErr surfaces the Begin error.
func TestManager_UpdateConfig_BeginErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectBegin().WillReturnError(errors.New("conn lost"))
	_, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{ThingType: "agent", ConfigKey: "h"})
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("err = %v, want begin-tx wrap", err)
	}
}

// TestManager_UpdateConfig_UpsertErr surfaces the template upsert error.
func TestManager_UpdateConfig_UpsertErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "h", pgxmock.AnyArg(), "actor").
		WillReturnError(errors.New("dup"))
	mock.ExpectRollback()
	_, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "h", State: map[string]any{"x": 1}, ActorID: "actor",
	})
	if err == nil || !strings.Contains(err.Error(), "upsert template") {
		t.Errorf("err = %v, want upsert-template wrap", err)
	}
}

// TestManager_UpdateConfig_UpdateDesiredErr surfaces the type fan-out error.
func TestManager_UpdateConfig_UpdateDesiredErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "h", pgxmock.AnyArg(), "actor").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(1)))
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "h", pgxmock.AnyArg()).
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()
	_, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "h", State: map[string]any{"x": 1}, ActorID: "actor",
	})
	if err == nil || !strings.Contains(err.Error(), "update desired") {
		t.Errorf("err = %v, want update-desired wrap", err)
	}
}

// TestManager_UpdateConfig_InsertEventErr surfaces the change-event error.
func TestManager_UpdateConfig_InsertEventErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "h", pgxmock.AnyArg(), "actor").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(1)))
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "h", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).AddRow("t-1", int64(2)))
	// agent type emits no pg_notify in UpdateDesiredForType.
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()
	_, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "h", State: map[string]any{"x": 1}, ActorID: "actor",
	})
	if err == nil || !strings.Contains(err.Error(), "insert change event") {
		t.Errorf("err = %v, want insert-change-event wrap", err)
	}
}

// TestManager_UpdateConfig_CommitErr surfaces the commit error.
func TestManager_UpdateConfig_CommitErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectQuery(`INSERT INTO thing_config_template`).
		WithArgs("agent", "h", pgxmock.AnyArg(), "actor").
		WillReturnRows(pgxmock.NewRows([]string{"version"}).AddRow(int64(1)))
	mock.ExpectQuery(`WITH next AS`).
		WithArgs("agent", "h", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "desired_ver"}).AddRow("t-1", int64(2)))
	// agent type emits no pg_notify in UpdateDesiredForType.
	mock.ExpectExec(`INSERT INTO config_change_event`).
		WithArgs("agent", "h", "", "actor", "", pgxmock.AnyArg(), int64(1), "", false).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
	_, err := mgr.UpdateConfig(context.Background(), UpdateConfigRequest{
		ThingType: "agent", ConfigKey: "h", State: map[string]any{"x": 1}, ActorID: "actor",
	})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v, want commit wrap", err)
	}
}

// TestCacheDesired_WithRedis verifies the redis-set path of cacheDesired by
// stamping a key through a miniredis instance. Without this branch the
// function only ever hits the nil-skip line (counted at 25%).
func TestCacheDesired_WithRedis(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	mgr := New(nil, rdb, nil, nil, "hub-1", silentLogger())
	mgr.cacheDesired(context.Background(), "agent", map[string]any{
		"hooks": map[string]any{"e": true},
	})
	got, err := mini.Get("nexus:desired:agent:hooks")
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v["e"] != true {
		t.Errorf("cached value mismatch: %+v", v)
	}
}

// TestCacheDesiredKey_WithRedis covers the per-key cache helper.
func TestCacheDesiredKey_WithRedis(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	mgr := New(nil, rdb, nil, nil, "hub-1", silentLogger())
	mgr.cacheDesiredKey(context.Background(), "agent", "routing", map[string]any{"r": 1})
	got, err := mini.Get("nexus:desired:agent:routing")
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	if !strings.Contains(got, `"r":1`) {
		t.Errorf("cached payload = %q, want r:1", got)
	}
}

// TestCacheShadow_WithRedis covers the per-shadow-key cache helper.
func TestCacheShadow_WithRedis(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	mgr := New(nil, rdb, nil, nil, "hub-1", silentLogger())
	mgr.cacheShadow(context.Background(), "thing-1", map[string]any{
		"hooks": map[string]any{"e": true},
	})
	got, err := mini.Get("nexus:shadow:thing-1:hooks")
	if err != nil {
		t.Fatalf("miniredis Get: %v", err)
	}
	if !strings.Contains(got, `"e":true`) {
		t.Errorf("cached payload = %q", got)
	}
}

func TestNullableString(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	if got := nullableString("x"); got != "x" {
		t.Errorf("\"x\" → %v, want \"x\"", got)
	}
}

func TestNullableJSON(t *testing.T) {
	if got := nullableJSON(nil); got != nil {
		t.Errorf("nil → %v, want nil", got)
	}
	if got := nullableJSON([]byte{}); got != nil {
		t.Errorf("empty → %v, want nil", got)
	}
	in := []byte(`{"a":1}`)
	got := nullableJSON(in)
	if gotB, ok := got.([]byte); !ok || string(gotB) != `{"a":1}` {
		t.Errorf("got = %v", got)
	}
}

// TestManager_PublishHubSignal_PublishErr covers the publish-error swallow
// (warn-only). Without this the publish-err branch in publishHubSignal stays
// uncovered.
func TestManager_PublishHubSignal_PublishErr(t *testing.T) {
	mq := &mockMQProducer{publishErr: errors.New("mq down")}
	mgr := New(nil, nil, mq, nil, "hub-1", silentLogger())
	// Must not panic; must not propagate.
	mgr.publishHubSignal(context.Background(), "agent", "h", map[string]any{"v": 1}, 5)
}

// audit chain advisory lock key, mirrored from traffic/chain/chain.go.
// The override insertAdminAuditLog helper calls NextHash which acquires this
// lock as its first statement inside the tx — every override test must wire
// it through the pgxmock expectation chain.
const auditChainAdvisoryLockKey int64 = 0x4E4558_4155_4348 // "NEXAUCH"

// expectAuditChainGenesis pins the chain-lock + chain-head SELECT for an
// audit insert in a brand-new chain (no prior rows). The INSERT itself is
// the very next statement in the override tx — see expectInsertAdminAudit.
func expectAuditChainGenesis(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`pg_advisory_xact_lock`).
		WithArgs(auditChainAdvisoryLockKey).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT "integrityHash" FROM "AdminAuditLog"`).
		WillReturnError(pgx.ErrNoRows)
}

// expectInsertAdminAudit pins the actual AdminAuditLog INSERT — args are
// opaque (uuid + timestamp + hash + etc.). The INSERT statement has 13
// placeholder args; we set every one to AnyArg since the hash inputs are
// derived non-deterministically inside NextHash.
func expectInsertAdminAudit(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
}

// expectConfigVersionLock pins the per-type advisory lock that UpdateConfig /
// SetOverride / ClearOverride take as the FIRST statement of their transaction
// (store.AcquireConfigVersionLock). It must be expected immediately
// after ExpectBegin and before any other in-tx statement.
func expectConfigVersionLock(mock pgxmock.PgxPoolIface, thingType string) {
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
		WithArgs(thingType).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
}

// expectRecomputeDesiredTx pins the two SELECTs recomputeDesiredTx runs
// inside the override tx (templates FOR SHARE then overrides). Both return
// empty result sets so the merged map is {} — sufficient to drive the
// happy-path WriteDesiredAndBumpVer assertion without seeding rows.
func expectRecomputeDesiredTx(mock pgxmock.PgxPoolIface, thingType, thingID string) {
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs(thingType).
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1`).
		WithArgs(thingID).
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}))
}

// expectWriteDesiredAndBumpVer pins the WriteDesiredAndBumpVer UPDATE +
// pg_notify pair the override tx fires after recompute.
func expectWriteDesiredAndBumpVer(mock pgxmock.PgxPoolIface, thingID string, newVer int64) {
	mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
		WithArgs(thingID, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"desired_ver"}).AddRow(newVer))
	mock.ExpectExec(`pg_notify`).
		WithArgs(pgxmock.AnyArg(), thingID).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
}

// TestManager_SetOverride_RequiresFields covers the early-return validation.
func TestManager_SetOverride_RequiresFields(t *testing.T) {
	mgr := New(nil, nil, nil, nil, "hub-1", silentLogger())
	if _, err := mgr.SetOverride(context.Background(), SetOverrideRequest{ConfigKey: "k"}); err == nil {
		t.Error("expected err for empty thingID")
	}
	if _, err := mgr.SetOverride(context.Background(), SetOverrideRequest{ThingID: "t"}); err == nil {
		t.Error("expected err for empty configKey")
	}
}

// TestManager_SetOverride_BlacklistKey_Mock covers the configtypes.IsOverridable
// short-circuit (credentials/runtime/etc). The non-Mock variant in
// override_test.go uses a real DB (skips when unavailable); this mock variant
// runs unconditionally.
func TestManager_SetOverride_BlacklistKey_Mock(t *testing.T) {
	mgr := New(nil, nil, nil, nil, "hub-1", silentLogger())
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "credentials",
		State: json.RawMessage(`{"x":1}`), SetBy: "alice",
	})
	if !errors.Is(err, ErrKeyNotOverridable) {
		t.Errorf("err = %v, want ErrKeyNotOverridable", err)
	}
}

// TestManager_SetOverride_BadState covers NewOverrideState validation
// (non-object JSON).
func TestManager_SetOverride_BadState(t *testing.T) {
	mgr := New(nil, nil, nil, nil, "hub-1", silentLogger())
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`123`), SetBy: "alice", // not an object
	})
	if err == nil {
		t.Error("expected err for non-object state")
	}
}

// TestManager_SetOverride_ThingMissing covers GetThing ErrNotFound surfaced.
func TestManager_SetOverride_ThingMissing(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "missing", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestManager_SetOverride_NoTemplate_Mock covers ErrTemplateMissing path.
func TestManager_SetOverride_NoTemplate_Mock(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnError(pgx.ErrNoRows)
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if !errors.Is(err, ErrTemplateMissing) {
		t.Errorf("err = %v, want ErrTemplateMissing", err)
	}
}

// TestManager_SetOverride_TemplateErrWraps surfaces a generic template-fetch
// error (not ErrNotFound) wrapped with "get config template".
func TestManager_SetOverride_TemplateErrWraps(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnError(errors.New("planner err"))
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "get config template") {
		t.Errorf("err = %v, want get-config-template wrap", err)
	}
}

// TestManager_SetOverride_Happy walks the full happy path: GetThing →
// GetConfigTemplate → Begin → UpsertOverride → recomputeDesiredTx (empty
// template + override sets) → WriteDesiredAndBumpVer → audit chain INSERT →
// Commit → GetOverride re-fetch → RePushConfigKey (which re-GetsThing +
// sends via WS).
func TestManager_SetOverride_Happy(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	// Step 1: GetThing.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	// Step 2: GetConfigTemplate → returns version 4.
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":false}`), int64(4), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	// Step 3: UpsertOverride.
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), // thing_id
			pgxmock.AnyArg(), // config_key
			pgxmock.AnyArg(), // state bytes
			pgxmock.AnyArg(), // template_ver_at_set
			pgxmock.AnyArg(), // set_by
			pgxmock.AnyArg(), // reason
			pgxmock.AnyArg(), // expires_at
			pgxmock.AnyArg(), // emergency_override
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Step 4: recomputeDesiredTx (empty templates+overrides).
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	// Step 5: WriteDesiredAndBumpVer.
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	// Step 6: insertAdminAuditLog → advisory_lock + head-select + INSERT.
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	// Step 7: post-commit GetOverride re-fetch.
	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow(
			"t-1", "hooks", []byte(`{"e":true}`), int64(4),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false,
		))
	// Step 8: RePushConfigKey → re-fetches the thing.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 1))

	got, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if got == nil || got.ThingID != "t-1" {
		t.Errorf("got: %+v", got)
	}
	if got.TemplateVerAtSet != 4 {
		t.Errorf("TemplateVerAtSet = %d, want 4", got.TemplateVerAtSet)
	}
}

// TestManager_SetOverride_KillswitchAutoEmergency verifies that the
// configKey="killswitch" path forces emergencyOverride=true even without a
// break-glass reason. The test wires the same expectation chain as Happy
// except it sets configKey=killswitch and asserts the returned override
// carries EmergencyOverride.
func TestManager_SetOverride_KillswitchAutoEmergency(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "killswitch").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "killswitch", []byte(`{"engaged":false}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), // thing_id
			pgxmock.AnyArg(), // config_key
			pgxmock.AnyArg(), // state bytes
			pgxmock.AnyArg(), // template_ver_at_set
			pgxmock.AnyArg(), // set_by
			pgxmock.AnyArg(), // reason
			pgxmock.AnyArg(), // expires_at
			pgxmock.AnyArg(), // emergency_override
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "killswitch").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow(
			"t-1", "killswitch", []byte(`{"engaged":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), true,
		))
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"killswitch": map[string]any{"engaged": true}}, 1))

	got, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "killswitch",
		State: json.RawMessage(`{"engaged":true}`), SetBy: "alice",
	})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if !got.EmergencyOverride {
		t.Error("EmergencyOverride should be true for killswitch")
	}
}

// TestManager_SetOverride_BreakGlassReason_Mock exercises the "break-glass:"
// reason prefix that flips emergencyOverride to true on a non-killswitch key.
func TestManager_SetOverride_BreakGlassReason_Mock(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":false}`), int64(2), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), // thing_id
			pgxmock.AnyArg(), // config_key
			pgxmock.AnyArg(), // state bytes
			pgxmock.AnyArg(), // template_ver_at_set
			pgxmock.AnyArg(), // set_by
			pgxmock.AnyArg(), // reason
			pgxmock.AnyArg(), // expires_at
			pgxmock.AnyArg(), // emergency_override
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	setAt := time.Now().UTC()
	reason := "break-glass: incident-1"
	expiresAt := time.Now().Add(2 * time.Hour).UTC()
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow(
			"t-1", "hooks", []byte(`{"e":true}`), int64(2),
			"responder", setAt, &reason, &expiresAt, true,
		))
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 1))

	got, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "responder",
		Reason: &reason, ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if !got.EmergencyOverride {
		t.Error("EmergencyOverride should be true for break-glass: reason")
	}
}

// TestManager_SetOverride_BeginErr surfaces the Begin error.
func TestManager_SetOverride_BeginErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":false}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin().WillReturnError(errors.New("conn lost"))
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("err = %v, want begin-tx wrap", err)
	}
}

func TestManager_ClearOverride_RequiresFields(t *testing.T) {
	mgr := New(nil, nil, nil, nil, "hub-1", silentLogger())
	if err := mgr.ClearOverride(context.Background(), "", "k", "actor"); err == nil {
		t.Error("expected err for empty thingID")
	}
	if err := mgr.ClearOverride(context.Background(), "t", "", "actor"); err == nil {
		t.Error("expected err for empty configKey")
	}
}

// TestManager_ClearOverride_ThingMissing surfaces ErrNotFound from GetThing.
func TestManager_ClearOverride_ThingMissing(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)
	err := mgr.ClearOverride(context.Background(), "missing", "k", "actor")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestManager_ClearOverride_OverrideMissing surfaces ErrNotFound from
// GetOverride (pre-tx capture for audit-before).
func TestManager_ClearOverride_OverrideMissing(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnError(pgx.ErrNoRows)
	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestManager_ClearOverride_Happy walks the full clear path.
func TestManager_ClearOverride_Happy(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	// GetThing.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	// GetOverride (pre-tx capture).
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow(
			"t-1", "hooks", []byte(`{"e":true}`), int64(4),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false,
		))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	// DeleteOverride.
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 2)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	// post-commit RePushConfigKey → re-fetches the thing.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": false}}, 2))

	if err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "bob"); err != nil {
		t.Fatalf("ClearOverride: %v", err)
	}
}

// TestManager_ClearOverride_DeleteRace covers the case where GetOverride saw
// a row but a parallel session deleted it between the two queries — surfaces
// store.ErrNotFound.
func TestManager_ClearOverride_DeleteRace(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow(
			"t-1", "hooks", []byte(`{"e":true}`), int64(4),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false,
		))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	mock.ExpectRollback()
	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound (race)", err)
	}
}

// TestManager_ClearOverride_BeginErr surfaces the Begin error.
func TestManager_ClearOverride_BeginErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectBegin().WillReturnError(errors.New("conn lost"))
	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if err == nil || !strings.Contains(err.Error(), "begin tx") {
		t.Errorf("err = %v, want begin-tx wrap", err)
	}
}

// TestRecomputeDesiredTx_TemplateQueryErr covers the templates-FOR-SHARE
// error wrap inside recomputeDesiredTx. We drive it via SetOverride so the
// outer wrapping is "recompute desired".
func TestRecomputeDesiredTx_TemplateQueryErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), // thing_id
			pgxmock.AnyArg(), // config_key
			pgxmock.AnyArg(), // state bytes
			pgxmock.AnyArg(), // template_ver_at_set
			pgxmock.AnyArg(), // set_by
			pgxmock.AnyArg(), // reason
			pgxmock.AnyArg(), // expires_at
			pgxmock.AnyArg(), // emergency_override
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()

	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "recompute desired") {
		t.Errorf("err = %v, want recompute-desired wrap", err)
	}
}

// TestManager_HandleBreakGlassReport_MissingReportedState covers the
// "key version declared but no reported state" skip.
func TestManager_HandleBreakGlassReport_MissingReportedState(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	// No template fetch — early skip on missing state.
	err := mgr.handleBreakGlassReport(context.Background(), ShadowReportRequest{
		ID:          "t-1",
		Reported:    map[string]any{}, // no key "killswitch"
		KeyVersions: map[string]int64{"killswitch": 5},
		Reason:      "break_glass",
	})
	if err != nil {
		t.Fatalf("handleBreakGlassReport: %v", err)
	}
}

// TestManager_HandleBreakGlassReport_GetThingErr surfaces a wrapped GetThing
// error.
func TestManager_HandleBreakGlassReport_GetThingErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("missing").
		WillReturnError(errors.New("planner err"))
	err := mgr.handleBreakGlassReport(context.Background(), ShadowReportRequest{
		ID:           "missing",
		Reported:     map[string]any{"killswitch": map[string]any{"engaged": true}},
		KeyVersions:  map[string]int64{"killswitch": 1},
		Reason:       "break_glass",
		ActorTokenID: "tok",
	})
	if err == nil || !strings.Contains(err.Error(), "get thing") {
		t.Errorf("err = %v, want get-thing wrap", err)
	}
}

// TestRecomputeDesiredTx_PopulatedSets exercises the rows-iteration paths in
// recomputeDesiredTx: templates returns one row, overrides returns one row
// that overrides a different key. The merged map should contain both keys.
// Driven through SetOverride so we exercise the full integrated tx.
func TestRecomputeDesiredTx_PopulatedSets(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{"e":false}`), int64(3), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// recomputeDesiredTx: templates returns one row, overrides returns one row.
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}).
			AddRow("hooks", []byte(`{"e":false}`)).
			AddRow("routing", []byte(`{"r":1}`)))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1`).
		WithArgs("t-1").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}).
			AddRow("hooks", []byte(`{"e":true}`)))
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(3),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent",
			map[string]any{"hooks": map[string]any{"e": true}, "routing": map[string]any{"r": 1}}, 1))

	if _, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	}); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
}

// TestRecomputeDesiredTx_TemplateMalformedJSON covers the unmarshal-error
// path inside recomputeDesiredTx when a template's state column is invalid
// JSON.
func TestRecomputeDesiredTx_TemplateMalformedJSON(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}).
			AddRow("hooks", []byte(`not json`)))
	mock.ExpectRollback()

	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "unmarshal template state") {
		t.Errorf("err = %v, want unmarshal-template-state wrap", err)
	}
}

// TestRecomputeDesiredTx_OverrideQueryErr covers the overrides-query error
// wrap (templates succeed; overrides fail).
func TestRecomputeDesiredTx_OverrideQueryErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1`).
		WithArgs("t-1").
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()

	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "query overrides") {
		t.Errorf("err = %v, want query-overrides wrap", err)
	}
}

// TestRecomputeDesiredTx_OverrideMalformedJSON covers the unmarshal-error
// path on overrides.
func TestRecomputeDesiredTx_OverrideMalformedJSON(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1`).
		WithArgs("t-1").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}).
			AddRow("hooks", []byte(`not json`)))
	mock.ExpectRollback()

	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "unmarshal override state") {
		t.Errorf("err = %v, want unmarshal-override-state wrap", err)
	}
}

// TestTxPool_FallsThroughToStorePool exercises the seam-default path where
// m.pool is nil — txPool() must call m.store.Pool() instead of returning the
// nil overrider. store.NewWithPgxPool leaves the underlying *pgxpool.Pool
// nil, so the test asserts the returned PgxPool interface wraps a typed-nil
// *pgxpool.Pool (interface header is not untyped-nil — the dynamic type is
// *pgxpool.Pool). Production builds construct Store via store.New(pool) which
// makes Pool() return the same pgxpool the manager handed in.
func TestTxPool_FallsThroughToStorePool(t *testing.T) {
	st := store.NewWithPgxPool(nil)
	mgr := New(st, nil, nil, nil, "hub-1", silentLogger())
	got := mgr.txPool()
	// Confirm the fallthrough actually ran. Interface comparison to untyped
	// nil is intentionally false here because the dynamic type information
	// of *pgxpool.Pool is carried in the interface header. We assert the
	// converse — got != nil — so the branch coverage is recorded.
	if got == nil {
		t.Fatal("txPool returned untyped nil; the fallthrough to m.store.Pool() did not run")
	}
}

// TestTxPool_UsesInjectedPool covers the seam-set path: a non-nil pool wins
// over m.store.Pool(). Implicitly tested by every override/break-glass test
// above, but pinned here as a direct call so the branch shows up in the
// coverage profile cleanly.
func TestTxPool_UsesInjectedPool(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())
	if got := mgr.txPool(); got == nil {
		t.Error("txPool returned nil with non-nil override")
	}
}

// TestManager_SetOverride_UpsertErr surfaces the override-upsert wrap.
func TestManager_SetOverride_UpsertErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "upsert override") {
		t.Errorf("err = %v, want upsert-override wrap", err)
	}
}

// TestManager_SetOverride_WriteDesiredErr surfaces the WriteDesiredAndBumpVer
// wrap.
func TestManager_SetOverride_WriteDesiredErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1`).
		WithArgs("t-1").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}))
	mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
		WithArgs("t-1", pgxmock.AnyArg()).
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "write desired") {
		t.Errorf("err = %v, want write-desired wrap", err)
	}
}

// TestManager_SetOverride_AuditInsertErr surfaces the audit-log INSERT wrap.
func TestManager_SetOverride_AuditInsertErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "insert audit log") {
		t.Errorf("err = %v, want insert-audit-log wrap", err)
	}
}

// TestManager_SetOverride_CommitErr surfaces the commit wrap.
func TestManager_SetOverride_CommitErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v, want commit wrap", err)
	}
}

// TestManager_SetOverride_RefetchErr exercises the post-commit GetOverride
// re-fetch error: the override is committed, the function logs a warn and
// falls back to the in-memory `override` value (returning a non-nil result).
// The post-commit RePushConfigKey also fires.
func TestManager_SetOverride_RefetchErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	// GetOverride re-fetch fails — non-fatal, logged.
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnError(errors.New("planner err"))
	// RePushConfigKey re-fetches the thing → succeeds.
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 1))

	got, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if got == nil || got.ThingID != "t-1" {
		t.Errorf("got = %+v, want fallback in-memory override", got)
	}
}

// TestManager_SetOverride_PostCommitPushFails exercises the WARN log when
// RePushConfigKey errors after commit. The function must still return the
// committed override successfully.
func TestManager_SetOverride_PostCommitPushFails(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	// No WS, no MQ → RePushConfigKey returns ErrNoDeliveryPath.
	mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	// RePushConfigKey re-fetches the thing.
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": true}}, 1))

	got, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err != nil {
		t.Fatalf("SetOverride: %v (push failure must be non-fatal)", err)
	}
	if got == nil {
		t.Error("got nil, want committed override")
	}
}

// TestManager_ClearOverride_RecomputeErr surfaces a recompute failure inside
// the clear tx.
func TestManager_ClearOverride_RecomputeErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()

	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if err == nil || !strings.Contains(err.Error(), "recompute desired") {
		t.Errorf("err = %v, want recompute-desired wrap", err)
	}
}

// TestManager_ClearOverride_WriteDesiredErr surfaces the write wrap.
func TestManager_ClearOverride_WriteDesiredErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
		WithArgs("t-1", pgxmock.AnyArg()).
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()

	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if err == nil || !strings.Contains(err.Error(), "write desired") {
		t.Errorf("err = %v, want write-desired wrap", err)
	}
}

// TestManager_ClearOverride_AuditInsertErr surfaces audit insert wrap.
func TestManager_ClearOverride_AuditInsertErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	reason := "test"
	expiresAt := time.Now().Add(1 * time.Hour).UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	// Override with non-nil reason + expires_at to cover those branches in
	// the ClearOverride auditMeta builder.
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, &reason, &expiresAt, true))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()

	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if err == nil || !strings.Contains(err.Error(), "insert audit log") {
		t.Errorf("err = %v, want insert-audit-log wrap", err)
	}
}

// TestManager_ClearOverride_CommitErr surfaces commit wrap.
func TestManager_ClearOverride_CommitErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v, want commit wrap", err)
	}
}

// TestManager_ClearOverride_DeleteErr surfaces the DeleteOverride wrap.
func TestManager_ClearOverride_DeleteErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnError(errors.New("planner err"))
	mock.ExpectRollback()

	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if err == nil || !strings.Contains(err.Error(), "delete override") {
		t.Errorf("err = %v, want delete-override wrap", err)
	}
}

// TestManager_ClearOverride_PostCommitPushFails exercises the WARN log path
// for post-commit RePushConfigKey error.
func TestManager_ClearOverride_PostCommitPushFails(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	// No WS, no MQ → RePushConfigKey returns ErrNoDeliveryPath.
	mgr := NewWithPool(st, mock, nil, nil, &mockWSPool{}, "hub-1", silentLogger())

	setAt := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1 AND config_key = \$2`).
		WithArgs("t-1", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{
			"thing_id", "config_key", "state", "template_ver_at_set",
			"set_by", "set_at", "reason", "expires_at", "emergency_override",
		}).AddRow("t-1", "hooks", []byte(`{"e":true}`), int64(1),
			"alice", setAt, (*string)(nil), (*time.Time)(nil), false))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`DELETE FROM thing_config_override`).
		WithArgs("t-1", "hooks").
		WillReturnResult(pgconn.NewCommandTag("DELETE 1"))
	expectRecomputeDesiredTx(mock, "agent", "t-1")
	expectWriteDesiredAndBumpVer(mock, "t-1", 1)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	// RePushConfigKey re-fetches.
	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{"hooks": map[string]any{"e": false}}, 1))

	err := mgr.ClearOverride(context.Background(), "t-1", "hooks", "actor")
	if err != nil {
		t.Errorf("err = %v, want nil (push failure must be non-fatal)", err)
	}
}

// TestInsertAdminAuditLog_BeforeStateMarshal covers the BeforeState !=nil
// happy-path branch (ClearOverride hits it via priorState; we also test the
// audit-helper directly by driving an entry that includes BeforeState).
func TestInsertAdminAuditLog_BeforeStateBranches(t *testing.T) {
	// This test runs entirely via ClearOverride's audit path — covered by
	// TestManager_ClearOverride_AuditInsertErr (which sets non-nil reason +
	// expires_at on the prior override → exercises the BeforeState marshal
	// branch + reason/expires_at conditional branches in the auditMeta map).
	// Keep as a placeholder marker for the branch this targets so the test
	// suite documentation stays anchored.
	t.Skip("BeforeState branch covered by TestManager_ClearOverride_AuditInsertErr (non-nil reason + expires_at)")
}

// TestRecomputeDesiredTx_TemplateScanErr covers the template-row scan error
// path: a row arrives with too few columns or wrong types so Scan errors.
func TestRecomputeDesiredTx_TemplateScanErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	// Templates query: only 1 column where 2 expected → scan err.
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"config_key"}).AddRow("hooks"))
	mock.ExpectRollback()

	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "scan template") {
		t.Errorf("err = %v, want scan-template wrap", err)
	}
}

// TestRecomputeDesiredTx_OverrideScanErr covers the override-row scan error.
func TestRecomputeDesiredTx_OverrideScanErr(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	mgr := NewWithPool(st, mock, nil, nil, nil, "hub-1", silentLogger())

	mock.ExpectQuery(`FROM thing t`).WithArgs("t-1").
		WillReturnRows(minimalGetThingRow("t-1", "agent", map[string]any{}, 0))
	tmplTime := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("agent", "hooks").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("agent", "hooks", []byte(`{}`), int64(1), tmplTime, "alice"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "agent")
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1\s+FOR SHARE`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"config_key", "state"}))
	mock.ExpectQuery(`FROM thing_config_override\s+WHERE thing_id = \$1`).
		WithArgs("t-1").
		WillReturnRows(pgxmock.NewRows([]string{"config_key"}).AddRow("hooks"))
	mock.ExpectRollback()

	_, err := mgr.SetOverride(context.Background(), SetOverrideRequest{
		ThingID: "t-1", ConfigKey: "hooks",
		State: json.RawMessage(`{"e":true}`), SetBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "scan override") {
		t.Errorf("err = %v, want scan-override wrap", err)
	}
}

// TestRePushConfigForThing_MarshalErr covers the per-key state marshal-err
// wrap inside rePushConfigForThing.
func TestRePushConfigForThing_MarshalErr(t *testing.T) {
	thing := &store.Thing{
		ID: "t-1", Type: "agent",
		Desired:    map[string]any{"k": make(chan int)},
		DesiredVer: 1,
	}
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := &Manager{logger: silentLogger(), ws: ws, hubID: "hub-1"}
	err := mgr.rePushConfigForThing(context.Background(), "agent", thing)
	if err == nil || !strings.Contains(err.Error(), "marshal state for key") {
		t.Errorf("err = %v, want marshal-state wrap", err)
	}
}

// TestRePushConfigKeyForThing_MarshalErr covers the per-key state marshal-err
// wrap inside rePushConfigKeyForThing.
func TestRePushConfigKeyForThing_MarshalErr(t *testing.T) {
	thing := &store.Thing{
		ID: "t-1", Type: "agent",
		Desired:    map[string]any{"k": make(chan int)},
		DesiredVer: 1,
	}
	ws := &mockWSPool{connectedIDs: map[string]bool{"t-1": true}}
	mgr := &Manager{logger: silentLogger(), ws: ws, hubID: "hub-1"}
	err := mgr.rePushConfigKeyForThing(context.Background(), thing, "k")
	if err == nil || !strings.Contains(err.Error(), "marshal state for key") {
		t.Errorf("err = %v, want marshal-state wrap", err)
	}
}

// TestPublishHubSignal_MarshalErr exercises the marshal-err early-return.
func TestPublishHubSignal_MarshalErr(t *testing.T) {
	mq := &mockMQProducer{}
	mgr := New(nil, nil, mq, nil, "hub-1", silentLogger())
	mgr.publishHubSignal(context.Background(), "agent", "k", make(chan int), 1)
	// No publish call because marshal failed.
	mq.mu.Lock()
	defer mq.mu.Unlock()
	if mq.publishCount != 0 {
		t.Errorf("publishCount = %d, want 0", mq.publishCount)
	}
}

// TestCacheDesiredKey_MarshalErr exercises the marshal-err early-return.
func TestCacheDesiredKey_MarshalErr(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mini.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer rdb.Close()

	mgr := New(nil, rdb, nil, nil, "hub-1", silentLogger())
	// chan value cannot be marshaled.
	mgr.cacheDesiredKey(context.Background(), "agent", "k", make(chan int))
	// Nothing written.
	if _, err := mini.Get("nexus:desired:agent:k"); err == nil {
		t.Error("cacheDesiredKey wrote despite marshal err")
	}
}

// TestBroadcastConfigChanged_StateMarshalErr exercises the inner-marshal
// failure (state value can't be encoded).
func TestBroadcastConfigChanged_StateMarshalErr(t *testing.T) {
	ws := &mockWSPool{broadcastCount: 1}
	mgr := &Manager{logger: silentLogger(), ws: ws, hubID: "hub-1"}
	notified := mgr.broadcastConfigChanged("agent", "k", make(chan int), 1)
	if notified != 0 {
		t.Errorf("notified = %d, want 0 on marshal err", notified)
	}
	// No broadcast call because marshal failed before m.ws.Broadcast.
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.lastBroadcastMsg != nil {
		t.Error("Broadcast was called despite marshal err")
	}
}
