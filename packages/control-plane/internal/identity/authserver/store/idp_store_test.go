package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

func TestIdPStore_ListEnabled(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewIdPStore(pool)
	ctx := context.Background()

	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,config,"roleMapping","defaultRole","jitEnabled","updatedAt")
		 VALUES ('oidc','test-oidc-list',TRUE,'{"issuer":"https://idp.example"}'::jsonb,'[]'::jsonb,'developer',TRUE,NOW())
		 RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed idp: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, id)
	})

	rows, err := s.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	found := false
	for _, p := range rows {
		if p.ID == id {
			found = true
			if p.Type != "oidc" || !p.Enabled {
				t.Fatalf("bad idp: %+v", p)
			}
			if p.Config["issuer"] != "https://idp.example" {
				t.Fatalf("config json did not round-trip: %v", p.Config)
			}
		}
	}
	if !found {
		t.Fatalf("seeded idp %s not in ListEnabled result", id)
	}
}

func TestIdPStore_GetByID(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewIdPStore(pool)
	ctx := context.Background()

	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,config,"roleMapping","defaultRole","jitEnabled","updatedAt")
		 VALUES ('oidc','test-oidc-byid',TRUE,'{}'::jsonb,'[{"claim":"groups","value":"admins","role":"admin"}]'::jsonb,'viewer',TRUE,NOW())
		 RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed idp: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, id)
	})

	p, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if p.DefaultRole != "viewer" || len(p.RoleMapping) != 1 {
		t.Fatalf("role mapping round-trip failed: %+v", p)
	}
	if p.RoleMapping[0]["role"] != "admin" {
		t.Fatalf("unexpected role mapping payload: %v", p.RoleMapping)
	}

	if _, err := s.GetByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrIdPNotFound) {
		t.Fatalf("expected ErrIdPNotFound, got %v", err)
	}
}

func TestIdPStore_GetLocal(t *testing.T) {
	pool := storetest.Open(t)
	s := store.NewIdPStore(pool)
	ctx := context.Background()

	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO "IdentityProvider"(type,name,enabled,config,"roleMapping","defaultRole","jitEnabled","updatedAt")
		 VALUES ('local','test-local',TRUE,'{}'::jsonb,'[]'::jsonb,'developer',TRUE,NOW())
		 RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed idp: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM "IdentityProvider" WHERE id=$1`, id)
	})

	p, err := s.GetLocal(ctx)
	if err != nil {
		t.Fatalf("GetLocal: %v", err)
	}
	if p.Type != "local" || !p.Enabled {
		t.Fatalf("unexpected local idp row: %+v", p)
	}
}
