package store_test

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func TestAuthCodeStore_PutGetDelete(t *testing.T) {
	s := store.NewAuthCodeStore(time.Second * 5)
	defer s.Close()

	s.Put("code1", store.AuthCodeEntry{
		ClientID:      "agent-desktop",
		UserID:        "usr_1",
		RedirectURI:   "http://127.0.0.1:1/callback",
		PKCEChallenge: "c",
		SessionID:     "s1",
		IdPID:         "local",
		ExpiresAt:     time.Now().Add(5 * time.Second),
	})

	e, ok := s.Get("code1")
	if !ok || e.UserID != "usr_1" {
		t.Fatalf("lookup failed: ok=%v entry=%+v", ok, e)
	}
	// Codes are single-use — second Get must fail.
	if _, ok := s.Get("code1"); ok {
		t.Fatal("code should be deleted after first Get")
	}
}

func TestAuthCodeStore_Expiry(t *testing.T) {
	s := store.NewAuthCodeStore(time.Millisecond * 10)
	defer s.Close()

	s.Put("k", store.AuthCodeEntry{ExpiresAt: time.Now().Add(time.Millisecond * 10)})
	time.Sleep(time.Millisecond * 50)
	if _, ok := s.Get("k"); ok {
		t.Fatal("expired entry should not be returned")
	}
}

func TestAuthCodeStore_ExpiryViaGet(t *testing.T) {
	s := store.NewAuthCodeStore(time.Millisecond * 10)
	defer s.Close()

	s.Put("sweep", store.AuthCodeEntry{ExpiresAt: time.Now().Add(time.Millisecond * 5)})
	time.Sleep(time.Millisecond * 20)
	if _, ok := s.Get("sweep"); ok {
		t.Fatal("expired entry leaked through Get")
	}
}

func TestAuthCodeStore_CloseIdempotent(t *testing.T) {
	s := store.NewAuthCodeStore(time.Second)
	s.Close()
	s.Close() // must not panic
}

func TestAuthCodeStore_PutOverwrites(t *testing.T) {
	s := store.NewAuthCodeStore(time.Second * 5)
	defer s.Close()

	s.Put("c", store.AuthCodeEntry{UserID: "u1", ExpiresAt: time.Now().Add(time.Second)})
	s.Put("c", store.AuthCodeEntry{UserID: "u2", ExpiresAt: time.Now().Add(time.Second)})

	e, ok := s.Get("c")
	if !ok || e.UserID != "u2" {
		t.Fatalf("expected overwrite; got ok=%v entry=%+v", ok, e)
	}
}
