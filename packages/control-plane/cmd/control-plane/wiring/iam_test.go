package wiring

import (
	"testing"

	pgxmock "github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

func TestInitIAM_NilDB_ReturnsNil(t *testing.T) {
	engine := InitIAM(nil, nil, silentLogger())
	if engine != nil {
		t.Error("expected nil IAM engine when db is nil")
	}
}

func TestInitIAM_WithDB_ReturnsNonNilEngine(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	db := store.NewWithPgxPool(mock)
	engine := InitIAM(db, nil, silentLogger())
	if engine == nil {
		t.Error("expected non-nil IAM engine when db is non-nil")
	}
}

func TestInitIAM_WithDBAndNilRedis_ReturnsNonNilEngine(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	db := store.NewWithPgxPool(mock)
	// Passing nil Redis is fine — InitIAM skips the Redis option when redisClient is nil.
	engine := InitIAM(db, nil, silentLogger())
	if engine == nil {
		t.Error("expected non-nil IAM engine")
	}
}

func TestInitIAM_WithDBAndFakeRedis_AppliesRedisOption(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	db := store.NewWithPgxPool(mock)
	// Provide a fake Redis client to exercise the non-nil Redis branch.
	// redis.NewClient returns a concrete *redis.Client that satisfies
	// redis.UniversalClient without making any connections at construction time.
	fakeRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer fakeRedis.Close()
	engine := InitIAM(db, fakeRedis, silentLogger())
	if engine == nil {
		t.Error("expected non-nil IAM engine when Redis client is provided")
	}
}
