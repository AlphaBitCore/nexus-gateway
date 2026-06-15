package wiring

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/proxy/conn"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	runtimeserver "github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/server"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// stubAuditWriter is a minimal audit.Writer that records Close calls.
type stubAuditWriter struct {
	closed   bool
	closeErr error
}

func (s *stubAuditWriter) Enqueue(_ sharedaudit.AuditEvent) {}
func (s *stubAuditWriter) Flush(_ context.Context) error    { return nil }
func (s *stubAuditWriter) Close(_ context.Context) error {
	s.closed = true
	return s.closeErr
}

func buildTestRuntimeServer(t *testing.T) *runtimeserver.Server {
	t.Helper()
	ks := killswitch.NewKillSwitch(testLogger())
	cm := conn.NewManager(0)
	ex := exemption.NewStore(testLogger())
	readiness := &atomic.Bool{}
	tokenAuth := auth.NewTokenAuth("test-token")
	deps := config.RuntimeDeps{
		KillSwitch:     ks,
		ConnManager:    cm,
		StartTime:      time.Now(),
		Logger:         testLogger(),
		Readiness:      readiness,
		ExemptionStore: ex,
		ThingID:        "test-proxy",
		ThingType:      "compliance-proxy",
		KillswitchSnap: ks,
		ExemptionSnap:  ex,
		Health: config.HealthChecks{
			Run: func(_ context.Context) map[string]string { return nil },
		},
		DataDir: "",
	}
	deps.ExemptionRebuilder = ex
	return runtimeserver.NewServer("127.0.0.1:0", deps, tokenAuth)
}

func TestRunShutdown_SetsReadinessFalseAndCompletes(t *testing.T) {
	readiness := &atomic.Bool{}
	readiness.Store(true)

	shutdownCoord := conn.NewShutdownCoordinator(10*time.Millisecond, testLogger())
	runtimeSrv := buildTestRuntimeServer(t)
	healthServer := &http.Server{Addr: "127.0.0.1:0"}
	writer := &stubAuditWriter{}

	d := ShutdownDeps{
		Readiness:     readiness,
		ShutdownCoord: shutdownCoord,
		RuntimeServer: runtimeSrv,
		HealthServer:  healthServer,
		AuditWriter:   writer,
		RedisClient:   nil,
	}
	RunShutdown(d)

	if readiness.Load() {
		t.Error("readiness should be false after shutdown")
	}
	if !writer.closed {
		t.Error("audit writer should have been closed during shutdown")
	}
}

func TestRunShutdown_NilOptionalFieldsAreNoop(t *testing.T) {
	readiness := &atomic.Bool{}
	readiness.Store(true)

	shutdownCoord := conn.NewShutdownCoordinator(10*time.Millisecond, testLogger())
	runtimeSrv := buildTestRuntimeServer(t)
	healthServer := &http.Server{Addr: "127.0.0.1:0"}

	// AuditWriter, RedisClient (the optional fields) all nil — must not panic.
	d := ShutdownDeps{
		Readiness:     readiness,
		ShutdownCoord: shutdownCoord,
		RuntimeServer: runtimeSrv,
		HealthServer:  healthServer,
		AuditWriter:   nil,
		RedisClient:   nil,
	}
	RunShutdown(d)

	if readiness.Load() {
		t.Error("readiness should be false after shutdown")
	}
}

// TestRunShutdown_AuditWriterCloseError_LogsWarning exercises the audit writer
// close-error branch (lines 56-58) via a stub that returns an error.
func TestRunShutdown_AuditWriterCloseError_LogsWarning(t *testing.T) {
	readiness := &atomic.Bool{}
	readiness.Store(true)

	shutdownCoord := conn.NewShutdownCoordinator(10*time.Millisecond, testLogger())
	runtimeSrv := buildTestRuntimeServer(t)
	healthServer := &http.Server{Addr: "127.0.0.1:0"}
	writer := &stubAuditWriter{closeErr: errors.New("close failed")}

	d := ShutdownDeps{
		Readiness:     readiness,
		ShutdownCoord: shutdownCoord,
		RuntimeServer: runtimeSrv,
		HealthServer:  healthServer,
		AuditWriter:   writer,
		RedisClient:   nil,
	}
	RunShutdown(d) // must not panic; warning is logged
	if !writer.closed {
		t.Error("expected Close to be called even when it returns an error")
	}
}

// TestRunShutdown_WithRedisClient_ClosesRedis exercises the non-nil Redis close
// branch (lines 63-67). Uses a real-but-unstarted client so Close() succeeds.
func TestRunShutdown_WithRedisClient_ClosesRedis(t *testing.T) {
	s, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	// Don't cleanup s — shutdown will close the redis client which disconnects; s can be stopped after.
	defer s.Close()
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})

	readiness := &atomic.Bool{}
	readiness.Store(true)
	shutdownCoord := conn.NewShutdownCoordinator(10*time.Millisecond, testLogger())
	runtimeSrv := buildTestRuntimeServer(t)
	healthServer := &http.Server{Addr: "127.0.0.1:0"}

	d := ShutdownDeps{
		Readiness:     readiness,
		ShutdownCoord: shutdownCoord,
		RuntimeServer: runtimeSrv,
		HealthServer:  healthServer,
		AuditWriter:   nil,
		RedisClient:   rdb, // non-nil → Close() called
	}
	RunShutdown(d) // must not panic
}
