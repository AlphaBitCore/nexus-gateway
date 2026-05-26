package tlsbump

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

const passthroughDialTimeout = 10 * time.Second

// PassThrough relays raw TCP traffic bidirectionally between client and upstream
// without TLS interception. Used when bump fails due to certificate pinning or
// the target is explicitly exempted from interception.
func PassThrough(ctx context.Context, clientConn net.Conn, targetHost string) error {
	dialer := net.Dialer{Timeout: passthroughDialTimeout}
	upstreamConn, err := dialer.DialContext(ctx, "tcp", targetHost)
	if err != nil {
		return &PassThroughError{Op: "dial", Host: targetHost, Err: err}
	}

	// ctx cancellation closes both connections, which unblocks io.Copy.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = clientConn.Close()
			_ = upstreamConn.Close()
		case <-done:
		}
	}()

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	recordErr := func(e error) {
		if e != nil {
			errOnce.Do(func() { firstErr = e })
		}
	}

	wg.Add(2)

	// client → upstream
	go func() {
		defer wg.Done()
		_, err := io.Copy(upstreamConn, clientConn)
		recordErr(err)
		// Half-close: signal upstream that no more data is coming.
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	// upstream → client
	go func() {
		defer wg.Done()
		_, err := io.Copy(clientConn, upstreamConn)
		recordErr(err)
		// Half-close: signal client that no more data is coming.
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()
	close(done)

	// Close upstream; clientConn is closed by the caller (listener.go defer).
	_ = upstreamConn.Close()

	return firstErr
}

// PassThroughError wraps errors from the pass-through relay.
type PassThroughError struct {
	Op   string // "dial", "copy"
	Host string
	Err  error
}

func (e *PassThroughError) Error() string {
	return "passthrough " + e.Op + " " + e.Host + ": " + e.Err.Error()
}

func (e *PassThroughError) Unwrap() error { return e.Err }
