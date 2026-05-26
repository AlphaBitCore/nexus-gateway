package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

func TestUserStore_GetByEmail(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewUserStore(pool)
	ctx := context.Background()

	id := "test-user-email-" + time.Now().Format("150405.000000000")
	email := id + "@test.local"
	_, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","passwordHash","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,$4,NOW())`,
		id, "Email User", email, "argon2id$...$hash")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, id) })

	got, pwd, disabledAt, err := s.GetByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if got != id || pwd != "argon2id$...$hash" || disabledAt != nil {
		t.Fatalf("unexpected result: id=%q pwd=%q disabledAt=%v", got, pwd, disabledAt)
	}

	if _, _, _, err := s.GetByEmail(ctx, "nobody-"+email); !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestUserStore_GetByID(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewUserStore(pool)
	ctx := context.Background()

	id := "test-user-id-" + time.Now().Format("150405.000000000")
	email := id + "@test.local"
	_, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","passwordHash","breakGlass","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,$4,TRUE,NOW())`,
		id, "By-ID User", email, "argon2id$...$hash")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, id) })

	u, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if u.ID != id || u.PasswordHash != "argon2id$...$hash" || !u.BreakGlass {
		t.Fatalf("unexpected user: %+v", u)
	}
	if u.Email == nil || *u.Email != email {
		t.Fatalf("email not round-tripped: %v", u.Email)
	}

	if _, err := s.GetByID(ctx, "nope-"+id); !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestUserStore_TouchLastLogin(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewUserStore(pool)
	ctx := context.Background()

	id := "test-user-touch-" + time.Now().Format("150405.000000000")
	email := id + "@test.local"
	_, err := pool.Exec(ctx,
		`INSERT INTO "NexusUser"(id,"displayName",email,status,"canAccessControlPlane","updatedAt")
		 VALUES ($1,$2,$3,'active',TRUE,NOW())`,
		id, "Touch User", email)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM "NexusUser" WHERE id=$1`, id) })

	if err := s.TouchLastLogin(ctx, id); err != nil {
		t.Fatalf("TouchLastLogin: %v", err)
	}
	u, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if u.LastLoginAt == nil {
		t.Fatal("expected LastLoginAt to be populated")
	}
}
