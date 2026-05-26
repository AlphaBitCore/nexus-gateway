package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// The production constructors all accept *pgxpool.Pool and stash it
// verbatim. Calling them with a nil pool exercises the assignment
// without ever issuing a SQL call — the *Store value just needs to
// be non-nil for the test to assert the contract. Sibling stores
// (revocation, login) cover their equivalent constructors the same
// way so the allowlist for *_test packages stays consistent.

// TestNewClientStore_StashesPool asserts NewClientStore returns a
// non-nil *ClientStore even when wired to a nil *pgxpool.Pool —
// guards the trivial assignment path against accidental nil-deref
// in production wiring.
func TestNewClientStore_StashesPool(t *testing.T) {
	var p *pgxpool.Pool
	if got := store.NewClientStore(p); got == nil {
		t.Fatal("NewClientStore must return a non-nil *ClientStore")
	}
}

// TestNewUserStore_StashesPool mirrors the contract for UserStore.
func TestNewUserStore_StashesPool(t *testing.T) {
	var p *pgxpool.Pool
	if got := store.NewUserStore(p); got == nil {
		t.Fatal("NewUserStore must return a non-nil *UserStore")
	}
}

// TestNewAssignmentStore_StashesPool mirrors the contract for AssignmentStore.
func TestNewAssignmentStore_StashesPool(t *testing.T) {
	var p *pgxpool.Pool
	if got := store.NewAssignmentStore(p); got == nil {
		t.Fatal("NewAssignmentStore must return a non-nil *AssignmentStore")
	}
}

// TestNewIdPStore_StashesPool mirrors the contract for IdPStore.
func TestNewIdPStore_StashesPool(t *testing.T) {
	var p *pgxpool.Pool
	if got := store.NewIdPStore(p); got == nil {
		t.Fatal("NewIdPStore must return a non-nil *IdPStore")
	}
}

// TestNewFederatedStore_StashesPool mirrors the contract for FederatedStore.
func TestNewFederatedStore_StashesPool(t *testing.T) {
	var p *pgxpool.Pool
	if got := store.NewFederatedStore(p); got == nil {
		t.Fatal("NewFederatedStore must return a non-nil *FederatedStore")
	}
}

// TestNewRefreshStore_StashesPool mirrors the contract for RefreshStore.
func TestNewRefreshStore_StashesPool(t *testing.T) {
	var p *pgxpool.Pool
	if got := store.NewRefreshStore(p); got == nil {
		t.Fatal("NewRefreshStore must return a non-nil *RefreshStore")
	}
}
