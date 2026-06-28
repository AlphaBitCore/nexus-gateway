package assistant

import (
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
)

// TestPendingConfirmStore_NilWhenUnavailable: no pool / no userId → nil store, and all
// methods are safe no-ops (the in-memory registry is the source of truth).
func TestPendingConfirmStore_NilWhenUnavailable(t *testing.T) {
	if newPendingConfirmStore(nil, "alice") != nil {
		t.Fatal("a nil pool must yield a nil store")
	}
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	if newPendingConfirmStore(mock, "") != nil {
		t.Fatal("a blank userId must yield a nil store")
	}
	var s *pendingConfirmStore // nil receiver — methods must not panic
	s.put(context.Background(), "k", "s", "c", "tool", nil, "r", false, false)
	s.del(context.Background(), "k")
	if s.fresh(context.Background(), "k") {
		t.Fatal("a nil store is never fresh")
	}
}

// TestPendingConfirmStore_PutInsertsAndReaps: put writes the row (ON CONFLICT DO
// NOTHING) and reaps this user's stale orphans, scoped to userId (I3).
func TestPendingConfirmStore_PutInsertsAndReaps(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO "AssistantPendingConfirm"`).
		WithArgs("alice:s1:c1", "alice", "s1", "c1", "set_kill_switch", []byte(`{"engaged":true}`), "stop traffic", true, true).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`DELETE FROM "AssistantPendingConfirm" WHERE "userId" = \$1 AND "createdAt" < \$2`).
		WithArgs("alice", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	s := newPendingConfirmStore(mock, "alice")
	s.put(context.Background(), "alice:s1:c1", "s1", "c1", "set_kill_switch", json.RawMessage(`{"engaged":true}`), "stop traffic", true, true)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("put must INSERT then reap: %v", err)
	}
}

// TestPendingConfirmStore_DelScopedToUser deletes by key AND userId so one principal
// can never delete another's row.
func TestPendingConfirmStore_DelScopedToUser(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectExec(`DELETE FROM "AssistantPendingConfirm" WHERE "key" = \$1 AND "userId" = \$2`).
		WithArgs("alice:s1:c1", "alice").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	newPendingConfirmStore(mock, "alice").del(context.Background(), "alice:s1:c1")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("del must be scoped to key+userId: %v", err)
	}
}

// TestPendingConfirmStore_Fresh reports a still-recent orphan (restart → re-issue) and
// fails closed on a DB error.
func TestPendingConfirmStore_Fresh(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("alice:s1:c1", "alice", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	if !newPendingConfirmStore(mock, "alice").fresh(context.Background(), "alice:s1:c1") {
		t.Fatal("a present fresh row must read as fresh (restart → re-issue)")
	}

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("alice:s1:c1", "alice", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	if newPendingConfirmStore(mock, "alice").fresh(context.Background(), "alice:s1:c1") {
		t.Fatal("an absent/stale row must read as not-fresh (expired)")
	}

	mock.ExpectQuery(`SELECT EXISTS`).WillReturnError(context.DeadlineExceeded)
	if newPendingConfirmStore(mock, "alice").fresh(context.Background(), "alice:s1:c1") {
		t.Fatal("a DB error must fail closed (not fabricate a re-issue)")
	}
}

// confirmReq builds a POST /confirm echo context for an Allow on (userId, sessionId,
// callId) with the bearer principal resolved.
func confirmReq(userID, sessionID, callID string) (*httptest.ResponseRecorder, echo.Context) {
	e := echo.New()
	body := `{"sessionId":"` + sessionID + `","callId":"` + callID + `","decision":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/assistant/confirm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("adminAuth", &auth.AdminAuth{KeyID: userID})
	return rec, c
}

// TestConfirm_RestartReissueVsExpired is the user-facing payoff: an in-memory
// miss (no parked channel) with a still-fresh durable orphan → restart_reissue; with no
// row → plain expired. Distinguishing the two is the whole point of persisting.
func TestConfirm_RestartReissueVsExpired(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	h := New(Config{Pool: mock}) // no Redis → nil owner registry, so no 421 short-circuit

	// A fresh orphan row exists for alice:s1:c1 → the pod restarted → re-issue.
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("alice:s1:c1", "alice", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	rec, c := confirmReq("alice", "s1", "c1")
	if err := h.Confirm(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "restart_reissue") {
		t.Fatalf("a fresh orphan must be restart_reissue 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// No row for alice:s1:c2 → genuinely expired.
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("alice:s1:c2", "alice", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	rec2, c2 := confirmReq("alice", "s1", "c2")
	if err := h.Confirm(c2); err != nil {
		t.Fatal(err)
	}
	if rec2.Code != http.StatusConflict || !strings.Contains(rec2.Body.String(), "expired") {
		t.Fatalf("no orphan must be expired 409, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "restart_reissue") {
		t.Fatal("absent orphan must NOT be restart_reissue")
	}
}

// runMakeConfirm starts makeConfirm for one tool in a goroutine and returns the parked
// callId (from the emitted confirm event) plus a channel that yields the final decision.
func runMakeConfirm(h *Handler, ctx context.Context, userID, sessionID string) (callID string, decision <-chan bool) {
	idc := make(chan string, 1)
	send := func(ev string, payload any) {
		if ev == "confirm" {
			idc <- payload.(map[string]any)["callId"].(string)
		}
	}
	cf := h.makeConfirm(userID, sessionID, send)
	out := make(chan bool, 1)
	go func() {
		ok, _ := cf(ctx, fakeConfirmTool{}, json.RawMessage(`{"engaged":true}`), "because")
		out <- ok
	}()
	return <-idc, out
}

// TestMakeConfirm_PersistsThenDeletesOnResolve drives the pool-backed lifecycle: a
// register INSERTs (+ reaps the user's stale orphans), and a normal resolve DELETEs the
// row by key+userId — so a normal flow leaves NO row (no false restart_reissue later).
func TestMakeConfirm_PersistsThenDeletesOnResolve(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.MatchExpectationsInOrder(false)
	h := New(Config{Pool: mock})

	mock.ExpectExec(`INSERT INTO "AssistantPendingConfirm"`).
		WithArgs(pgxmock.AnyArg(), "alice", "s1", pgxmock.AnyArg(), "mitigate_kill_switch", pgxmock.AnyArg(), "because", false, false).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`DELETE FROM "AssistantPendingConfirm" WHERE "userId" = \$1 AND "createdAt" < \$2`).
		WithArgs("alice", pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "AssistantPendingConfirm" WHERE "key" = \$1 AND "userId" = \$2`).
		WithArgs(pgxmock.AnyArg(), "alice").WillReturnResult(pgxmock.NewResult("DELETE", 1))

	callID, decision := runMakeConfirm(h, context.Background(), "alice", "s1")
	h.confirms.decide("alice:s1:"+callID, true, "")
	if !<-decision {
		t.Fatal("an Allow must resolve true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("register must INSERT+reap and resolve must DELETE the row: %v", err)
	}
}

// TestMakeConfirm_DeletesOnCancel proves the detached-ctx cleanup: when the turn ctx is
// cancelled (interrupt / disconnect-grace), the deferred del still removes the row even
// though the turn ctx is dead.
func TestMakeConfirm_DeletesOnCancel(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.MatchExpectationsInOrder(false)
	h := New(Config{Pool: mock})

	mock.ExpectExec(`INSERT INTO "AssistantPendingConfirm"`).
		WithArgs(pgxmock.AnyArg(), "alice", "s1", pgxmock.AnyArg(), "mitigate_kill_switch", pgxmock.AnyArg(), "because", false, false).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`DELETE FROM "AssistantPendingConfirm" WHERE "userId" = \$1 AND "createdAt" < \$2`).
		WithArgs("alice", pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "AssistantPendingConfirm" WHERE "key" = \$1 AND "userId" = \$2`).
		WithArgs(pgxmock.AnyArg(), "alice").WillReturnResult(pgxmock.NewResult("DELETE", 1))

	ctx, cancel := context.WithCancel(context.Background())
	_, decision := runMakeConfirm(h, ctx, "alice", "s1")
	cancel() // turn cancelled (interrupt / grace) → fail-safe deny + deferred cleanup
	if <-decision {
		t.Fatal("a cancelled confirm must fail-safe deny")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a cancelled confirm must still DELETE its durable row via the detached ctx: %v", err)
	}
}
