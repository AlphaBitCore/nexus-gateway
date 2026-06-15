package kms

import (
	"context"
	"encoding/base64"
	"testing"
)

// TestCustody_Noop_PassesRawEnv pins the dev/appliance default: provider noop
// returns the raw env value unchanged (byte-identical to reading os.Getenv), so
// wiring a secret through Custody with no KMS configured does not change behavior.
func TestCustody_Noop_PassesRawEnv(t *testing.T) {
	for _, prov := range []string{"", "noop", "NOOP", " noop "} {
		c, err := NewCustody(CustodyConfig{Provider: prov})
		if err != nil {
			t.Fatalf("NewCustody(%q): %v", prov, err)
		}
		if !c.IsNoop() {
			t.Fatalf("provider %q: IsNoop=false, want true", prov)
		}
		t.Setenv("ROOT_SECRET_X", "deadbeefcafe-raw-hex")
		got, err := c.Unwrap(context.Background(), "ROOT_SECRET_X")
		if err != nil {
			t.Fatalf("Unwrap: %v", err)
		}
		if got != "deadbeefcafe-raw-hex" {
			t.Fatalf("noop Unwrap = %q, want the raw env value", got)
		}
	}
}

// TestCustody_Noop_EmptyEnv: an absent secret returns "" with no error (the
// caller's own required-check decides mandatoriness).
func TestCustody_Noop_EmptyEnv(t *testing.T) {
	c, _ := NewCustody(CustodyConfig{Provider: "noop"})
	t.Setenv("ROOT_SECRET_ABSENT", "")
	got, err := c.Unwrap(context.Background(), "ROOT_SECRET_ABSENT")
	if err != nil || got != "" {
		t.Fatalf("Unwrap empty = (%q,%v), want (\"\",nil)", got, err)
	}
}

// TestCustody_Command_UnwrapsBlob: provider command base64-decodes the env value
// (a wrapped blob) and Decrypts it. `cat {file}` is an identity decrypt, so the
// plaintext round-trips — proving the base64-decode + Decrypt plumbing.
func TestCustody_Command_UnwrapsBlob(t *testing.T) {
	c, err := NewCustody(CustodyConfig{Provider: "command", Command: []string{"cat", "{file}"}, TimeoutSec: 5})
	if err != nil {
		t.Fatalf("NewCustody: %v", err)
	}
	if c.IsNoop() {
		t.Fatal("command provider must not report IsNoop")
	}
	t.Setenv("ROOT_SECRET_WRAPPED", base64.StdEncoding.EncodeToString([]byte("the-plaintext-secret")))
	got, err := c.Unwrap(context.Background(), "ROOT_SECRET_WRAPPED")
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if got != "the-plaintext-secret" {
		t.Fatalf("command Unwrap = %q, want the decrypted plaintext", got)
	}
}

// TestCustody_Command_TrimsTrailingNewline: a decrypt command that appends a
// trailing newline — the near-universal CLI behavior (e.g. `aws kms decrypt …
// --output text | base64 -d` run through a shell) — must still yield the EXACT
// plaintext. The loader strips the trailing newline so the unwrapped value
// matches the noop/plaintext form byte-for-byte and the cross-service [MUST MATCH]
// holds; without the trim, the raw-consumed ADMIN_KEY_HMAC_SECRET would silently
// diverge and break admin/VK auth fleet-wide with no boot error.
func TestCustody_Command_TrimsTrailingNewline(t *testing.T) {
	// `cat {file}` emits the decoded blob; `; echo` appends a lone "\n".
	c, err := NewCustody(CustodyConfig{Provider: "command", Command: []string{"sh", "-c", "cat {file}; echo"}, TimeoutSec: 5})
	if err != nil {
		t.Fatalf("NewCustody: %v", err)
	}
	t.Setenv("ROOT_SECRET_NL", base64.StdEncoding.EncodeToString([]byte("exact-plaintext-no-newline")))
	got, err := c.Unwrap(context.Background(), "ROOT_SECRET_NL")
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if got != "exact-plaintext-no-newline" {
		t.Fatalf("command Unwrap = %q, want the plaintext with the trailing newline stripped", got)
	}
}

// TestCustody_Command_EmptyEnv: optional secret absent under command → "" no error.
func TestCustody_Command_EmptyEnv(t *testing.T) {
	c, _ := NewCustody(CustodyConfig{Provider: "command", Command: []string{"cat", "{file}"}})
	t.Setenv("ROOT_SECRET_OPT", "")
	got, err := c.Unwrap(context.Background(), "ROOT_SECRET_OPT")
	if err != nil || got != "" {
		t.Fatalf("Unwrap empty (command) = (%q,%v), want (\"\",nil)", got, err)
	}
}

// TestCustody_Command_InvalidBase64_FailsClosed: a non-empty value that is not a
// valid wrapped blob aborts boot rather than treating ciphertext as plaintext.
func TestCustody_Command_InvalidBase64_FailsClosed(t *testing.T) {
	c, _ := NewCustody(CustodyConfig{Provider: "command", Command: []string{"cat", "{file}"}})
	t.Setenv("ROOT_SECRET_BAD", "not!valid!base64!")
	if _, err := c.Unwrap(context.Background(), "ROOT_SECRET_BAD"); err == nil {
		t.Fatal("expected fail-closed error for invalid base64 under provider=command")
	}
}

// TestCustody_Command_DecryptFail_FailsClosed: a decrypt command that exits
// non-zero aborts boot.
func TestCustody_Command_DecryptFail_FailsClosed(t *testing.T) {
	c, err := NewCustody(CustodyConfig{Provider: "command", Command: []string{"false"}})
	if err != nil {
		t.Fatalf("NewCustody: %v", err)
	}
	t.Setenv("ROOT_SECRET_FAILS", base64.StdEncoding.EncodeToString([]byte("blob")))
	if _, err := c.Unwrap(context.Background(), "ROOT_SECRET_FAILS"); err == nil {
		t.Fatal("expected fail-closed error when the decrypt command fails")
	}
}

// TestCustody_UnknownProvider_FailsClosed: a typo'd provider must abort boot, not
// silently fall back to raw-env (which would defeat custody).
func TestCustody_UnknownProvider_FailsClosed(t *testing.T) {
	if _, err := NewCustody(CustodyConfig{Provider: "ksm-typo"}); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// TestCustody_Command_RequiresArgv: provider=command with no Command is rejected
// at construction (delegates to NewCommandProvider).
func TestCustody_Command_RequiresArgv(t *testing.T) {
	if _, err := NewCustody(CustodyConfig{Provider: "command"}); err == nil {
		t.Fatal("expected error for command provider with empty argv")
	}
}
