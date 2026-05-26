package revocation_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store/storetest"
)

func TestStore_InsertListSinceDeleteExpired(t *testing.T) {
	ctx := context.Background()
	s := revocation.NewStore(storetest.Open(t))

	userID := uuid.NewString()
	row := revocation.Row{
		Scope:        revocation.ScopeUser,
		TargetUserID: &userID,
		RevokedAt:    time.Now().UTC(),
		ExpiresAt:    time.Now().Add(time.Hour).UTC(),
		Reason:       revocation.ReasonAdminDisable,
	}
	id, err := s.Insert(ctx, row)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	// ListSince from 0 returns at least our row.
	events, last, err := s.ListSince(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if last < id {
		t.Fatalf("expected last >= %d, got %d", id, last)
	}
	found := false
	for _, e := range events {
		if e.TargetUserID != nil && *e.TargetUserID == userID {
			found = true
		}
	}
	if !found {
		t.Fatalf("inserted row not returned by ListSince")
	}

	// Delete-expired removes rows whose expires_at < now() - 1d. Our row is
	// future, so this must be a no-op for it specifically.
	if _, err := s.DeleteExpired(ctx); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
}
