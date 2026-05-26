package kms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// KMSProvider abstracts the "decrypt the wrapped CA private key" step so the
// compliance-proxy can keep its CA key on disk in a wrapped form and unwrap it
// at startup via an external key-management service.
//
// Design notes:
//   - Decryption happens once at process startup, not per cert sign. The
//     unwrapped key is held in memory for the lifetime of the process. A
//     50-200ms KMS round-trip is acceptable in exchange for never storing
//     the raw key on disk.
//   - The interface is intentionally narrow: only Decrypt. Encryption (key
//     wrapping) happens out-of-band on a workstation when the operator first
//     generates the CA — the running proxy never encrypts new keys.
//   - "Remote signing" (where every cert sign goes through KMS, so the key
//     never leaves at all) would require a different interface shape because
//     it touches the hot path. Envelope encryption with one-shot decrypt at
//     startup is the current scope.
type KMSProvider interface {
	// Name returns a short identifier for logs and the runtime API
	// (e.g. "noop", "command", "aws-kms").
	Name() string
	// Decrypt unwraps the given ciphertext and returns the plaintext PEM
	// bytes of the CA private key. The ciphertext is whatever blob the
	// operator stored on disk.
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// NoopProvider is the default KMS provider used when no KMS is configured.
// It returns the input bytes verbatim — i.e. the on-disk file is the raw
// PEM key, exactly as the compliance-proxy expects when no KMS is configured.
type NoopProvider struct{}

func (NoopProvider) Name() string { return "noop" }

func (NoopProvider) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}

// CommandProvider unwraps the CA key by shelling out to a configurable
// command. Designed to be cloud-agnostic so a single binary can support
// AWS KMS / GCP KMS / Azure Key Vault / Vault / sops / age / etc. without
// pulling every cloud SDK into the build.
//
// The command receives the ciphertext file path via a `{file}` placeholder
// in any of the configured args, and is expected to write the plaintext PEM
// bytes to stdout. Stderr is captured into the error message on failure.
//
// Examples (operator picks one):
//
//	# AWS KMS via aws-cli
//	command: ["aws", "kms", "decrypt", "--ciphertext-blob", "fileb://{file}",
//	          "--output", "text", "--query", "Plaintext", "--cli-binary-format",
//	          "raw-in-base64-out"]
//	# (then pipe to base64 -d in a wrapper script)
//
//	# sops
//	command: ["sops", "--decrypt", "{file}"]
//
//	# age
//	command: ["age", "--decrypt", "--identity", "/etc/age.key", "{file}"]
//
//	# HashiCorp Vault
//	command: ["vault", "kv", "get", "-field=ca_key", "secret/compliance-proxy"]
//	# (no {file} placeholder needed; ignores ciphertext file)
type CommandProvider struct {
	args    []string
	timeout time.Duration
	logTag  string
}

// NewCommandProvider returns a provider that runs the given argv on every
// Decrypt call. The first element is the executable; subsequent elements are
// arguments. Any element equal to "{file}" is replaced with the ciphertext
// file path that NewIssuer creates from the in-memory ciphertext bytes.
//
// timeout caps the wall-clock time the command may run; defaults to 10s
// when zero so a hung sub-process cannot wedge proxy startup.
func NewCommandProvider(args []string, timeout time.Duration) (*CommandProvider, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("cert/kms: command provider requires non-empty args")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &CommandProvider{
		args:    args,
		timeout: timeout,
		logTag:  args[0],
	}, nil
}

func (p *CommandProvider) Name() string { return "command:" + p.logTag }

// tempWriteKMSFn wraps the (*os.File).Write call inside Decrypt so tests can
// inject a failing variant to exercise the error-handling arm that fires after
// os.CreateTemp succeeds. On a healthy POSIX filesystem a Write to a just-
// opened temp file never fails; only fault-injection can reach this branch in
// production. Test-only override; production never reassigns this variable.
var tempWriteKMSFn = func(f interface{ Write([]byte) (int, error) }, b []byte) (int, error) {
	return f.Write(b)
}

// tempCloseKMSFn wraps the (*os.File).Close call inside Decrypt so tests can
// inject a failing variant to exercise the "close temp file" error arm.
// On a healthy filesystem, Close on a successfully-written temp file never
// fails; only fault-injection can reach this branch in production.
// Test-only override; production never reassigns this variable.
var tempCloseKMSFn = func(f interface{ Close() error }) error {
	return f.Close()
}

// Decrypt writes the ciphertext to a temp file (so the configured command
// can reference it via {file}), runs the command, and returns its stdout.
// The temp file is removed even if the command fails. Stderr is included
// in error messages so KMS failures are immediately diagnosable.
func (p *CommandProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	tmp, err := os.CreateTemp("", "nexus-ca-wrapped-*.bin")
	if err != nil {
		return nil, fmt.Errorf("cert/kms: create temp file: %w", err)
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()
	if _, err := tempWriteKMSFn(tmp, ciphertext); err != nil {
		_ = tempCloseKMSFn(tmp)
		return nil, fmt.Errorf("cert/kms: write ciphertext to temp file: %w", err)
	}
	if err := tempCloseKMSFn(tmp); err != nil {
		return nil, fmt.Errorf("cert/kms: close temp file: %w", err)
	}

	// Substitute {file} placeholder anywhere in the args. We deliberately
	// allow the operator to omit it (e.g. for Vault commands that pull the
	// key from a different store entirely).
	resolved := make([]string, len(p.args))
	for i, a := range p.args {
		resolved[i] = strings.ReplaceAll(a, "{file}", tmp.Name())
	}

	cmdCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, resolved[0], resolved[1:]...)
	stdout, err := cmd.Output()
	if err != nil {
		// CommandContext returns *exec.ExitError on non-zero exit; expose
		// stderr in the message so the operator can see KMS auth failures
		// without enabling debug logging.
		stderr := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.Stderr != nil {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("cert/kms: command %q failed: %w (stderr: %s)",
			p.args[0], err, stderr)
	}
	if len(stdout) == 0 {
		return nil, fmt.Errorf("cert/kms: command %q produced empty output", p.args[0])
	}
	return stdout, nil
}
