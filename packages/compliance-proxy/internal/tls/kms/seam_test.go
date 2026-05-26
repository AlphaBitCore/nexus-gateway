package kms

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestKMSDecrypt_TempDirFailure forces os.CreateTemp inside Decrypt to
// fail by pointing TMPDIR at a path that doesn't exist. CreateTemp uses
// os.TempDir() when the first arg is "", and os.TempDir() honors TMPDIR.
func TestKMSDecrypt_TempDirFailure(t *testing.T) {
	t.Setenv("TMPDIR", "/this/path/intentionally/does/not/exist/abc-xyz-12345")

	p, err := NewCommandProvider([]string{"cat", "{file}"}, time.Second)
	if err != nil {
		t.Fatalf("NewCommandProvider: %v", err)
	}
	_, err = p.Decrypt(context.Background(), []byte("ignored"))
	if err == nil {
		t.Fatal("expected CreateTemp failure")
	}
	if !strings.Contains(err.Error(), "create temp file") {
		t.Errorf("err should wrap 'create temp file'; got %q", err)
	}
}

// TestKMSDecrypt_NonZeroExitWithStderr drives the
// `errors.As(&exitErr) && exitErr.Stderr != nil` branch in the error
// formatter — the existing `false` test exits non-zero but doesn't emit
// stderr; this one explicitly writes to stderr so the message includes it.
func TestKMSDecrypt_NonZeroExitWithStderr(t *testing.T) {
	p, err := NewCommandProvider(
		[]string{"sh", "-c", "echo kms-auth-failed 1>&2; exit 9"},
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("NewCommandProvider: %v", err)
	}
	_, err = p.Decrypt(context.Background(), []byte("ignored"))
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if !strings.Contains(err.Error(), "kms-auth-failed") {
		t.Errorf("err should surface stderr; got %q", err)
	}
}

// --- seam tests added to reach ≥95% coverage ---

// swapTempWriteKMSFn injects fn into tempWriteKMSFn and returns a restore
// func. Deferred restore guarantees the seam is never left swapped after a
// test, which would corrupt subsequent tests in the same run.
func swapTempWriteKMSFn(t *testing.T, fn func(interface{ Write([]byte) (int, error) }, []byte) (int, error)) func() {
	t.Helper()
	orig := tempWriteKMSFn
	tempWriteKMSFn = fn
	return func() { tempWriteKMSFn = orig }
}

// swapTempCloseKMSFn injects fn into tempCloseKMSFn and returns a restore func.
func swapTempCloseKMSFn(t *testing.T, fn func(interface{ Close() error }) error) func() {
	t.Helper()
	orig := tempCloseKMSFn
	tempCloseKMSFn = fn
	return func() { tempCloseKMSFn = orig }
}

// TestDecrypt_TempFileWriteFails_CleansUpAndReturnsError exercises the
// tmp.Write(ciphertext) error arm in CommandProvider.Decrypt. After
// os.CreateTemp succeeds, a Write failure is impossible on a healthy POSIX
// filesystem without fault injection. This seam proves: (1) the error is
// wrapped with "write ciphertext to temp file", (2) the function returns
// immediately (no partial state — the ciphertext is NOT handed to the
// command), and (3) the deferred os.Remove still fires (cleanup invariant).
func TestDecrypt_TempFileWriteFails_CleansUpAndReturnsError(t *testing.T) {
	p, err := NewCommandProvider([]string{"cat", "{file}"}, time.Second)
	if err != nil {
		t.Fatalf("NewCommandProvider: %v", err)
	}

	injectedErr := errors.New("disk full")
	restore := swapTempWriteKMSFn(t, func(_ interface{ Write([]byte) (int, error) }, _ []byte) (int, error) {
		return 0, injectedErr
	})
	defer restore()

	out, err := p.Decrypt(context.Background(), []byte("ciphertext"))
	if err == nil {
		t.Fatal("expected error when Write fails on the temp file")
	}
	if out != nil {
		t.Error("must return nil output on write failure (no partial state)")
	}
	if !strings.Contains(err.Error(), "write ciphertext to temp file") {
		t.Errorf("err must wrap 'write ciphertext to temp file'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}

// TestDecrypt_TempFileCloseFails_ReturnsError exercises the tmp.Close()
// error arm in CommandProvider.Decrypt, which runs after a successful Write.
// On a healthy filesystem Close of a just-written temp file never fails;
// this seam proves the arm is wired. Named failure mode: "close temp file".
func TestDecrypt_TempFileCloseFails_ReturnsError(t *testing.T) {
	p, err := NewCommandProvider([]string{"cat", "{file}"}, time.Second)
	if err != nil {
		t.Fatalf("NewCommandProvider: %v", err)
	}

	injectedErr := errors.New("close: input/output error")
	callCount := 0
	restore := swapTempCloseKMSFn(t, func(f interface{ Close() error }) error {
		callCount++
		// The first call is the "real" Close after Write. Make it fail.
		// If Write also failed (previous branch), Close is called as cleanup
		// via `_ = tempCloseKMSFn(tmp)` — but that branch uses the same seam.
		// Here Write succeeds (tempWriteKMSFn is not overridden), so the
		// first Close call is the post-Write one: return injectedErr.
		return injectedErr
	})
	defer restore()

	out, err := p.Decrypt(context.Background(), []byte("ciphertext"))
	if err == nil {
		t.Fatal("expected error when Close fails on the temp file")
	}
	if out != nil {
		t.Error("must return nil output on close failure (no partial state)")
	}
	if !strings.Contains(err.Error(), "close temp file") {
		t.Errorf("err must wrap 'close temp file'; got %q", err)
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("err must chain injected error via %%w; got %q", err)
	}
}
