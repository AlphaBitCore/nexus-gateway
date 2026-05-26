package enrollment

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// callbackResult holds the OAuth callback parameters.
type callbackResult struct {
	Code  string
	State string
	Err   string
}

// callbackServer is a short-lived HTTP server that listens on 127.0.0.1:0
// for a single OAuth callback, then shuts down.
type callbackServer struct {
	ln     net.Listener
	srv    *http.Server
	result chan callbackResult
	once   sync.Once
}

func newCallbackServer() (*callbackServer, error) {
	ln, err := ssoNetListen("tcp", "127.0.0.1:0") //nolint:noctx // callback server lifecycle is per-flow, not request-scoped
	if err != nil {
		return nil, fmt.Errorf("ssoenroll: listen: %w", err)
	}
	s := &callbackServer{
		ln:     ln,
		result: make(chan callbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", s.handleCallback)
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }() //nolint:errcheck
	return s, nil
}

// Port returns the port the server is listening on.
func (s *callbackServer) Port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

// Wait blocks until the OAuth callback is received or ctx is cancelled.
// Returns the code and state on success, or an error on cancellation/timeout.
func (s *callbackServer) Wait(ctx context.Context) (code, state string, err error) {
	select {
	case <-ctx.Done():
		s.Close()
		return "", "", ctx.Err()
	case r := <-s.result:
		if r.Err != "" {
			return "", "", fmt.Errorf("ssoenroll: callback error: %s", r.Err)
		}
		return r.Code, r.State, nil
	}
}

// Close shuts down the server. It performs a graceful shutdown with a
// 500 ms deadline so in-flight response writes (e.g. the HTML body after
// a callback) are flushed before connections are forcefully closed.
func (s *callbackServer) Close() {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	})
}

func (s *callbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	result := callbackResult{
		Code:  q.Get("code"),
		State: q.Get("state"),
		Err:   q.Get("error"),
	}

	select {
	case s.result <- result:
	default:
		// Second callback call — ignore.
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if result.Err != "" {
		fmt.Fprintf(w, "<html><body><h2>Authentication failed: %s</h2><p>You may close this window.</p></body></html>", result.Err) //nolint:errcheck
	} else {
		fmt.Fprint(w, "<html><body><h2>Authentication successful!</h2><p>You may close this window and return to the Nexus Agent.</p></body></html>") //nolint:errcheck
	}

	go s.Close()
}
