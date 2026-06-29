package kms

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNoopProvider_PassthroughBytes(t *testing.T) {
	p := NoopProvider{}
	if p.Name() != "noop" {
		t.Errorf("expected name noop, got %q", p.Name())
	}
	in := []byte("hello world")
	out, err := p.Decrypt(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("noop should return input verbatim, got %q", out)
	}
}

func TestCommandProvider_RejectsEmptyArgs(t *testing.T) {
	if _, err := NewCommandProvider(nil, 0); err == nil {
		t.Errorf("expected error for nil args")
	}
	if _, err := NewCommandProvider([]string{}, 0); err == nil {
		t.Errorf("expected error for empty args")
	}
}

func TestCommandProvider_DefaultTimeout(t *testing.T) {
	// timeout=0 should default to 10s, not 0 (which would make every
	// command immediately fail).
	p, err := NewCommandProvider([]string{"echo", "ok"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.timeout != 10*time.Second {
		t.Errorf("expected default timeout 10s, got %v", p.timeout)
	}
}

func TestCommandProvider_Echo(t *testing.T) {
	// `cat {file}` reads the ciphertext file and writes it to stdout —
	// effectively a noop, but exercises the temp-file + arg substitution
	// path that real KMS commands rely on.
	p, err := NewCommandProvider([]string{"cat", "{file}"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(p.Name(), "command:") {
		t.Errorf("expected command: prefix, got %q", p.Name())
	}
	in := []byte("decrypted-pem-bytes")
	out, err := p.Decrypt(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("expected round-trip via cat to return input, got %q", out)
	}
}

func TestCommandProvider_NonZeroExitSurfacesStderr(t *testing.T) {
	// `false` always exits non-zero. Verify the error message includes
	// enough context that an operator can diagnose KMS auth failures
	// without enabling debug logging.
	p, err := NewCommandProvider([]string{"false"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = p.Decrypt(context.Background(), []byte("ignored"))
	if err == nil {
		t.Fatalf("expected error from `false` exit non-zero")
	}
	if !strings.Contains(err.Error(), "false") {
		t.Errorf("error should mention the command name, got: %v", err)
	}
}

func TestCommandProvider_EmptyOutputFails(t *testing.T) {
	// `true` exits 0 with no output. We treat empty output as a failure
	// because a successful KMS decrypt MUST produce the PEM bytes — an
	// empty result is almost always a misconfiguration.
	p, err := NewCommandProvider([]string{"true"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = p.Decrypt(context.Background(), []byte("ignored"))
	if err == nil {
		t.Fatalf("expected error for empty output")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty output, got: %v", err)
	}
}

func TestNewCommandEncryptor_RejectsEmptyArgs(t *testing.T) {
	if _, err := NewCommandEncryptor(nil, 0); err == nil {
		t.Error("expected error for nil args")
	}
	if _, err := NewCommandEncryptor([]string{}, 0); err == nil {
		t.Error("expected error for empty args")
	}
}

func TestNewCommandEncryptor_DefaultTimeout(t *testing.T) {
	// timeout=0 must default to 10s (mirrors NewCommandProvider) so a
	// misconfigured field can't wedge the DEK bootstrap with instant cancels.
	e, err := NewCommandEncryptor([]string{"cat"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.timeout != 10*time.Second {
		t.Errorf("expected default timeout 10s, got %v", e.timeout)
	}
	if !strings.HasPrefix(e.Name(), "command:") {
		t.Errorf("expected command: name prefix, got %q", e.Name())
	}
}

// TestCommandEncryptor_WrapRoundTrip: Encrypt reads from stdin (`cat` with no
// file arg), Decrypt reads from a {file} temp path (`cat {file}`). Together
// they form an identity envelope — round-tripping proves the stdin plumbing for
// Encrypt and the temp-file + arg substitution for Decrypt both work.
func TestCommandEncryptor_WrapRoundTrip(t *testing.T) {
	// CommandEncryptor.Encrypt uses runCommandStdin — the command reads plaintext
	// from STDIN; {file} is NOT substituted on the encrypt path (the plaintext
	// is a secret that must never touch disk).
	enc, err := NewCommandEncryptor([]string{"cat"}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewCommandEncryptor: %v", err)
	}
	dec, err := NewCommandProvider([]string{"cat", "{file}"}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewCommandProvider: %v", err)
	}
	dek := []byte("a-32-byte-dek-padded-to-length!!")
	wrapped, err := enc.Encrypt(context.Background(), dek)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := dec.Decrypt(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(dek) {
		t.Errorf("round-trip mismatch: got %q want %q", got, dek)
	}
}

// TestCommandEncryptor_NonZeroExitSurfacesStderr: a failing KMS encrypt must
// surface the command name + stderr so an operator can diagnose a missing
// kms:Encrypt grant without enabling debug logging.
func TestCommandEncryptor_NonZeroExitSurfacesStderr(t *testing.T) {
	e, err := NewCommandEncryptor([]string{"sh", "-c", "echo denied 1>&2; exit 5"}, 5*time.Second)
	if err != nil {
		t.Fatalf("NewCommandEncryptor: %v", err)
	}
	_, err = e.Encrypt(context.Background(), []byte("dek"))
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("err should surface stderr 'denied'; got %q", err)
	}
}

func TestCommandProvider_FilePlaceholderSubstitution(t *testing.T) {
	// `wc -c {file}` returns the byte count followed by the filename.
	// Asserts the {file} placeholder gets replaced AND the temp file is
	// readable by the subprocess (permissions, content, etc.).
	p, err := NewCommandProvider([]string{"wc", "-c", "{file}"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := p.Decrypt(context.Background(), []byte("12345"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// On macOS the format is "       5 /tmp/...", on linux "5 /tmp/...".
	if !strings.Contains(string(out), "5") {
		t.Errorf("expected wc to report 5 bytes, got %q", out)
	}
}
