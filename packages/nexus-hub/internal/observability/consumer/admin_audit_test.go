package consumer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// adminAuditTestPool returns a pgx pool for admin-audit consumer tests.
// Same pattern as testPool() in traffic_test.go.
//
// The chain-population test below SCOPES its assertions to entityIds it
// inserted itself (prefix `prov-chain-test-...`). It does not TRUNCATE the
// shared AdminAuditLog table — the chain helper itself is exercised under
// contention in audit/chain_test.go (which keeps a separate destructive
// gate). Cleanup uses entityId LIKE prefix so only this test's rows are
// removed.
func adminAuditTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skip: DB unavailable (%v)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skip: DB ping failed (%v)", err)
	}
	return pool
}

// TestAdminAuditWriter_InsertPopulatesChain verifies that the MQ-consumer
// path writes integrityHash for every row and links each row to its
// predecessor via previousHash. F3 acceptance: the consumer must call
// audit.NextHash, not stamp NULLs.
//
// Isolation: the test inserts exactly two rows with a unique entityId
// (`prov-chain-test-<uuid>`) and queries only those rows by entityId. It
// does NOT TRUNCATE the table or reason about absolute sequenceNumbers —
// only the relative chain link between its own two rows.
func TestAdminAuditWriter_InsertPopulatesChain(t *testing.T) {
	pool := adminAuditTestPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Unique entityId so reruns + parallel sessions can't collide on the
	// chain assertion. Two rows are inserted under this entity; they MUST
	// be adjacent in the chain because they're written in the same tx
	// inside insertAdminEvents, with the advisory lock held across both
	// inserts. (Other sessions can't interleave — pg_advisory_xact_lock
	// serialises chain head reads.)
	entityID := "prov-chain-test-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM "AdminAuditLog" WHERE "entityId" = $1`, entityID)
	})

	w := NewAdminAuditWriter(pool, nil, AdminAuditWriterConfig{BatchSize: 10, FlushInterval: time.Hour}, discardLogger(), nil)

	items := []pendingAdminMessage{
		{event: mq.AdminAuditMessage{
			ID:         uuid.New().String(),
			Timestamp:  time.Now().UTC().Add(-2 * time.Second),
			ActorID:    "actor-1",
			ActorLabel: "actor-1",
			Action:     "create",
			EntityType: "provider",
			EntityID:   entityID,
			AfterState: map[string]any{"name": "OpenAI"},
		}},
		{event: mq.AdminAuditMessage{
			ID:         uuid.New().String(),
			Timestamp:  time.Now().UTC().Add(-1 * time.Second),
			ActorID:    "actor-2",
			ActorLabel: "actor-2",
			Action:     "update",
			EntityType: "provider",
			EntityID:   entityID,
			AfterState: map[string]any{"name": "OpenAI v2"},
		}},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := w.insertAdminEvents(ctx, tx, items); err != nil {
		t.Fatalf("insertAdminEvents: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := pool.Query(ctx, `
        SELECT "sequenceNumber", "previousHash", "integrityHash"
        FROM "AdminAuditLog"
        WHERE "entityId" = $1
        ORDER BY "sequenceNumber" ASC
    `, entityID)
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	defer rows.Close()

	type row struct {
		seq   int64
		prev  *string
		integ string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.prev, &r.integ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}

	// Adjacent inserts within the same advisory-locked tx: row 2's
	// previousHash must equal row 1's integrityHash even when the wider
	// AdminAuditLog table already has unrelated rows from other actors.
	if len(got[0].integ) != 64 {
		t.Errorf("row 1 integrityHash len = %d, want 64", len(got[0].integ))
	}
	if got[1].prev == nil {
		t.Fatalf("row 2 previousHash is NULL, want row 1's integrity")
	}
	if *got[1].prev != got[0].integ {
		t.Errorf("row 2 previousHash = %s, want %s (row 1 integrity)", *got[1].prev, got[0].integ)
	}
	if got[1].integ == got[0].integ {
		t.Error("row 2 integrityHash must differ from row 1 integrity")
	}
	if got[1].seq != got[0].seq+1 {
		t.Errorf("rows not adjacent in chain: seq[0]=%d seq[1]=%d (advisory lock should have serialised both inserts back-to-back)", got[0].seq, got[1].seq)
	}
}
