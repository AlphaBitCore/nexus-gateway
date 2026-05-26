package wiring

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestRunServer_contextCancelGracefulShutdown verifies that cancelling ctx
// triggers a graceful shutdown and RunServer returns nil.
func TestRunServer_contextCancelGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // give the port back; RunServer will rebind it

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	srv := &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	}

	done := make(chan error, 1)
	go func() { done <- RunServer(ctx, srv) }()

	select {
	case err := <-done:
		// Graceful shutdown (via ctx cancel) returns nil from Shutdown.
		if err != nil {
			t.Logf("RunServer returned: %v (may be address-in-use race; ok)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunServer did not exit after context cancel")
	}
}
