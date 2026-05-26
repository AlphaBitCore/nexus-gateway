package conn

import (
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// mockConn is a minimal net.Conn implementation for testing.
// Uses atomic.Bool for the closed flag to avoid data races between
// the idle-timeout goroutine and the test goroutine.
type mockConn struct {
	net.Conn
	readData  []byte
	readErr   error
	writeErr  error
	closed    atomic.Bool
	closeChan chan struct{}
}

func newMockConn() *mockConn {
	return &mockConn{
		readData:  []byte("hello"),
		closeChan: make(chan struct{}, 1),
	}
}

func (m *mockConn) Read(b []byte) (int, error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	n := copy(b, m.readData)
	return n, nil
}

func (m *mockConn) Write(b []byte) (int, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return len(b), nil
}

func (m *mockConn) Close() error {
	m.closed.Store(true)
	select {
	case m.closeChan <- struct{}{}:
	default:
	}
	return nil
}

func (m *mockConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(_ time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(_ time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestIdleConn_TimeoutCloses(t *testing.T) {
	mc := newMockConn()
	ic := NewIdleConn(mc, 50*time.Millisecond)
	_ = ic // keep reference to prevent GC

	select {
	case <-mc.closeChan:
		// Connection was closed by idle timeout — success.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected idle timeout to close connection within 500ms")
	}

	if !mc.closed.Load() {
		t.Fatal("expected underlying connection to be closed")
	}
}

func TestIdleConn_ReadResetsTimer(t *testing.T) {
	mc := newMockConn()
	ic := NewIdleConn(mc, 80*time.Millisecond)

	// Read at 40ms to reset the timer.
	time.Sleep(40 * time.Millisecond)
	buf := make([]byte, 16)
	_, err := ic.Read(buf)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	// At 80ms from start (40ms after read), the timer should not have fired
	// because it was reset to 80ms from the read.
	time.Sleep(40 * time.Millisecond)
	if mc.closed.Load() {
		t.Fatal("connection should still be open after read reset the timer")
	}

	// Wait for the full idle period after the last read.
	select {
	case <-mc.closeChan:
		// Expected: closed after idle timeout from last read.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected idle timeout to eventually close connection")
	}
}

func TestIdleConn_WriteResetsTimer(t *testing.T) {
	mc := newMockConn()
	ic := NewIdleConn(mc, 80*time.Millisecond)

	// Write at 40ms to reset the timer.
	time.Sleep(40 * time.Millisecond)
	_, err := ic.Write([]byte("data"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	// At 80ms from start (40ms after write), the timer should not have fired.
	time.Sleep(40 * time.Millisecond)
	if mc.closed.Load() {
		t.Fatal("connection should still be open after write reset the timer")
	}

	// Wait for the idle period to expire after the last write.
	select {
	case <-mc.closeChan:
		// Expected.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected idle timeout to eventually close connection")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestShutdownCoordinator_GracefulDrain(t *testing.T) {
	sc := NewShutdownCoordinator(2*time.Second, testLogger())

	// Simulate two active connections.
	sc.TrackConnection()
	sc.TrackConnection()

	// Untrack them after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sc.UntrackConnection()
		sc.UntrackConnection()
	}()

	err := sc.Shutdown()
	if err != nil {
		t.Fatalf("expected graceful drain without error, got: %v", err)
	}

	// Context should be cancelled after shutdown.
	select {
	case <-sc.Context().Done():
		// Expected.
	default:
		t.Fatal("expected context to be cancelled after shutdown")
	}
}

func TestIdleConn_CloseIsIdempotent(t *testing.T) {
	// Second Close() must be a no-op so call sites that double-close
	// (defer + explicit close on error) don't double-close the underlying
	// net.Conn or panic on the already-stopped timer.
	mc := newMockConn()
	ic := NewIdleConn(mc, time.Hour) // long timeout so timer never fires

	if err := ic.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !mc.closed.Load() {
		t.Fatal("underlying conn should be closed after first Close")
	}
	// Second Close must return nil and must NOT call the underlying
	// Close again (mc.Close() would re-set closed but the channel push
	// is buffered/skipped; the guarantee we assert is no error).
	if err := ic.Close(); err != nil {
		t.Fatalf("second Close must be no-op, got: %v", err)
	}
}

func TestShutdownCoordinator_ForceClose(t *testing.T) {
	sc := NewShutdownCoordinator(50*time.Millisecond, testLogger())

	// Track a connection that never completes.
	sc.TrackConnection()

	start := time.Now()
	err := sc.Shutdown()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when grace period expires")
	}

	// Should have taken roughly the grace period.
	if elapsed < 40*time.Millisecond {
		t.Fatalf("shutdown returned too quickly: %v", elapsed)
	}

	// Context should be cancelled.
	select {
	case <-sc.Context().Done():
		// Expected.
	default:
		t.Fatal("expected context to be cancelled after force-close")
	}
}
