package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// errInjected is a sentinel DB error used to drive the ledger/reconcile error paths.
var errInjected = errors.New("injected db error")

func newLedgerMock(t *testing.T) (*Ledger, pgxmock.PgxPoolIface) {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	t.Cleanup(m.Close)
	return NewLedger(m), m
}

// pushServer is an httptest stand-in for Hub's /api/hub/config/update endpoint.
// It counts pushes and returns the configured status so the Client integration
// tests can drive both the success and failure propagation paths.
func pushServer(t *testing.T, status int, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/config/update" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		atomic.AddInt32(hits, 1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(ConfigChangeResponse{OK: true, Version: 1})
	}))
}

func TestNewLedger_nilPoolYieldsNil(t *testing.T) {
	if NewLedger(nil) != nil {
		t.Fatal("NewLedger(nil) must return nil so callers can treat it as no backstop")
	}
}

func TestLedger_nilReceiverMethodsAreNoOps(t *testing.T) {
	var l *Ledger
	if seq, err := l.RecordIntent(context.Background(), "t", "k"); err != nil || seq != 0 {
		t.Fatalf("nil RecordIntent = %d,%v", seq, err)
	}
	if err := l.MarkAcked(context.Background(), "t", "k", 5); err != nil {
		t.Fatalf("nil MarkAcked = %v", err)
	}
	if got, err := l.ListPending(context.Background()); err != nil || got != nil {
		t.Fatalf("nil ListPending = %v,%v", got, err)
	}
}

func TestLedger_RecordIntentReturnsBumpedSeq(t *testing.T) {
	l, m := newLedgerMock(t)
	m.ExpectQuery(`INSERT INTO system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:credentials", "ai-gateway", "credentials").
		WillReturnRows(pgxmock.NewRows([]string{"intended"}).AddRow(int64(4)))
	seq, err := l.RecordIntent(context.Background(), "ai-gateway", "credentials")
	if err != nil || seq != 4 {
		t.Fatalf("RecordIntent = %d,%v want 4,nil", seq, err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLedger_MarkAckedUpdatesRow(t *testing.T) {
	l, m := newLedgerMock(t)
	m.ExpectExec(`UPDATE system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:virtual_keys", int64(9)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := l.MarkAcked(context.Background(), "ai-gateway", "virtual_keys", 9); err != nil {
		t.Fatalf("MarkAcked: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLedger_ListPendingReturnsLaggingKeys(t *testing.T) {
	l, m := newLedgerMock(t)
	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnRows(pgxmock.NewRows([]string{"thingType", "configKey", "intended"}).
			AddRow("ai-gateway", "routing_rules", int64(3)).
			AddRow("ai-gateway", "quota_policies", int64(1)))
	pending, err := l.ListPending(context.Background())
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 2 || pending[0].ConfigKey != "routing_rules" || pending[0].IntendedSeq != 3 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// F-0345: a successful Category-B push records intent then stamps the ack, so
// the key is not left pending.
func TestInvalidateConfigE_ledgerSuccessRecordsAck(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusOK, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)

	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer m.Close()
	c.SetLedger(NewLedger(m))

	m.ExpectQuery(`INSERT INTO system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:credentials", "ai-gateway", "credentials").
		WillReturnRows(pgxmock.NewRows([]string{"intended"}).AddRow(int64(1)))
	m.ExpectExec(`UPDATE system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:credentials", int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "credentials"); err != nil {
		t.Fatalf("InvalidateConfigE: %v", err)
	}
	if hits != 1 {
		t.Fatalf("push hits = %d, want 1", hits)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// F-0345 + F-0099: when the push fails the intent is recorded but the ack is NOT
// stamped (no ExpectExec), so the key stays pending for the reconcile arm — and
// the error surfaces (handler returns 502).
func TestInvalidateConfigE_ledgerFailureLeavesPending(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusInternalServerError, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)

	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer m.Close()
	c.SetLedger(NewLedger(m))

	m.ExpectQuery(`INSERT INTO system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:routing_rules", "ai-gateway", "routing_rules").
		WillReturnRows(pgxmock.NewRows([]string{"intended"}).AddRow(int64(1)))
	// No ExpectExec: MarkAcked must NOT run when the push fails.

	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "routing_rules"); err == nil {
		t.Fatal("expected propagation error to surface, got nil")
	}
	if hits == 0 {
		t.Fatal("push was never attempted")
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// F-0345: the reconcile arm re-pushes a pending key and stamps the ack on
// success.
func TestReconcilePending_repushesPendingKey(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusOK, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)

	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer m.Close()
	c.SetLedger(NewLedger(m))

	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnRows(pgxmock.NewRows([]string{"thingType", "configKey", "intended"}).
			AddRow("ai-gateway", "credentials", int64(2)))
	m.ExpectExec(`UPDATE system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:credentials", int64(2)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	healed, err := c.ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if healed != 1 {
		t.Fatalf("healed = %d, want 1", healed)
	}
	if hits != 1 {
		t.Fatalf("push hits = %d, want 1", hits)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// F-0345: an up-to-date fleet (no pending keys) triggers no re-push.
func TestReconcilePending_noPendingNoPush(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusOK, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)

	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer m.Close()
	c.SetLedger(NewLedger(m))

	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnRows(pgxmock.NewRows([]string{"thingType", "configKey", "intended"}))

	healed, err := c.ReconcilePending(context.Background())
	if err != nil || healed != 0 {
		t.Fatalf("ReconcilePending = %d,%v want 0,nil", healed, err)
	}
	if hits != 0 {
		t.Fatalf("an up-to-date key must not be re-pushed; hits = %d", hits)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A nil ledger (dev/no-DB) degrades to a plain fire-once push that still
// succeeds, and ReconcilePending is a no-op.
func TestInvalidateConfigE_noLedgerStillPushes(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusOK, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)

	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "models"); err != nil {
		t.Fatalf("InvalidateConfigE without ledger: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
	if healed, err := c.ReconcilePending(context.Background()); err != nil || healed != 0 {
		t.Fatalf("ReconcilePending without ledger = %d,%v want 0,nil", healed, err)
	}
}

// ErrNotConfigured (dev Hub) records no intent and returns nil — there is
// nothing to reconcile.
func TestInvalidateConfigE_notConfiguredNoIntent(t *testing.T) {
	c := New("", "tok", http.DefaultClient, nil)
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer m.Close()
	c.SetLedger(NewLedger(m))
	// RecordIntent runs first (the ledger does not know Hub is unconfigured),
	// but the push maps ErrNotConfigured to success, so no ack/extra calls.
	m.ExpectQuery(`INSERT INTO system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:models", "ai-gateway", "models").
		WillReturnRows(pgxmock.NewRows([]string{"intended"}).AddRow(int64(1)))
	m.ExpectExec(`UPDATE system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:models", int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "models"); err != nil {
		t.Fatalf("InvalidateConfigE: %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// --- Ledger error-path coverage ---

func TestLedger_RecordIntentWrapsDBError(t *testing.T) {
	l, m := newLedgerMock(t)
	m.ExpectQuery(`INSERT INTO system_metadata`).
		WithArgs("propagation_ledger:t:k", "t", "k").
		WillReturnError(errInjected)
	if _, err := l.RecordIntent(context.Background(), "t", "k"); err == nil {
		t.Fatal("expected wrapped error")
	}
}

func TestLedger_MarkAckedWrapsDBError(t *testing.T) {
	l, m := newLedgerMock(t)
	m.ExpectExec(`UPDATE system_metadata`).
		WithArgs("propagation_ledger:t:k", int64(1)).
		WillReturnError(errInjected)
	if err := l.MarkAcked(context.Background(), "t", "k", 1); err == nil {
		t.Fatal("expected wrapped error")
	}
}

func TestLedger_ListPendingWrapsQueryError(t *testing.T) {
	l, m := newLedgerMock(t)
	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnError(errInjected)
	if _, err := l.ListPending(context.Background()); err == nil {
		t.Fatal("expected wrapped error")
	}
}

func TestLedger_ListPendingScanError(t *testing.T) {
	l, m := newLedgerMock(t)
	// intended column delivered as a non-numeric value → Scan into int64 fails.
	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnRows(pgxmock.NewRows([]string{"thingType", "configKey", "intended"}).
			AddRow("t", "k", "not-an-int"))
	if _, err := l.ListPending(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestLedger_ListPendingRowError(t *testing.T) {
	l, m := newLedgerMock(t)
	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnRows(pgxmock.NewRows([]string{"thingType", "configKey", "intended"}).
			AddRow("t", "k", int64(1)).RowError(0, errInjected))
	if _, err := l.ListPending(context.Background()); err == nil {
		t.Fatal("expected row iteration error")
	}
}

// --- Client ledger error-path coverage ---

// A ledger record-intent failure must not block the live push: InvalidateConfigE
// falls back to a plain fire-once push.
func TestInvalidateConfigE_recordIntentFailureFallsBackToPush(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusOK, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)
	m, _ := pgxmock.NewPool()
	defer m.Close()
	c.SetLedger(NewLedger(m))
	m.ExpectQuery(`INSERT INTO system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:credentials", "ai-gateway", "credentials").
		WillReturnError(errInjected)
	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "credentials"); err != nil {
		t.Fatalf("fallback push should succeed: %v", err)
	}
	if hits != 1 {
		t.Fatalf("push hits = %d, want 1", hits)
	}
}

// A mark-acked failure after a successful push is logged, not surfaced — the
// admin write succeeded; the worst case is a redundant reconcile re-push.
func TestInvalidateConfigE_markAckedFailureIsNonFatal(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusOK, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)
	m, _ := pgxmock.NewPool()
	defer m.Close()
	c.SetLedger(NewLedger(m))
	m.ExpectQuery(`INSERT INTO system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:models", "ai-gateway", "models").
		WillReturnRows(pgxmock.NewRows([]string{"intended"}).AddRow(int64(1)))
	m.ExpectExec(`UPDATE system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:models", int64(1)).
		WillReturnError(errInjected)
	if err := c.InvalidateConfigE(context.Background(), "ai-gateway", "models"); err != nil {
		t.Fatalf("mark-acked failure must not surface: %v", err)
	}
}

func TestReconcilePending_listError(t *testing.T) {
	c := New("http://unused", "tok", http.DefaultClient, nil)
	m, _ := pgxmock.NewPool()
	defer m.Close()
	c.SetLedger(NewLedger(m))
	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnError(errInjected)
	if _, err := c.ReconcilePending(context.Background()); err == nil {
		t.Fatal("expected list error to surface")
	}
}

// A re-push that fails leaves the key pending (not acked) and is counted as not
// healed; the loop continues.
func TestReconcilePending_repushFailureSkips(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusInternalServerError, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)
	m, _ := pgxmock.NewPool()
	defer m.Close()
	c.SetLedger(NewLedger(m))
	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnRows(pgxmock.NewRows([]string{"thingType", "configKey", "intended"}).
			AddRow("ai-gateway", "credentials", int64(1)))
	// No ExpectExec: a failed re-push must NOT mark acked.
	healed, err := c.ReconcilePending(context.Background())
	if err != nil || healed != 0 {
		t.Fatalf("ReconcilePending = %d,%v want 0,nil", healed, err)
	}
}

// A mark-acked failure during reconcile skips that key (not counted healed) and
// continues.
func TestReconcilePending_markAckedFailureSkips(t *testing.T) {
	var hits int32
	ts := pushServer(t, http.StatusOK, &hits)
	defer ts.Close()
	c := New(ts.URL, "tok", ts.Client(), nil)
	m, _ := pgxmock.NewPool()
	defer m.Close()
	c.SetLedger(NewLedger(m))
	m.ExpectQuery(`SELECT value->>'thingType'`).
		WithArgs("propagation_ledger:%").
		WillReturnRows(pgxmock.NewRows([]string{"thingType", "configKey", "intended"}).
			AddRow("ai-gateway", "credentials", int64(1)))
	m.ExpectExec(`UPDATE system_metadata`).
		WithArgs("propagation_ledger:ai-gateway:credentials", int64(1)).
		WillReturnError(errInjected)
	healed, err := c.ReconcilePending(context.Background())
	if err != nil || healed != 0 {
		t.Fatalf("ReconcilePending = %d,%v want 0,nil", healed, err)
	}
}
