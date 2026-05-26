package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

func TestFederatedStore_UpsertAndFind(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewFederatedStore(pool)
	ctx := context.Background()

	uid := "test-user-fed-" + time.Now().Format("150405.000000000")
	_, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,NOW())`,
		uid, "Fed User "+uid, uid+"@test.local")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, uid) })

	var idpID string
	err = pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,"updatedAt")
		 VALUES ('local','test-fed-local',TRUE,NOW())
		 RETURNING id`,
	).Scan(&idpID)
	if err != nil {
		t.Fatalf("seed idp: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, idpID) })

	subject := uid + "@test.local"

	// First upsert inserts.
	if err := s.UpsertLocalIdentity(ctx, uid, idpID, subject); err != nil {
		t.Fatalf("UpsertLocalIdentity (insert): %v", err)
	}
	// Second call is idempotent and must not error.
	if err := s.UpsertLocalIdentity(ctx, uid, idpID, subject); err != nil {
		t.Fatalf("UpsertLocalIdentity (upsert): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "UserFederatedIdentity" WHERE "idpId"=$1 AND "externalSubject"=$2`, idpID, subject)
	})

	fi, found, err := s.FindByIdPSubject(ctx, idpID, subject)
	if err != nil {
		t.Fatalf("FindByIdPSubject: %v", err)
	}
	if !found {
		t.Fatal("expected to find federated identity we just upserted")
	}
	if fi.UserID != uid || fi.IdPID != idpID || fi.ExternalSubject != subject {
		t.Fatalf("unexpected federated identity: %+v", fi)
	}

	// Not-found path returns (nil, false, nil).
	if _, f2, err := s.FindByIdPSubject(ctx, idpID, "no-such-subject"); err != nil || f2 {
		t.Fatalf("expected (nil,false,nil); got found=%v err=%v", f2, err)
	}
}

func TestFederatedStore_TouchLastLogin(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewFederatedStore(pool)
	ctx := context.Background()

	uid := "test-user-touch-" + time.Now().Format("150405.000000000")
	_, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,NOW())`,
		uid, "Touch User "+uid, uid+"@test.local")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, uid) })

	var idpID string
	err = pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,"updatedAt")
		 VALUES ('local','test-touch-local',TRUE,NOW())
		 RETURNING id`,
	).Scan(&idpID)
	if err != nil {
		t.Fatalf("seed idp: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, idpID) })

	subject := uid + "@test.local"
	if err := s.UpsertLocalIdentity(ctx, uid, idpID, subject); err != nil {
		t.Fatalf("UpsertLocalIdentity: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "UserFederatedIdentity" WHERE "idpId"=$1 AND "externalSubject"=$2`, idpID, subject)
	})

	fi, found, err := s.FindByIdPSubject(ctx, idpID, subject)
	if err != nil || !found {
		t.Fatalf("pre-touch find: found=%v err=%v", found, err)
	}

	if err := s.TouchLastLogin(ctx, fi.ID); err != nil {
		t.Fatalf("TouchLastLogin: %v", err)
	}

	fi2, found, err := s.FindByIdPSubject(ctx, idpID, subject)
	if err != nil || !found {
		t.Fatalf("post-touch find: found=%v err=%v", found, err)
	}
	if fi2.LastLoginAt == nil {
		t.Fatal("expected LastLoginAt to be set after TouchLastLogin")
	}
}
