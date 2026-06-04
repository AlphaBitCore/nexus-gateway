package assistant

import (
	"context"
	"encoding/json"
	"time"
)

// dbconfirm.go is the NFR-10 durable mirror of the in-memory confirm registry. The
// in-memory channel is the live rendezvous for a parked dangerous-write; this row lets
// a POST /confirm that misses the in-memory entry distinguish "the pod restarted —
// re-issue" from "expired / unknown". It does NOT resume the write after a restart (the
// turn goroutine is in-memory and gone) — see the migration comment. Isolated per user
// (I3): every query carries WHERE "userId" with the authenticated principal's id.

// pendingConfirmFreshWindow bounds how recent an orphaned row may be to still read as
// "restart — re-issue". A confirm that has been parked longer than confirmTimeout would
// have timed out anyway, so an older orphan reads as plain "expired". Equal to
// confirmTimeout so the DB verdict matches the in-memory timeout semantics.
const pendingConfirmFreshWindow = confirmTimeout

type pendingConfirmStore struct {
	pool   pgxPool
	userID string
}

// newPendingConfirmStore builds the store, or returns nil when persistence is
// unavailable (no pool / no userId) so callers degrade to the in-memory-only behavior.
func newPendingConfirmStore(pool pgxPool, userID string) *pendingConfirmStore {
	if pool == nil || userID == "" {
		return nil
	}
	return &pendingConfirmStore{pool: pool, userID: userID}
}

// put records a parked confirm. Best-effort and non-fatal: the in-memory channel is the
// source of truth for the live turn, so a DB hiccup must never block a confirm. It also
// opportunistically reaps this user's orphans older than the freshness window — so
// growth is bounded PER RETURNING USER (a restart leaks at most the in-flight confirms,
// reaped on that user's next confirm). A user who restart-orphans a row and never issues
// another confirm leaves it until the user is deleted (FK cascade); negligible in
// practice (small rows, restart-mid-confirm only) — add a periodic sweep if many
// one-shot principals are ever expected.
func (s *pendingConfirmStore) put(ctx context.Context, key, sessionID, callID, tool string, input json.RawMessage, reason string, requiresSecond, isProd bool) {
	if s == nil {
		return
	}
	in := input
	if len(in) == 0 {
		in = json.RawMessage("null")
	}
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO "AssistantPendingConfirm"
		   ("key","userId","sessionId","callId","tool","input","reason","requiresSecond","isProd")
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT ("key") DO NOTHING`,
		key, s.userID, sessionID, callID, tool, []byte(in), reason, requiresSecond, isProd)
	_, _ = s.pool.Exec(ctx,
		`DELETE FROM "AssistantPendingConfirm" WHERE "userId" = $1 AND "createdAt" < $2`,
		s.userID, time.Now().Add(-pendingConfirmFreshWindow))
}

// del removes the row when a confirm resolves (the makeConfirm defer). A normal flow
// leaves no row behind; only a process restart orphans one.
func (s *pendingConfirmStore) del(ctx context.Context, key string) {
	if s == nil {
		return
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM "AssistantPendingConfirm" WHERE "key" = $1 AND "userId" = $2`, key, s.userID)
}

// fresh reports whether a still-recent orphaned confirm exists for key — i.e. an
// in-memory miss is a restart ("re-issue") rather than a genuine expiry. A row older
// than the freshness window reads as not-fresh (expired). Fails closed (false) on a DB
// error so a degraded read never fabricates a re-issue.
func (s *pendingConfirmStore) fresh(ctx context.Context, key string) bool {
	if s == nil {
		return false
	}
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM "AssistantPendingConfirm"
		   WHERE "key" = $1 AND "userId" = $2 AND "createdAt" > $3)`,
		key, s.userID, time.Now().Add(-pendingConfirmFreshWindow)).Scan(&exists)
	return err == nil && exists
}
