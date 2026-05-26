package wiring

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
)

// TestRunUntilSignal_ServerError_Returns1 verifies the server-error path.
// We start a listener on port N, then attempt to start Echo on the same port
// (bound as ":N" which overlaps 0.0.0.0:N). Echo's Start fails → returns 1.
func TestRunUntilSignal_ServerError_Returns1(t *testing.T) {
	// Grab an ephemeral port and keep the listener open so echo can't bind.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	defer ln.Close() // keep port occupied during the whole test

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	cfg := &config.Config{}
	cfg.Server.Port = port
	cfg.Server.ShutdownTimeout = 100 * time.Millisecond
	cfg.Log.Level = "info"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// RunUntilSignal must return 1 because the port is already in use.
	code := RunUntilSignal(ctx, cancel, e, cfg, silentLogger())
	if code != 1 {
		t.Errorf("expected return code 1 (server error), got %d", code)
	}
}

// TestRunUntilSignal_SignalReceived_Returns0 sends SIGTERM to the process to
// exercise the signal-receive path in RunUntilSignal, verifying it returns 0.
func TestRunUntilSignal_SignalReceived_Returns0(t *testing.T) {
	// Get a free port.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // release so echo can bind

	// Wait briefly to avoid immediate port conflict.
	time.Sleep(10 * time.Millisecond)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	cfg := &config.Config{}
	cfg.Server.Port = port
	cfg.Server.ShutdownTimeout = 200 * time.Millisecond
	cfg.Log.Level = "info"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() {
		done <- RunUntilSignal(ctx, cancel, e, cfg, silentLogger())
	}()

	// Wait for Echo to start listening.
	time.Sleep(150 * time.Millisecond)

	// Send SIGTERM to trigger the quit channel.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("expected return code 0 (signal shutdown), got %d", code)
		}
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("RunUntilSignal did not exit within timeout after SIGTERM")
	}
}
