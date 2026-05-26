package selfshadow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakePooledConn drives the poolConnAdapter through its three methods
// without standing up a real Postgres listener. Mirrors the cp/store
// pgxmock seam pattern used 13+ times elsewhere in the repo: the
// adapter holds a *pooledConn* interface field, the wrapper around
// *pgxpool.Conn satisfies it in production, and this fake satisfies it
// in tests so Exec / WaitForNotification / Release become unit-testable.
type fakePooledConn struct {
	execErr      error
	waitErr      error
	waitNotif    *pgconn.Notification
	execCalls    atomic.Int32
	waitCalls    atomic.Int32
	releaseCalls atomic.Int32
	lastSQL      atomic.Value // string
}

func (f *fakePooledConn) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.execCalls.Add(1)
	f.lastSQL.Store(sql)
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakePooledConn) WaitForNotification(_ context.Context) (*pgconn.Notification, error) {
	f.waitCalls.Add(1)
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return f.waitNotif, nil
}

func (f *fakePooledConn) Release() { f.releaseCalls.Add(1) }

// TestPoolConnAdapter_ExecForwardsToInner asserts that poolConnAdapter.Exec
// forwards the SQL string to the underlying pooledConn and returns nil on
// success. This is the production wiring's `_, err := c.conn.Exec(...)`
// happy path, previously only reachable against a live Postgres.
func TestPoolConnAdapter_ExecForwardsToInner(t *testing.T) {
	fc := &fakePooledConn{}
	a := &poolConnAdapter{conn: fc}

	if err := a.Exec(context.Background(), "LISTEN config_changed"); err != nil {
		t.Fatalf("Exec returned err: %v", err)
	}
	if fc.execCalls.Load() != 1 {
		t.Errorf("Exec call count = %d, want 1", fc.execCalls.Load())
	}
	if got, _ := fc.lastSQL.Load().(string); got != "LISTEN config_changed" {
		t.Errorf("Exec SQL = %q, want LISTEN config_changed", got)
	}
}

// TestPoolConnAdapter_ExecSurfacesError asserts the err-branch: a failure
// inside the inner Exec propagates back to the listen loop so it can bail
// out and reacquire (covered by TestListen_ExecLISTENFailureReacquires).
func TestPoolConnAdapter_ExecSurfacesError(t *testing.T) {
	sentinel := errors.New("listen denied")
	fc := &fakePooledConn{execErr: sentinel}
	a := &poolConnAdapter{conn: fc}

	if err := a.Exec(context.Background(), "LISTEN x"); !errors.Is(err, sentinel) {
		t.Errorf("Exec err = %v, want %v", err, sentinel)
	}
}

// TestPoolConnAdapter_WaitForNotificationProjects asserts the success
// branch: the inner *pgconn.Notification is projected onto the package's
// internal pgconnNotification shape so the listen loop never imports
// pgconn directly. Channel + Payload must survive the projection.
func TestPoolConnAdapter_WaitForNotificationProjects(t *testing.T) {
	fc := &fakePooledConn{waitNotif: &pgconn.Notification{
		PID:     42,
		Channel: "config_changed",
		Payload: "hub-self",
	}}
	a := &poolConnAdapter{conn: fc}

	n, err := a.WaitForNotification(context.Background())
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if n == nil {
		t.Fatal("WaitForNotification returned nil notification")
	}
	if n.Channel != "config_changed" {
		t.Errorf("Channel = %q, want config_changed", n.Channel)
	}
	if n.Payload != "hub-self" {
		t.Errorf("Payload = %q, want hub-self", n.Payload)
	}
}

// TestPoolConnAdapter_WaitForNotificationSurfacesError asserts the err
// branch: a failure inside WaitForNotification returns (nil, err) so the
// listen loop can release the conn and reacquire.
func TestPoolConnAdapter_WaitForNotificationSurfacesError(t *testing.T) {
	sentinel := errors.New("conn broken")
	fc := &fakePooledConn{waitErr: sentinel}
	a := &poolConnAdapter{conn: fc}

	n, err := a.WaitForNotification(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	if n != nil {
		t.Errorf("notification on err must be nil; got %+v", n)
	}
}

// TestPoolConnAdapter_ReleaseForwards asserts the trivial Release plumbing
// — the inner conn's Release is called exactly once.
func TestPoolConnAdapter_ReleaseForwards(t *testing.T) {
	fc := &fakePooledConn{}
	a := &poolConnAdapter{conn: fc}
	a.Release()
	if fc.releaseCalls.Load() != 1 {
		t.Errorf("Release call count = %d, want 1", fc.releaseCalls.Load())
	}
}

// fakeNotifierForAcquire pretends to be a notifier so we can drive
// poolAdapter.Acquire's success path. The real poolAdapter holds a
// *pgxpool.Pool; covering its success branch via the real type requires a
// live Postgres. Instead, we directly construct &poolConnAdapter{conn: …}
// here in a separate test to lock down the wiring shape: the listen loop
// expects a *poolConnAdapter and the adapter expects a pooledConn field.
func TestPoolConnAdapter_FieldShapeWiring(t *testing.T) {
	// Compile-time assertion mirrored at runtime: an instance built with
	// only the pooledConn seam must satisfy pooledListener and round-trip
	// every method without nil-pointer panics. This protects against
	// future refactors that might inadvertently re-introduce a hard
	// dependency on *pgxpool.Conn inside poolConnAdapter.
	var _ pooledListener = (*poolConnAdapter)(nil)

	fc := &fakePooledConn{waitNotif: &pgconn.Notification{Channel: "c", Payload: "p"}}
	a := &poolConnAdapter{conn: fc}

	if err := a.Exec(context.Background(), "X"); err != nil {
		t.Errorf("Exec: %v", err)
	}
	if _, err := a.WaitForNotification(context.Background()); err != nil {
		t.Errorf("WaitForNotification: %v", err)
	}
	a.Release()

	if fc.execCalls.Load() != 1 || fc.waitCalls.Load() != 1 || fc.releaseCalls.Load() != 1 {
		t.Errorf("call counts unexpected: exec=%d wait=%d release=%d",
			fc.execCalls.Load(), fc.waitCalls.Load(), fc.releaseCalls.Load())
	}
}
