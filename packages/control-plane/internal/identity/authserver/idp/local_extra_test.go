package idp

import (
	"errors"
	"strings"
	"testing"
)

// TestMustDummyHash_RandReadPanic drives the first defensive panic by swapping
// randReadFn to a stub that returns an error. The default crypto/rand.Read is
// not reachable on Go ≥1.26 (failures fatal the process) but the guard must
// still behave when its function-pointer seam is forced to fail — this is the
// contract mustDummyHash promises in its comment.
func TestMustDummyHash_RandReadPanic(t *testing.T) {
	orig := randReadFn
	t.Cleanup(func() { randReadFn = orig })

	sentinel := errors.New("rand: forced failure")
	randReadFn = func(_ []byte) (int, error) { return 0, sentinel }

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("mustDummyHash: expected panic, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("mustDummyHash: expected string panic value, got %T: %v", r, r)
		}
		if !strings.Contains(msg, "idp: init dummyHash:") {
			t.Fatalf("mustDummyHash: panic message %q missing prefix", msg)
		}
		if !strings.Contains(msg, sentinel.Error()) {
			t.Fatalf("mustDummyHash: panic message %q missing sentinel %q", msg, sentinel.Error())
		}
	}()
	_ = mustDummyHash()
}

// TestMustDummyHash_HashPasswordPanic drives the second defensive panic by
// swapping hashPasswordFn. auth.HashPassword cannot error with a fixed 32-byte
// input today, but the guard exists in case the implementation changes — the
// seam lets us verify the panic message wraps the underlying error.
func TestMustDummyHash_HashPasswordPanic(t *testing.T) {
	origHash := hashPasswordFn
	t.Cleanup(func() { hashPasswordFn = origHash })

	sentinel := errors.New("hash: forced failure")
	hashPasswordFn = func(_ string) (string, error) { return "", sentinel }

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("mustDummyHash: expected panic, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("mustDummyHash: expected string panic value, got %T: %v", r, r)
		}
		if !strings.Contains(msg, "idp: init dummyHash:") {
			t.Fatalf("mustDummyHash: panic message %q missing prefix", msg)
		}
		if !strings.Contains(msg, sentinel.Error()) {
			t.Fatalf("mustDummyHash: panic message %q missing sentinel %q", msg, sentinel.Error())
		}
	}()
	_ = mustDummyHash()
}

// TestMustDummyHash_HappyPath_ReturnsNonEmpty exercises the success path of
// the seam (defaults intact) to confirm production callers still get a
// non-empty hash. This is the path that actually runs at package init via
// var dummyHash = mustDummyHash().
func TestMustDummyHash_HappyPath_ReturnsNonEmpty(t *testing.T) {
	h := mustDummyHash()
	if h == "" {
		t.Fatalf("mustDummyHash: returned empty hash on happy path")
	}
	// Production format is "salt_hex:hash_hex"; assert the colon separator
	// is present so any future format change forces test review.
	if !strings.Contains(h, ":") {
		t.Fatalf("mustDummyHash: %q missing salt:hash separator", h)
	}
}
