package store_test

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

func TestBindingStore_PutGet(t *testing.T) {
	s := store.NewBindingStore()
	defer s.Close()

	s.Put("b1", store.BindingEntry{
		DeviceID:      "dev_1",
		State:         "st",
		CodeChallenge: "cc",
		ExpiresAt:     time.Now().Add(5 * time.Minute),
	})

	e, ok := s.Get("b1")
	if !ok || e.DeviceID != "dev_1" {
		t.Fatalf("lookup failed: ok=%v entry=%+v", ok, e)
	}
	// Bindings are NOT consumed on Get — second Get must still succeed.
	if _, ok := s.Get("b1"); !ok {
		t.Fatal("binding should survive Get")
	}
}

func TestBindingStore_Expiry(t *testing.T) {
	s := store.NewBindingStore()
	defer s.Close()

	s.Put("b2", store.BindingEntry{
		DeviceID:  "dev_2",
		ExpiresAt: time.Now().Add(time.Millisecond * 10),
	})
	time.Sleep(time.Millisecond * 30)
	if _, ok := s.Get("b2"); ok {
		t.Fatal("expired binding should not be returned")
	}
}

func TestBindingStore_Delete(t *testing.T) {
	s := store.NewBindingStore()
	defer s.Close()

	s.Put("b3", store.BindingEntry{DeviceID: "dev_3", ExpiresAt: time.Now().Add(time.Minute)})
	s.Delete("b3")
	if _, ok := s.Get("b3"); ok {
		t.Fatal("binding should be removed after Delete")
	}

	// Delete of missing key is a no-op.
	s.Delete("nope")
}

func TestBindingStore_CloseIdempotent(t *testing.T) {
	s := store.NewBindingStore()
	s.Close()
	s.Close() // must not panic
}
