package token_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// sha256Bytes returns the SHA-256 of b. Mirrors the default RefreshHelper
// hash so tests can look up rows the helper wrote.
func sha256Bytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// seedRefreshFixtures mirrors the helper in store/refresh_store_test.go but is
// duplicated here to keep the token package's integration tests self-contained.
// The helper creates a throwaway user + client so the refresh rows satisfy the
// schema's foreign keys without colliding with real data.
func seedRefreshFixtures(t *testing.T, ctx context.Context) (userID, clientID string) {
	t.Helper()
	pool := storetest.Open(t)
	userID = "tok-refresh-user-" + time.Now().Format("150405.000000000")
	clientID = "tok-refresh-client-" + time.Now().Format("150405.000000000")

	_, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,NOW())`,
		userID, "Refresh User", userID+"@test.local")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, userID) })

	_, err = pool.Exec(ctx,
		`INSERT INTO "OAuthClient"(id,name,type,"redirectUris","allowedScopes","updatedAt")
		 VALUES ($1,$2,'public',$3,$4,NOW())`,
		clientID, "Refresh Client",
		[]string{"http://127.0.0.1:*/callback"}, []string{"traffic:write"},
	)
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "OAuthClient" WHERE id=$1`, clientID) })

	// Blanket cleanup: any refresh rows the test inserts referencing these
	// fixtures get deleted on teardown. This keeps individual tests from
	// having to track jtis manually.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "userId"=$1`, userID)
	})

	return userID, clientID
}

func TestRefreshHelper_NewChainPersistsRow(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	h := token.NewRefreshHelper(store.NewRefreshStore(pool))
	raw, sid, jti, err := h.NewChain(ctx, userID, clientID, "dev-1", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if raw == "" || sid == "" || jti == "" {
		t.Fatalf("empty return: raw=%q sid=%q jti=%q", raw, sid, jti)
	}

	// The helper hashes with SHA-256 by default; confirm we can find the row
	// by applying the same hash to the raw token we got back.
	hashed := sha256Bytes([]byte(raw))
	row, found, err := store.NewRefreshStore(pool).FindByTokenHash(ctx, hashed)
	if err != nil || !found {
		t.Fatalf("FindByTokenHash: found=%v err=%v", found, err)
	}
	if row.JTI != jti || row.SessionID != sid {
		t.Fatalf("row mismatch: jti=%q sid=%q row=%+v", jti, sid, row)
	}
	if row.ParentJTI != "" {
		t.Errorf("root row ParentJTI = %q, want empty", row.ParentJTI)
	}
	if row.DeviceID == nil || *row.DeviceID != "dev-1" {
		t.Errorf("DeviceID = %v, want dev-1", row.DeviceID)
	}
}

func TestRefreshHelper_RotateHappyPath(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	rs := store.NewRefreshStore(pool)
	h := token.NewRefreshHelper(rs)

	raw1, sid, jti1, err := h.NewChain(ctx, userID, clientID, "dev-2", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	raw2, jti2, parent, err := h.Rotate(ctx, raw1, time.Hour)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if raw2 == "" || jti2 == "" {
		t.Fatalf("Rotate return empty: raw=%q jti=%q", raw2, jti2)
	}
	if raw2 == raw1 {
		t.Fatalf("rotated token must differ from parent")
	}
	if parent.JTI != jti1 {
		t.Errorf("parent.JTI = %q, want %q", parent.JTI, jti1)
	}
	if parent.SessionID != sid {
		t.Errorf("parent.SessionID = %q, want %q", parent.SessionID, sid)
	}

	// Old row must be flagged used.
	oldRow, _, err := rs.FindByTokenHash(ctx, sha256Bytes([]byte(raw1)))
	if err != nil {
		t.Fatalf("find old: %v", err)
	}
	if oldRow.UsedAt == nil {
		t.Fatalf("old row usedAt should be non-nil after Rotate")
	}

	// New row must exist, reference the parent, and share the session id.
	newRow, _, err := rs.FindByTokenHash(ctx, sha256Bytes([]byte(raw2)))
	if err != nil {
		t.Fatalf("find new: %v", err)
	}
	if newRow.ParentJTI != jti1 {
		t.Errorf("ParentJTI = %q, want %q", newRow.ParentJTI, jti1)
	}
	if newRow.SessionID != sid {
		t.Errorf("new SessionID = %q, want %q", newRow.SessionID, sid)
	}
	if newRow.UsedAt != nil {
		t.Errorf("new row should not be used yet")
	}
}

func TestRefreshHelper_RotateReplayReturnsErrReplay(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	h := token.NewRefreshHelper(store.NewRefreshStore(pool))
	raw, _, _, err := h.NewChain(ctx, userID, clientID, "", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	if _, _, _, err := h.Rotate(ctx, raw, time.Hour); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	// Replaying the same raw token must return ErrReplay because usedAt is
	// now non-nil.
	if _, _, _, err := h.Rotate(ctx, raw, time.Hour); !errors.Is(err, token.ErrReplay) {
		t.Fatalf("second Rotate: err=%v, want ErrReplay", err)
	}
}

func TestRefreshHelper_RotateUnknownTokenReturnsErrReplay(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	_, _ = seedRefreshFixtures(t, ctx)

	h := token.NewRefreshHelper(store.NewRefreshStore(pool))
	// A random string that was never inserted must be classified as replay
	// (not found is indistinguishable from "used + pruned" to the attacker).
	if _, _, _, err := h.Rotate(ctx, "never-issued-token", time.Hour); !errors.Is(err, token.ErrReplay) {
		t.Fatalf("unknown Rotate: err=%v, want ErrReplay", err)
	}
}

func TestRefreshHelper_ReplayHookFiresOnReusedToken(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	h := token.NewRefreshHelper(store.NewRefreshStore(pool))

	var hookCalls []*store.RefreshTokenRow
	h.ReplayHook = func(_ context.Context, row *store.RefreshTokenRow) error {
		hookCalls = append(hookCalls, row)
		return nil
	}

	raw, sid, jti, err := h.NewChain(ctx, userID, clientID, "", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if _, _, _, err := h.Rotate(ctx, raw, time.Hour); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	// Replaying the original raw must fire the hook exactly once because the
	// parent row's usedAt is now non-nil.
	if _, _, _, err := h.Rotate(ctx, raw, time.Hour); !errors.Is(err, token.ErrReplay) {
		t.Fatalf("second Rotate: err=%v, want ErrReplay", err)
	}
	if len(hookCalls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(hookCalls))
	}
	got := hookCalls[0]
	if got.JTI != jti {
		t.Errorf("hook row.JTI = %q, want %q (the original parent)", got.JTI, jti)
	}
	if got.SessionID != sid {
		t.Errorf("hook row.SessionID = %q, want %q", got.SessionID, sid)
	}
}

func TestRefreshHelper_ReplayHookNotFiredOnUnknownToken(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	_, _ = seedRefreshFixtures(t, ctx)

	h := token.NewRefreshHelper(store.NewRefreshStore(pool))
	var called int
	h.ReplayHook = func(_ context.Context, _ *store.RefreshTokenRow) error {
		called++
		return nil
	}

	// Unknown token hash has no parent row to scope the hook to; Rotate must
	// surface ErrReplay without invoking the hook.
	if _, _, _, err := h.Rotate(ctx, "never-issued-token", time.Hour); !errors.Is(err, token.ErrReplay) {
		t.Fatalf("unknown Rotate: err=%v, want ErrReplay", err)
	}
	if called != 0 {
		t.Fatalf("hook called %d times on unknown token, want 0", called)
	}
}

func TestRefreshHelper_ReplayHookNilTolerated(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	h := token.NewRefreshHelper(store.NewRefreshStore(pool))
	// ReplayHook left nil on purpose: replay must still surface ErrReplay and
	// must not panic.
	raw, _, _, err := h.NewChain(ctx, userID, clientID, "", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if _, _, _, err := h.Rotate(ctx, raw, time.Hour); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	if _, _, _, err := h.Rotate(ctx, raw, time.Hour); !errors.Is(err, token.ErrReplay) {
		t.Fatalf("replay Rotate (nil hook): err=%v, want ErrReplay", err)
	}
}

func TestRefreshHelper_RotateExpiredReturnsErrExpired(t *testing.T) {
	pool := storetest.Open(t)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	rs := store.NewRefreshStore(pool)
	h := token.NewRefreshHelper(rs)

	raw, _, jti, err := h.NewChain(ctx, userID, clientID, "", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	// Backdate the row's expiresAt so Rotate's clock check fires.
	if _, err := pool.Exec(ctx,
		`UPDATE "RefreshToken" SET "expiresAt" = NOW() - INTERVAL '1 hour' WHERE jti = $1`, jti,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, _, _, err := h.Rotate(ctx, raw, time.Hour); !errors.Is(err, token.ErrExpired) {
		t.Fatalf("expired Rotate: err=%v, want ErrExpired", err)
	}

	// Row must still be unused: we reject pre-MarkUsed so the chain isn't
	// silently extended off an expired parent.
	row, _, _ := rs.FindByTokenHash(ctx, sha256Bytes([]byte(raw)))
	if row.UsedAt != nil {
		t.Errorf("expired row should not be marked used, got usedAt=%v", row.UsedAt)
	}
}
