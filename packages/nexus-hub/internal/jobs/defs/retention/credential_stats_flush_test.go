package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

func newMiniredisRdb(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mini.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mini, rdb
}

func TestCredentialStatsFlush_Identity(t *testing.T) {
	j := NewCredentialStatsFlush(nil, nil, 0, testLogger())
	if j.ID() != credStatsFlushJobID {
		t.Errorf("ID = %q", j.ID())
	}
	if j.Name() != credStatsFlushJobName {
		t.Errorf("Name = %q", j.Name())
	}
	if j.Description() != credStatsFlushJobDesc {
		t.Errorf("Description = %q", j.Description())
	}
	if j.Interval() != 60*time.Second {
		t.Errorf("default interval = %v", j.Interval())
	}
	j2 := NewCredentialStatsFlush(nil, nil, 5*time.Second, testLogger())
	if j2.Interval() != 5*time.Second {
		t.Errorf("custom interval = %v", j2.Interval())
	}
}

func TestCredentialStatsFlush_Run_NilRedis(t *testing.T) {
	j := &CredentialStatsFlushJob{rdb: nil, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialStatsFlush_Run_EmptySet(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	j := &CredentialStatsFlushJob{rdb: rdb, logger: testLogger()}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCredentialStatsFlush_Run_DrainsAndWrites(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	ctx := context.Background()

	const credID = "cred-stats-1"
	key := credstate.StatsKey(credID)
	mini.SAdd(credstate.StatsDirtySet, credID)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mini.HSet(key, "cnt", "7")
	mini.HSet(key, credstate.StatsFieldUsedAt, now)
	mini.HSet(key, credstate.StatsFieldOkAt, now)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "Credential" SET`).
		WithArgs(credID, int64(7), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CredentialStatsFlushJob{pool: mock, rdb: rdb, logger: testLogger()}
	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
	// SREM should have cleared the dirty set.
	members, _ := mini.SMembers(credstate.StatsDirtySet)
	if len(members) != 0 {
		t.Errorf("dirty set not drained: %v", members)
	}
	// cnt should have been reset to 0 by the Lua script.
	cnt := mini.HGet(key, "cnt")
	if cnt != "0" {
		t.Errorf("cnt = %q, want 0", cnt)
	}
}

func TestCredentialStatsFlush_FlushOne_NothingToWrite(t *testing.T) {
	_, rdb := newMiniredisRdb(t)
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// No expectations — flushOne should short-circuit.
	j := &CredentialStatsFlushJob{pool: mock, rdb: rdb, logger: testLogger()}
	if err := j.flushOne(context.Background(), "no-such-cred"); err != nil {
		t.Fatalf("flushOne: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestCredentialStatsFlush_FlushOne_DBError(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	const credID = "cred-stats-err"
	key := credstate.StatsKey(credID)
	mini.HSet(key, "cnt", "3")

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	sentinel := errors.New("db boom")
	mock.ExpectExec(`UPDATE "Credential" SET`).
		WithArgs(credID, int64(3), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(sentinel)

	j := &CredentialStatsFlushJob{pool: mock, rdb: rdb, logger: testLogger()}
	err := j.flushOne(context.Background(), credID)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestCredentialStatsFlush_Run_MalformedTimestampSkippedNoError(t *testing.T) {
	mini, rdb := newMiniredisRdb(t)
	ctx := context.Background()
	const credID = "cred-stats-bad-ts"
	mini.SAdd(credstate.StatsDirtySet, credID)
	mini.HSet(credstate.StatsKey(credID), "cnt", "2")
	mini.HSet(credstate.StatsKey(credID), credstate.StatsFieldUsedAt, "not-a-timestamp")

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	// cnt > 0 so an UPDATE is still expected even though the timestamp parses to nil.
	mock.ExpectExec(`UPDATE "Credential" SET`).
		WithArgs(credID, int64(2), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	j := &CredentialStatsFlushJob{pool: mock, rdb: rdb, logger: testLogger()}
	if err := j.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
