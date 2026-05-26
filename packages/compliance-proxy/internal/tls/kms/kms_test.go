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
