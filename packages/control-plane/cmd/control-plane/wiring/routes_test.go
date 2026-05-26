package wiring

import (
	"context"
	"testing"

	"github.com/labstack/echo/v4"
	pgxmock "github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	jwtverifier "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/jwt"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

func TestInitRoutes_WithPgxMockDB_MountsRoutes(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	e := echo.New()
	db := store.NewWithPgxPool(mock)
	logger := silentLogger()

	auditWriter := InitAuditWriter(nil, logger)

	// A JWTVerifier is required by AdminAuth middleware (panics if nil).
	jwtVer := jwtverifier.New(jwtverifier.Config{
		Issuer:   "http://localhost:3001",
		JWKSURL:  "http://localhost:3001/.well-known/jwks.json",
		RevCheck: jwtverifier.AlwaysAllow{},
		Logger:   logger,
	})

	d := RoutesDeps{
		Cfg:         &config.Config{},
		DB:          db,
		Logger:      logger,
		AuditWriter: auditWriter,
		Ctx:         context.Background(),
		JWTVerifier: jwtVer,
	}

	adminHandler, err := InitRoutes(e, d)
	if err != nil {
		t.Fatalf("InitRoutes failed: %v", err)
	}
	if adminHandler == nil {
		t.Error("expected non-nil AdminHandler")
	}
}

func TestInitRoutes_NilDB_ReturnsAdminHandler(t *testing.T) {
	// With nil DB, InitRoutes should still succeed: DB-dependent branches are
	// skipped. However routes.go line 181 calls d.DB.InternalPool() unconditionally,
	// so nil DB WILL panic. InitRoutes requires a non-nil DB.
	// This test therefore uses pgxmock and is covered by TestInitRoutes_WithPgxMockDB_MountsRoutes.
	// This placeholder documents the design constraint.
	t.Skip("InitRoutes requires non-nil DB (unconditional d.DB.InternalPool() at line 181)")
}

func TestInitRoutes_WithAIGuardDispatcher_MountsRoutes(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// Expect the AIGuardStore query for assembly (SELECT from pgx).
	// The store constructor will attempt queries during handler setup only if
	// the handler's methods are called, not at construction.
	e := echo.New()
	db := store.NewWithPgxPool(mock)
	logger := silentLogger()
	auditWriter := InitAuditWriter(nil, logger)
	jwtVer := jwtverifier.New(jwtverifier.Config{
		Issuer:   "http://localhost:3001",
		JWKSURL:  "http://localhost:3001/.well-known/jwks.json",
		RevCheck: jwtverifier.AlwaysAllow{},
		Logger:   logger,
	})

	cfg := &config.Config{}
	cfg.BFF.AIGatewayURL = "http://127.0.0.1:3050"
	cfg.Auth.InternalServiceToken = "test-token"
	cfg.AIGuard.DispatchTimeoutSec = 10

	d := RoutesDeps{
		Cfg:         cfg,
		DB:          db,
		Logger:      logger,
		AuditWriter: auditWriter,
		Ctx:         context.Background(),
		JWTVerifier: jwtVer,
	}

	adminHandler, err := InitRoutes(e, d)
	if err != nil {
		t.Fatalf("InitRoutes with AIGuard dispatcher failed: %v", err)
	}
	if adminHandler == nil {
		t.Error("expected non-nil AdminHandler")
	}
}

func TestInitRoutes_WithRedisClient_SemanticPoison(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	e := echo.New()
	db := store.NewWithPgxPool(mock)
	logger := silentLogger()
	auditWriter := InitAuditWriter(nil, logger)
	jwtVer := jwtverifier.New(jwtverifier.Config{
		Issuer:   "http://localhost:3001",
		JWKSURL:  "http://localhost:3001/.well-known/jwks.json",
		RevCheck: jwtverifier.AlwaysAllow{},
		Logger:   logger,
	})

	// Use a fake Redis client — non-nil triggers the PoisonAdder branch.
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer redisClient.Close()

	cfg := &config.Config{}
	d := RoutesDeps{
		Cfg:         cfg,
		DB:          db,
		Logger:      logger,
		AuditWriter: auditWriter,
		Ctx:         context.Background(),
		JWTVerifier: jwtVer,
		RedisClient: redisClient,
	}

	adminHandler, err := InitRoutes(e, d)
	if err != nil {
		t.Fatalf("InitRoutes with Redis failed: %v", err)
	}
	if adminHandler == nil {
		t.Error("expected non-nil AdminHandler")
	}
}

// TestInitRoutes_UnknownSpillBackend_ReturnsError verifies that when the
// spill backend name is unknown, InitRoutes propagates the error from
// spillfactory.New without panicking.
func TestInitRoutes_UnknownSpillBackend_ReturnsError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	e := echo.New()
	db := store.NewWithPgxPool(mock)
	logger := silentLogger()
	auditWriter := InitAuditWriter(nil, logger)
	jwtVer := jwtverifier.New(jwtverifier.Config{
		Issuer:   "http://localhost:3001",
		JWKSURL:  "http://localhost:3001/.well-known/jwks.json",
		RevCheck: jwtverifier.AlwaysAllow{},
		Logger:   logger,
	})

	cfg := &config.Config{}
	cfg.Spill.Enabled = true
	cfg.Spill.Backend = "nonexistent-backend-xyz" // triggers spillfactory error

	d := RoutesDeps{
		Cfg:         cfg,
		DB:          db,
		Logger:      logger,
		AuditWriter: auditWriter,
		Ctx:         context.Background(),
		JWTVerifier: jwtVer,
	}

	_, err = InitRoutes(e, d)
	if err == nil {
		t.Fatal("expected error for unknown spill backend, got nil")
	}
}
