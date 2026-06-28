package manager

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// breakGlassExpiryMatcher is a pgxmock argument matcher asserting the
// thing_config_override.expires_at value adoptBreakGlassPerThing stamps is a
// non-nil *time.Time roughly breakGlassPerThingTTL into the future —
// not the old nil that left the emergency exemption permanent.
type breakGlassExpiryMatcher struct{}

func (breakGlassExpiryMatcher) Match(v any) bool {
	exp, ok := v.(*time.Time)
	if !ok || exp == nil {
		return false
	}
	want := time.Now().UTC().Add(breakGlassPerThingTTL)
	diff := exp.Sub(want)
	return diff > -time.Minute && diff < time.Minute
}

// TestShadowReportRequest_BreakGlassJSONFields locks in the wire contract for
// the four break-glass extension fields on ShadowReportRequest. Any rename or
// tag drift would silently corrupt the proxy->hub contract, so explicit key
// assertions catch it.
func TestShadowReportRequest_BreakGlassJSONFields(t *testing.T) {
	req := ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"killswitch": map[string]any{"engaged": true}},
		ReportedVer:  4,
		KeyVersions:  map[string]int64{"killswitch": 4},
		Reason:       "break_glass",
		SourceIP:     "10.0.0.7",
		ActorTokenID: "a1b2c3d4",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "reported", "reportedVer", "keyVersions", "reason", "sourceIp", "actorTokenId"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
	if m["reason"] != "break_glass" {
		t.Errorf("reason = %v, want break_glass", m["reason"])
	}
	if m["actorTokenId"] != "a1b2c3d4" {
		t.Errorf("actorTokenId = %v, want a1b2c3d4", m["actorTokenId"])
	}
}

// TestShadowReportRequest_OmitsBreakGlassFieldsWhenEmpty ensures a normal
// (non-break-glass) report does not leak empty break-glass fields onto the
// wire. omitempty tags carry this contract; any drift would make normal
// reports look like break-glass reports on the receiving side.
func TestShadowReportRequest_OmitsBreakGlassFieldsWhenEmpty(t *testing.T) {
	req := ShadowReportRequest{
		ID:          "proxy-1",
		Reported:    map[string]any{"k": "v"},
		ReportedVer: 1,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"keyVersions", "reason", "sourceIp", "actorTokenId"} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected JSON key %q for non-break-glass report", k)
		}
	}
}

// TestHandleBreakGlassReport_RejectsMissingKeyVersions asserts the
// reconciliation helper refuses a malformed break-glass report. This is the
// guard against a data-plane regression that forgets to attach per-key
// versions — without them, the Hub has no authoritative target to adopt.
//
// The Manager is constructed with nil deps because the function returns
// before touching the store when keyVersions is empty.
func TestHandleBreakGlassReport_RejectsMissingKeyVersions(t *testing.T) {
	mgr := New(nil, nil, nil, nil, "hub-test", slog.Default())

	req := ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"killswitch": map[string]any{"engaged": true}},
		ReportedVer:  4,
		Reason:       "break_glass",
		ActorTokenID: "a1b2c3d4",
		// KeyVersions intentionally nil.
	}
	err := mgr.handleBreakGlassReport(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing keyVersions, got nil")
	}
}

// --- server-side allowlist + schema gate ---

// TestValidateBreakGlassReport covers the authority gate that mirrors the
// data-plane {killswitch, exemptions} allowlist on the Hub side and validates
// each state against its canonical configtypes schema.
func TestValidateBreakGlassReport(t *testing.T) {
	t.Run("missing keyVersions rejected", func(t *testing.T) {
		err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported: map[string]any{"killswitch": map[string]any{"engaged": true}},
		})
		if err == nil {
			t.Fatal("expected error for missing keyVersions")
		}
	})
	t.Run("non-allowlisted key rejected", func(t *testing.T) {
		err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported:    map[string]any{"credentials": map[string]any{"token": "x"}},
			KeyVersions: map[string]int64{"credentials": 4},
		})
		if !errors.Is(err, ErrBreakGlassKeyNotAllowed) {
			t.Fatalf("err = %v, want ErrBreakGlassKeyNotAllowed", err)
		}
	})
	t.Run("killswitch wrong JSON type rejected", func(t *testing.T) {
		err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported:    map[string]any{"killswitch": "engaged"},
			KeyVersions: map[string]int64{"killswitch": 4},
		})
		if !errors.Is(err, ErrBreakGlassStateInvalid) {
			t.Fatalf("err = %v, want ErrBreakGlassStateInvalid", err)
		}
	})
	t.Run("killswitch unknown field rejected (enabled vs engaged drift)", func(t *testing.T) {
		err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported:    map[string]any{"killswitch": map[string]any{"enabled": true}},
			KeyVersions: map[string]int64{"killswitch": 4},
		})
		if !errors.Is(err, ErrBreakGlassStateInvalid) {
			t.Fatalf("err = %v, want ErrBreakGlassStateInvalid", err)
		}
	})
	t.Run("exemptions wrong shape rejected", func(t *testing.T) {
		err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported:    map[string]any{"exemptions": map[string]any{"entries": "not-an-array"}},
			KeyVersions: map[string]int64{"exemptions": 4},
		})
		if !errors.Is(err, ErrBreakGlassStateInvalid) {
			t.Fatalf("err = %v, want ErrBreakGlassStateInvalid", err)
		}
	})
	t.Run("valid killswitch accepted", func(t *testing.T) {
		if err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported:    map[string]any{"killswitch": map[string]any{"engaged": true}},
			KeyVersions: map[string]int64{"killswitch": 4},
		}); err != nil {
			t.Fatalf("valid killswitch rejected: %v", err)
		}
	})
	t.Run("valid exemptions accepted", func(t *testing.T) {
		if err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported: map[string]any{"exemptions": map[string]any{
				"entries": []any{map[string]any{"id": "e1", "sourceIP": "10.0.0.1", "targetHost": "h", "expiresAt": "2030-01-01T00:00:00Z", "reason": "r", "approvedBy": "a"}},
			}},
			KeyVersions: map[string]int64{"exemptions": 4},
		}); err != nil {
			t.Fatalf("valid exemptions rejected: %v", err)
		}
	})
	t.Run("declared key with no reported state skips schema check", func(t *testing.T) {
		// version declared but state absent → not a schema violation; adoption
		// skips it later.
		if err := ValidateBreakGlassReport(ShadowReportRequest{
			Reported:    map[string]any{},
			KeyVersions: map[string]int64{"killswitch": 4},
		}); err != nil {
			t.Fatalf("declared-no-state should pass validation: %v", err)
		}
	})
}

// TestHandleBreakGlassReport_RejectsBeforeStore asserts the gate runs BEFORE
// any store access: a non-allowlisted key is rejected with the Manager wired to
// nil deps (a GetThing call would panic), proving the privilege check is the
// first thing that happens.
func TestHandleBreakGlassReport_RejectsBeforeStore(t *testing.T) {
	mgr := New(nil, nil, nil, nil, "hub-test", slog.Default())
	err := mgr.handleBreakGlassReport(context.Background(), ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"credentials": map[string]any{"token": "x"}},
		KeyVersions:  map[string]int64{"credentials": 4},
		Reason:       "break_glass",
		ActorTokenID: "a1b2c3d4",
	})
	if !errors.Is(err, ErrBreakGlassKeyNotAllowed) {
		t.Fatalf("err = %v, want ErrBreakGlassKeyNotAllowed (gate must precede store)", err)
	}
}

// --- scope routing (fleet template vs per-Thing override) ---

// TestHandleBreakGlassReport_Killswitch_WritesFleetTemplate verifies that a
// fleet-scoped key (killswitch) still adopts into thing_config_template at the
// reported version with an emergency_override audit row — and crucially takes
// NO per-Thing override path (no advisory lock, no thing_config_override INSERT,
// no desired-ver bump). The pgxmock expectation set is exact: any unexpected
// query fails the test.
// TestHandleBreakGlassReport_Killswitch_WritesPerThingOverride locks the
// invariant that a node-initiated killswitch break-glass NO LONGER writes the
// fleet-wide thing_config_template (which would flip the kill-switch for every
// Thing of the type — a cross-node blast radius a compromised node could abuse).
// It now adopts as a PER-THING override on the reporting Thing ONLY (self-
// protection), exactly like exemptions: AcquireConfigVersionLock → upsert
// thing_config_override → recompute + bump THIS Thing's desired_ver → admin audit
// → push back to the node. The expectation set asserts the override write happens
// AND that NO UpsertConfigTemplateAt (fleet) is issued. Genuine fleet-wide
// killswitch stays operator-only via POST /api/admin/compliance/killswitch.
func TestHandleBreakGlassReport_Killswitch_WritesPerThingOverride(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"proxy-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-test", silentLogger())

	ksState := map[string]any{"engaged": true}

	// GetThing → desired_ver 1 (reported 5 > 1 → adopt).
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("proxy-1").
		WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{}, 1))
	// Template baseline lookup (single key) → version 2.
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("compliance-proxy", "killswitch").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("compliance-proxy", "killswitch", []byte(`{"engaged":false}`), int64(2), now, "admin"))
	mock.ExpectBegin()
	expectConfigVersionLock(mock, "compliance-proxy")
	// Per-Thing override upsert (NOT thing_config_template), emergency_override=true.
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			"proxy-1", "killswitch",
			pgxmock.AnyArg(), int64(2), "break-glass:a1b2c3d4",
			pgxmock.AnyArg(), breakGlassExpiryMatcher{}, true,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "compliance-proxy", "proxy-1")
	expectWriteDesiredAndBumpVer(mock, "proxy-1", 2)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	// RePushConfigKey re-fetches the Thing.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("proxy-1").
		WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{"killswitch": ksState}, 2))

	err := mgr.handleBreakGlassReport(context.Background(), ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"killswitch": ksState},
		ReportedVer:  5,
		KeyVersions:  map[string]int64{"killswitch": 5},
		Reason:       "break_glass",
		SourceIP:     "10.0.0.7",
		ActorTokenID: "a1b2c3d4",
	})
	if err != nil {
		t.Fatalf("handleBreakGlassReport: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected queries (killswitch break-glass must be PER-THING, never fleet template): %v", err)
	}
}

// TestHandleBreakGlassReport_Exemptions_WritesPerThingOverride verifies the
// scope-routing fix: a non-fleet key (exemptions) adopts as a PER-THING override on
// the reporting Thing only — it takes AcquireConfigVersionLock,
// upserts thing_config_override, recomputes + bumps that Thing's desired_ver,
// writes a chain-hashed admin audit row, and pushes the key back to the node.
// It MUST NOT touch thing_config_template (no fleet-wide blast radius). The
// exact expectation set asserts both: the override write happens AND no
// UpsertConfigTemplateAt is issued.
func TestHandleBreakGlassReport_Exemptions_WritesPerThingOverride(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	st := store.NewWithPgxPool(mock)
	ws := &mockWSPool{connectedIDs: map[string]bool{"proxy-1": true}}
	mgr := NewWithPool(st, mock, nil, nil, ws, "hub-test", silentLogger())

	exState := map[string]any{"entries": []any{map[string]any{
		"id": "e1", "sourceIP": "10.0.0.1", "targetHost": "h",
		"expiresAt": "2030-01-01T00:00:00Z", "reason": "incident", "approvedBy": "ops",
	}}}

	// GetThing → desired_ver 1 (reported 5 > 1 → adopt).
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("proxy-1").
		WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{}, 1))
	// Template baseline lookup (single key) → version 2.
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("compliance-proxy", "exemptions").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("compliance-proxy", "exemptions", []byte(`{"entries":[]}`), int64(2), now, "admin"))
	mock.ExpectBegin()
	// advisory lock on the per-Thing path.
	expectConfigVersionLock(mock, "compliance-proxy")
	// Per-Thing override upsert (8 args), emergency_override=true. Arg 7
	// (expires_at) MUST be a non-nil ~8h auto-revert stamp, not nil.
	mock.ExpectExec(`INSERT INTO thing_config_override`).
		WithArgs(
			"proxy-1", "exemptions",
			pgxmock.AnyArg(), int64(2), "break-glass:a1b2c3d4",
			pgxmock.AnyArg(), breakGlassExpiryMatcher{}, true,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	expectRecomputeDesiredTx(mock, "compliance-proxy", "proxy-1")
	expectWriteDesiredAndBumpVer(mock, "proxy-1", 2)
	expectAuditChainGenesis(mock)
	expectInsertAdminAudit(mock)
	mock.ExpectCommit()
	// RePushConfigKey re-fetches the Thing.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("proxy-1").
		WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{"exemptions": exState}, 2))

	err := mgr.handleBreakGlassReport(context.Background(), ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"exemptions": exState},
		ReportedVer:  5,
		KeyVersions:  map[string]int64{"exemptions": 5},
		Reason:       "break_glass",
		SourceIP:     "10.0.0.7",
		ActorTokenID: "a1b2c3d4",
	})
	if err != nil {
		t.Fatalf("handleBreakGlassReport: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected queries (per-Thing path leaked a fleet template write?): %v", err)
	}
}

// TestValidateBreakGlassState_Direct covers the two defensive branches of
// validateBreakGlassState not reachable through ValidateBreakGlassReport's
// allowlist gate: a state that fails json.Marshal, and a key with no registered
// schema (the closed default).
func TestValidateBreakGlassState_Direct(t *testing.T) {
	t.Run("unmarshalable state rejected", func(t *testing.T) {
		err := validateBreakGlassState("killswitch", map[string]any{"engaged": make(chan int)})
		if !errors.Is(err, ErrBreakGlassStateInvalid) {
			t.Fatalf("err = %v, want ErrBreakGlassStateInvalid", err)
		}
	})
	t.Run("key with no schema is closed (default rejects)", func(t *testing.T) {
		err := validateBreakGlassState("not_a_real_key", map[string]any{"x": 1})
		if !errors.Is(err, ErrBreakGlassStateInvalid) {
			t.Fatalf("err = %v, want ErrBreakGlassStateInvalid (closed default)", err)
		}
	})
}

// bgPerThingExemptionsReq is the canonical per-Thing break-glass request used by
// the error-path tests below (a single allowlisted, non-fleet key).
func bgPerThingExemptionsReq() ShadowReportRequest {
	return ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"exemptions": map[string]any{"entries": []any{}}},
		ReportedVer:  5,
		KeyVersions:  map[string]int64{"exemptions": 5},
		Reason:       "break_glass",
		SourceIP:     "10.0.0.7",
		ActorTokenID: "a1b2c3d4",
	}
}

// expectPerThingPrefix programs the mock through GetThing + the template
// baseline lookup for the per-Thing exemptions path. desiredVer<reportedVer so
// the stale guard passes. tmplVer is the template version returned.
func expectPerThingPrefix(mock pgxmock.PgxPoolIface, tmplVer int64) {
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("proxy-1").
		WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{}, 1))
	mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
		WithArgs("compliance-proxy", "exemptions").
		WillReturnRows(pgxmock.NewRows([]string{"type", "config_key", "state", "version", "updated_at", "updated_by"}).
			AddRow("compliance-proxy", "exemptions", []byte(`{"entries":[]}`), tmplVer, now, "admin"))
}

// TestHandleBreakGlassReport_PerThing_ErrorPaths walks each failure point of
// adoptBreakGlassPerThing and asserts the wrapped error surfaces (or, for the
// post-commit push, that the failure is non-fatal).
func TestHandleBreakGlassReport_PerThing_ErrorPaths(t *testing.T) {
	t.Run("template missing", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).WithArgs("proxy-1").
			WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{}, 1))
		mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
			WithArgs("compliance-proxy", "exemptions").
			WillReturnError(pgx.ErrNoRows)
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if !errors.Is(err, ErrTemplateMissing) {
			t.Fatalf("err = %v, want ErrTemplateMissing", err)
		}
	})
	t.Run("template generic err", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		mock.ExpectQuery(`FROM thing t`).WithArgs("proxy-1").
			WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{}, 1))
		mock.ExpectQuery(`FROM thing_config_template\s+WHERE type = \$1 AND config_key = \$2`).
			WithArgs("compliance-proxy", "exemptions").
			WillReturnError(errors.New("planner err"))
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if err == nil || !strings.Contains(err.Error(), "get template") {
			t.Fatalf("err = %v, want get-template wrap", err)
		}
	})
	t.Run("begin tx err", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		expectPerThingPrefix(mock, 2)
		mock.ExpectBegin().WillReturnError(errors.New("conn lost"))
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if err == nil || !strings.Contains(err.Error(), "begin") {
			t.Fatalf("err = %v, want begin wrap", err)
		}
	})
	t.Run("acquire lock err", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		expectPerThingPrefix(mock, 2)
		mock.ExpectBegin()
		mock.ExpectExec(`SELECT pg_advisory_xact_lock\(hashtextextended\(\$1, 0\)\)`).
			WithArgs("compliance-proxy").WillReturnError(errors.New("lock err"))
		mock.ExpectRollback()
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if err == nil || !strings.Contains(err.Error(), "acquire lock") {
			t.Fatalf("err = %v, want acquire-lock wrap", err)
		}
	})
	t.Run("upsert override err", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		expectPerThingPrefix(mock, 2)
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "compliance-proxy")
		mock.ExpectExec(`INSERT INTO thing_config_override`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnError(errors.New("planner err"))
		mock.ExpectRollback()
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if err == nil || !strings.Contains(err.Error(), "upsert override") {
			t.Fatalf("err = %v, want upsert-override wrap", err)
		}
	})
	t.Run("write desired err", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		expectPerThingPrefix(mock, 2)
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "compliance-proxy")
		mock.ExpectExec(`INSERT INTO thing_config_override`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
		expectRecomputeDesiredTx(mock, "compliance-proxy", "proxy-1")
		mock.ExpectQuery(`UPDATE thing\s+SET desired\s+= \$2::jsonb`).
			WithArgs("proxy-1", pgxmock.AnyArg()).
			WillReturnError(errors.New("planner err"))
		mock.ExpectRollback()
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if err == nil || !strings.Contains(err.Error(), "write desired") {
			t.Fatalf("err = %v, want write-desired wrap", err)
		}
	})
	t.Run("audit insert err", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		expectPerThingPrefix(mock, 2)
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "compliance-proxy")
		mock.ExpectExec(`INSERT INTO thing_config_override`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
		expectRecomputeDesiredTx(mock, "compliance-proxy", "proxy-1")
		expectWriteDesiredAndBumpVer(mock, "proxy-1", 2)
		expectAuditChainGenesis(mock)
		mock.ExpectExec(`INSERT INTO "AdminAuditLog"`).
			WillReturnError(errors.New("planner err"))
		mock.ExpectRollback()
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if err == nil || !strings.Contains(err.Error(), "insert audit") {
			t.Fatalf("err = %v, want insert-audit wrap", err)
		}
	})
	t.Run("commit err", func(t *testing.T) {
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		expectPerThingPrefix(mock, 2)
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "compliance-proxy")
		mock.ExpectExec(`INSERT INTO thing_config_override`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
		expectRecomputeDesiredTx(mock, "compliance-proxy", "proxy-1")
		expectWriteDesiredAndBumpVer(mock, "proxy-1", 2)
		expectAuditChainGenesis(mock)
		expectInsertAdminAudit(mock)
		mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
		err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq())
		if err == nil || !strings.Contains(err.Error(), "commit") {
			t.Fatalf("err = %v, want commit wrap", err)
		}
	})
	t.Run("post-commit push failure is non-fatal", func(t *testing.T) {
		// ws nil + mq nil → RePushConfigKey hits ErrNoDeliveryPath after the
		// commit; adoptBreakGlassPerThing logs it and returns nil.
		mgr, mock := newPgxmockManager(t)
		defer mock.Close()
		expectPerThingPrefix(mock, 2)
		mock.ExpectBegin()
		expectConfigVersionLock(mock, "compliance-proxy")
		mock.ExpectExec(`INSERT INTO thing_config_override`).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
		expectRecomputeDesiredTx(mock, "compliance-proxy", "proxy-1")
		expectWriteDesiredAndBumpVer(mock, "proxy-1", 2)
		expectAuditChainGenesis(mock)
		expectInsertAdminAudit(mock)
		mock.ExpectCommit()
		// RePushConfigKey re-fetches the Thing (key present in desired → reaches
		// the no-delivery-path branch).
		mock.ExpectQuery(`FROM thing t`).WithArgs("proxy-1").
			WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy",
				map[string]any{"exemptions": map[string]any{"entries": []any{}}}, 2))
		if err := mgr.handleBreakGlassReport(context.Background(), bgPerThingExemptionsReq()); err != nil {
			t.Fatalf("push failure must be non-fatal, got: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
}

// TestHandleBreakGlassReport_Exemptions_StaleSkipped verifies the per-Thing
// stale-write guard: a report whose version no longer exceeds the Thing's
// desired_ver (an admin write already advanced it) is skipped — only GetThing
// runs, no tx, no override write.
func TestHandleBreakGlassReport_Exemptions_StaleSkipped(t *testing.T) {
	mgr, mock := newPgxmockManager(t)
	defer mock.Close()

	// desired_ver 10; reported 5 <= 10 → skip.
	mock.ExpectQuery(`FROM thing t`).
		WithArgs("proxy-1").
		WillReturnRows(minimalGetThingRow("proxy-1", "compliance-proxy", map[string]any{}, 10))

	err := mgr.handleBreakGlassReport(context.Background(), ShadowReportRequest{
		ID:           "proxy-1",
		Reported:     map[string]any{"exemptions": map[string]any{"entries": []any{}}},
		ReportedVer:  5,
		KeyVersions:  map[string]int64{"exemptions": 5},
		Reason:       "break_glass",
		ActorTokenID: "a1b2c3d4",
	})
	if err != nil {
		t.Fatalf("handleBreakGlassReport: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("stale report must not write anything beyond GetThing: %v", err)
	}
}
