package store_test

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func TestPendingAuthzStore_PutTake(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	s.Put("ctx1", store.PendingAuthzEntry{
		ClientID:      "agent-desktop",
		RedirectURI:   "http://127.0.0.1:54321/callback",
		Scope:         "openid traffic:write",
		State:         "st",
		Nonce:         "n",
		CodeChallenge: "cc",
		DeviceID:      "dev_1",
		ExpiresAt:     time.Now().Add(time.Minute),
	})

	e, ok := s.Take("ctx1")
	if !ok || e.ClientID != "agent-desktop" || e.DeviceID != "dev_1" {
		t.Fatalf("lookup failed: ok=%v entry=%+v", ok, e)
	}
	// Pending entries are single-use — second Take must fail.
	if _, ok := s.Take("ctx1"); ok {
		t.Fatal("pending entry should be deleted after first Take")
	}
}

func TestPendingAuthzStore_Expiry(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	s.Put("k", store.PendingAuthzEntry{ExpiresAt: time.Now().Add(time.Millisecond * 5)})
	time.Sleep(time.Millisecond * 20)
	if _, ok := s.Take("k"); ok {
		t.Fatal("expired entry should not be returned")
	}
}

func TestPendingAuthzStore_PutOverwrites(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	s.Put("c", store.PendingAuthzEntry{ClientID: "c1", ExpiresAt: time.Now().Add(time.Minute)})
	s.Put("c", store.PendingAuthzEntry{ClientID: "c2", ExpiresAt: time.Now().Add(time.Minute)})
	e, ok := s.Take("c")
	if !ok || e.ClientID != "c2" {
		t.Fatalf("expected overwrite; got ok=%v entry=%+v", ok, e)
	}
}

func TestPendingAuthzStore_TakeMissing(t *testing.T) {
	s := store.NewPendingAuthzStore()
	defer s.Close()

	if _, ok := s.Take("nope"); ok {
		t.Fatal("Take on missing authctx should report ok=false")
	}
}

func TestPendingAuthzStore_CloseIdempotent(t *testing.T) {
	s := store.NewPendingAuthzStore()
	s.Close()
	s.Close() // must not panic
}
