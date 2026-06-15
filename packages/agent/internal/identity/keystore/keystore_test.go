package keystore

import (
	"bytes"
	"testing"
)

func TestMemoryStore_SetGetDelete(t *testing.T) {
	s := NewMemoryStore()

	// Missing key returns (nil, nil) — the not-found contract the
	// attestation signer relies on to detect "not enrolled yet".
	if got, err := s.Get("absent"); err != nil || got != nil {
		t.Fatalf("Get(absent) = (%v, %v); want (nil, nil)", got, err)
	}

	val := []byte("secret-bytes")
	if err := s.Set("k", val); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, val) {
		t.Errorf("Get = %q; want %q", got, val)
	}

	// Get must return a copy: mutating the returned slice must not corrupt
	// the stored value, and mutating the original input must not either.
	got[0] = 'X'
	val[1] = 'Y'
	again, _ := s.Get("k")
	if !bytes.Equal(again, []byte("secret-bytes")) {
		t.Errorf("store corrupted by external mutation: %q", again)
	}

	if err := s.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, _ := s.Get("k"); got != nil {
		t.Errorf("Get after Delete = %q; want nil", got)
	}
	// Deleting an absent key is a no-op, not an error.
	if err := s.Delete("absent"); err != nil {
		t.Errorf("Delete(absent) = %v; want nil", err)
	}
}

func TestGetOrCreateDBKey_GeneratesThenReturnsStable(t *testing.T) {
	s := NewMemoryStore()

	// First call generates a fresh 32-byte key and persists it.
	k1, err := GetOrCreateDBKey(s)
	if err != nil {
		t.Fatalf("first GetOrCreateDBKey: %v", err)
	}
	if len(k1) != dbKeyLen {
		t.Fatalf("key length = %d; want %d", len(k1), dbKeyLen)
	}
	stored, _ := s.Get(dbKeyName)
	if !bytes.Equal(stored, k1) {
		t.Errorf("returned key not persisted under %q", dbKeyName)
	}

	// Second call returns the SAME key (idempotent) — a fresh key each call
	// would silently make the existing SQLCipher DB unreadable.
	k2, err := GetOrCreateDBKey(s)
	if err != nil {
		t.Fatalf("second GetOrCreateDBKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Errorf("GetOrCreateDBKey not stable: %x != %x", k1, k2)
	}
}
