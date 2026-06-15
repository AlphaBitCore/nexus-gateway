package selfshadow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// fakePooledListener is a test double for pooledListener. It lets us
// drive the listen loop's Exec / WaitForNotification / Release paths
// without standing up a real Postgres listener or holding a *pgxpool.Conn.
// After the selfshadow adapter-ladder simplification (F-0260d) tests inject
// this directly as what a fakeNotifier.Acquire returns — one layer rather than
// the former two (fakePooledConn → poolConnAdapter).
type fakePooledListener struct {
	execErr   error
	waitErr   error
	waitNotif *pgconnNotification

	execCalls    atomic.Int32
	waitCalls    atomic.Int32
	releaseCalls atomic.Int32
	lastSQL      atomic.Value // string
}

func (f *fakePooledListener) Exec(_ context.Context, sql string) error {
	f.execCalls.Add(1)
	f.lastSQL.Store(sql)
	return f.execErr
}

func (f *fakePooledListener) WaitForNotification(_ context.Context) (*pgconnNotification, error) {
	f.waitCalls.Add(1)
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	return f.waitNotif, nil
}

func (f *fakePooledListener) Release() { f.releaseCalls.Add(1) }

// fakeNotifier is an Acquire factory that returns a pre-built fakePooledListener.
// This is the single test-seam injection point for the listen loop.
type fakeNotifier struct {
	listener   *fakePooledListener
	acquireErr error
}

func (fn *fakeNotifier) Acquire(_ context.Context) (pooledListener, error) {
	if fn.acquireErr != nil {
		return nil, fn.acquireErr
	}
	return fn.listener, nil
}

// Compile-time assertion: fakePooledListener satisfies pooledListener.
var _ pooledListener = (*fakePooledListener)(nil)

// TestFakePooledListener_ExecForwards asserts that fakePooledListener.Exec
// records the SQL and returns nil on success — verifying the test double
// itself is correct.
func TestFakePooledListener_ExecForwards(t *testing.T) {
	f := &fakePooledListener{}
	if err := f.Exec(context.Background(), "LISTEN config_changed"); err != nil {
		t.Fatalf("Exec returned err: %v", err)
	}
	if f.execCalls.Load() != 1 {
		t.Errorf("execCalls = %d, want 1", f.execCalls.Load())
	}
	if got, _ := f.lastSQL.Load().(string); got != "LISTEN config_changed" {
		t.Errorf("lastSQL = %q, want LISTEN config_changed", got)
	}
}

// TestFakePooledListener_ExecSurfacesError verifies the error branch.
func TestFakePooledListener_ExecSurfacesError(t *testing.T) {
	sentinel := errors.New("listen denied")
	f := &fakePooledListener{execErr: sentinel}
	if err := f.Exec(context.Background(), "LISTEN x"); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

// TestFakePooledListener_WaitForNotification_Success verifies the success path:
// Channel and Payload survive the round-trip through fakePooledListener.
func TestFakePooledListener_WaitForNotification_Success(t *testing.T) {
	f := &fakePooledListener{waitNotif: &pgconnNotification{
		Channel: "config_changed",
		Payload: "hub-self",
	}}
	n, err := f.WaitForNotification(context.Background())
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

// TestFakePooledListener_WaitForNotification_Error verifies the error branch.
func TestFakePooledListener_WaitForNotification_Error(t *testing.T) {
	sentinel := errors.New("conn broken")
	f := &fakePooledListener{waitErr: sentinel}
	n, err := f.WaitForNotification(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
	if n != nil {
		t.Errorf("notification on err must be nil; got %+v", n)
	}
}

// TestFakePooledListener_ReleaseForwards verifies Release is tracked.
func TestFakePooledListener_ReleaseForwards(t *testing.T) {
	f := &fakePooledListener{}
	f.Release()
	if f.releaseCalls.Load() != 1 {
		t.Errorf("releaseCalls = %d, want 1", f.releaseCalls.Load())
	}
}
