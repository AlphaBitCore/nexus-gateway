package conn

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// IdleConn wraps a net.Conn with an idle timeout timer.
// Every successful Read or Write resets the timer. When the timer fires,
// the underlying connection is closed automatically.
type IdleConn struct {
	net.Conn
	timer   *time.Timer
	timeout time.Duration
	mu      sync.Mutex
	closed  bool
}

// NewIdleConn wraps conn with an idle timeout. If no Read or Write occurs
// within the timeout duration, the connection is closed.
func NewIdleConn(conn net.Conn, timeout time.Duration) *IdleConn {
	ic := &IdleConn{
		Conn:    conn,
		timeout: timeout,
	}
	// Write ic.timer under the mutex so the AfterFunc goroutine (which
	// reads ic.timer inside Close under the same mutex) has a proper
	// happens-before guarantee per the Go memory model.
	ic.mu.Lock()
	ic.timer = time.AfterFunc(timeout, func() {
		_ = ic.Close()
	})
	ic.mu.Unlock()
	return ic
}

// Read resets the idle timer and delegates to the underlying connection.
func (c *IdleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.resetTimer()
	}
	return n, err
}

// Write resets the idle timer and delegates to the underlying connection.
func (c *IdleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.resetTimer()
	}
	return n, err
}

// Close closes the connection and stops the idle timer. It is safe to call
// multiple times; subsequent calls are no-ops.
func (c *IdleConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.timer.Stop()
	return c.Conn.Close()
}

func (c *IdleConn) resetTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.timer.Reset(c.timeout)
	}
}

// ShutdownCoordinator manages graceful shutdown of the proxy.
// It tracks active connections via a WaitGroup and enforces a grace period
// after which remaining connections are force-closed via context cancellation.
type ShutdownCoordinator struct {
	wg           sync.WaitGroup
	ctx          context.Context
	cancel       context.CancelFunc
	grace        time.Duration
	logger       *slog.Logger
	shutdownOnce sync.Once
	shutdownErr  error
}

// NewShutdownCoordinator creates a coordinator with the given grace period.
// The returned context is cancelled either when Shutdown completes or when
// the grace period expires, whichever comes first.
func NewShutdownCoordinator(grace time.Duration, logger *slog.Logger) *ShutdownCoordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &ShutdownCoordinator{
		ctx:    ctx,
		cancel: cancel,
		grace:  grace,
		logger: logger,
	}
}

// TrackConnection registers a new connection for graceful drain tracking.
func (s *ShutdownCoordinator) TrackConnection() {
	s.wg.Add(1)
}

// UntrackConnection marks a tracked connection as completed.
func (s *ShutdownCoordinator) UntrackConnection() {
	s.wg.Done()
}

// Shutdown initiates graceful shutdown. It waits for all tracked connections
// to complete up to the grace period, then cancels the context to signal
// force-close to any remaining handlers. Safe to call multiple times;
// subsequent calls return the result of the first invocation.
func (s *ShutdownCoordinator) Shutdown() error {
	s.shutdownOnce.Do(func() {
		s.logger.Info("shutdown initiated, draining connections", "grace_period", s.grace)

		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			s.logger.Info("all connections drained gracefully")
			s.cancel()
		case <-time.After(s.grace):
			s.logger.Warn("grace period expired, force-closing remaining connections")
			s.cancel()
			s.shutdownErr = fmt.Errorf("shutdown grace period (%s) expired with connections still active", s.grace)
		}
	})
	return s.shutdownErr
}

// Context returns the shutdown context. Connection handlers should select on
// this context to detect force-close signals.
func (s *ShutdownCoordinator) Context() context.Context {
	return s.ctx
}
