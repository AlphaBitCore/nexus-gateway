package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

// seedRefreshFixtures creates a throwaway NexusUser + OAuthClient for
// refresh-token tests. Returns (userID, clientID) and registers cleanup.
func seedRefreshFixtures(t *testing.T, ctx context.Context) (userID, clientID string) {
	t.Helper()
	pool := storetest.Open(t)
	userID = "test-user-refresh-" + time.Now().Format("150405.000000000")
	clientID = "test-client-refresh-" + time.Now().Format("150405.000000000")

	_, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,NOW())`,
		userID, "Refresh User", userID+"@test.local")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, userID) })

	_, err = pool.Exec(ctx,
		`INSERT INTO "OAuthClient"(id,name,type,"redirectUris","allowedScopes","requirePkce","updatedAt")
		 VALUES ($1,$2,'public',$3,$4,TRUE,NOW())`,
		clientID, "Refresh Client",
		[]string{"http://127.0.0.1:*/callback"}, []string{"traffic:write"},
	)
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "OAuthClient" WHERE id=$1`, clientID) })

	return userID, clientID
}

func sha256of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func TestRefreshStore_InsertAndFind(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewRefreshStore(pool)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	row := &store.RefreshTokenRow{
		JTI:       uuid.NewString(),
		SessionID: uuid.NewString(),
		UserID:    userID,
		ClientID:  clientID,
		TokenHash: sha256of("raw-refresh-token-A"),
		ExpiresAt: time.Now().Add(1 * time.Hour).UTC(),
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE jti=$1`, row.JTI) })

	if err := s.Insert(ctx, row); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, found, err := s.FindByTokenHash(ctx, row.TokenHash)
	if err != nil || !found {
		t.Fatalf("FindByTokenHash: found=%v err=%v", found, err)
	}
	if got.JTI != row.JTI || got.UserID != userID || got.ClientID != clientID {
		t.Fatalf("row mismatch: %+v", got)
	}
	if !bytes.Equal(got.TokenHash, row.TokenHash) {
		t.Fatalf("tokenHash mismatch")
	}
	if got.UsedAt != nil {
		t.Fatalf("expected usedAt nil on fresh row; got %v", got.UsedAt)
	}

	// unknown hash
	if _, f2, err := s.FindByTokenHash(ctx, sha256of("nope")); err != nil || f2 {
		t.Fatalf("expected (nil,false,nil); got found=%v err=%v", f2, err)
	}
}

func TestRefreshStore_MarkUsedIdempotent(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewRefreshStore(pool)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	row := &store.RefreshTokenRow{
		JTI:       uuid.NewString(),
		SessionID: uuid.NewString(),
		UserID:    userID,
		ClientID:  clientID,
		TokenHash: sha256of("raw-refresh-token-B"),
		ExpiresAt: time.Now().Add(1 * time.Hour).UTC(),
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE jti=$1`, row.JTI) })
	if err := s.Insert(ctx, row); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	ok, err := s.MarkUsed(ctx, row.JTI)
	if err != nil || !ok {
		t.Fatalf("first MarkUsed: ok=%v err=%v", ok, err)
	}
	ok, err = s.MarkUsed(ctx, row.JTI)
	if err != nil || ok {
		t.Fatalf("second MarkUsed: expected (false,nil), got ok=%v err=%v", ok, err)
	}

	// MarkUsed of an unknown jti returns (false, nil) not an error.
	ok, err = s.MarkUsed(ctx, uuid.NewString())
	if err != nil || ok {
		t.Fatalf("unknown MarkUsed: expected (false,nil), got ok=%v err=%v", ok, err)
	}
}

func TestRefreshStore_DeleteBySessionID(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewRefreshStore(pool)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	// Two rows sharing the same sessionId simulate a rotated chain: parent
	// already used, child fresh. DeleteBySessionID must remove both.
	sid := uuid.NewString()
	parent := &store.RefreshTokenRow{
		JTI:       uuid.NewString(),
		SessionID: sid,
		UserID:    userID,
		ClientID:  clientID,
		TokenHash: sha256of("raw-refresh-session-parent"),
		ExpiresAt: time.Now().Add(1 * time.Hour).UTC(),
	}
	child := &store.RefreshTokenRow{
		JTI:       uuid.NewString(),
		SessionID: sid,
		ParentJTI: parent.JTI,
		UserID:    userID,
		ClientID:  clientID,
		TokenHash: sha256of("raw-refresh-session-child"),
		ExpiresAt: time.Now().Add(1 * time.Hour).UTC(),
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE jti IN ($1, $2)`, parent.JTI, child.JTI)
	})

	if err := s.Insert(ctx, parent); err != nil {
		t.Fatalf("Insert parent: %v", err)
	}
	if err := s.Insert(ctx, child); err != nil {
		t.Fatalf("Insert child: %v", err)
	}

	if err := s.DeleteBySessionID(ctx, sid); err != nil {
		t.Fatalf("DeleteBySessionID: %v", err)
	}

	for _, h := range [][]byte{parent.TokenHash, child.TokenHash} {
		if _, f, err := s.FindByTokenHash(ctx, h); err != nil || f {
			t.Fatalf("row still present after DeleteBySessionID: found=%v err=%v", f, err)
		}
	}

	// A second call on an empty sessionId is a no-op, not an error.
	if err := s.DeleteBySessionID(ctx, sid); err != nil {
		t.Fatalf("second DeleteBySessionID: %v", err)
	}
}

func TestRefreshStore_DeleteExpired(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewRefreshStore(pool)
	ctx := context.Background()
	userID, clientID := seedRefreshFixtures(t, ctx)

	expired := &store.RefreshTokenRow{
		JTI:       uuid.NewString(),
		SessionID: uuid.NewString(),
		UserID:    userID,
		ClientID:  clientID,
		TokenHash: sha256of("raw-refresh-token-expired"),
		ExpiresAt: time.Now().Add(-1 * time.Hour).UTC(),
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE jti=$1`, expired.JTI) })
	if err := s.Insert(ctx, expired); err != nil {
		t.Fatalf("Insert expired: %v", err)
	}

	future := &store.RefreshTokenRow{
		JTI:       uuid.NewString(),
		SessionID: uuid.NewString(),
		UserID:    userID,
		ClientID:  clientID,
		TokenHash: sha256of("raw-refresh-token-future"),
		ExpiresAt: time.Now().Add(1 * time.Hour).UTC(),
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE jti=$1`, future.JTI) })
	if err := s.Insert(ctx, future); err != nil {
		t.Fatalf("Insert future: %v", err)
	}

	if err := s.DeleteExpired(ctx); err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}

	// expired gone
	if _, f, err := s.FindByTokenHash(ctx, expired.TokenHash); err != nil || f {
		t.Fatalf("expired should be deleted; found=%v err=%v", f, err)
	}
	// future still there
	if _, f, err := s.FindByTokenHash(ctx, future.TokenHash); err != nil || !f {
		t.Fatalf("future should survive; found=%v err=%v", f, err)
	}
}
