package kms

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"
)

// CustodyConfig is the per-SERVICE envelope-custody configuration for unwrapping
// a service's fleet root secrets at boot (rotation/custody-substrate design,
// Layer C). It is yaml/argv only and carries NO secret: the secret material is
// the env-delivered wrapped blob plus the process's ambient KMS grant. One
// config per service unwraps that service's whole root-secret set (the
// per-service grant default — the same Decrypt command applies to each secret's
// blob via the {file} placeholder).
//
// This is SERVER-side custody. Desktop agents do not hold the fleet roots and
// cannot take a fleet KMS grant; their at-rest custody is an OS-keystore
// Provider dropped into the same KMSProvider interface, NOT this
// config.
type CustodyConfig struct {
	// Provider selects how a root secret named by an env var is resolved:
	//   "" / "noop" → the env var holds the RAW secret (dev / appliance simplest
	//                 mode; byte-identical to reading os.Getenv directly).
	//   "command"   → the env var holds a base64 wrapped blob, unwrapped once at
	//                 boot via Command (the cloud-agnostic CommandProvider:
	//                 aws kms / sops / age / vault).
	// Unknown values are a fail-closed boot error.
	Provider string `yaml:"provider"`
	// Command is the Decrypt argv for provider=="command" (a `{file}` placeholder
	// is substituted with a temp file holding the wrapped blob). Ignored for noop.
	Command []string `yaml:"command"`
	// TimeoutSec bounds each Decrypt invocation; 0 defaults to 10s.
	TimeoutSec int `yaml:"timeoutSec"`
}

// Custody resolves a service's root secrets per its CustodyConfig. Build once at
// boot via NewCustody, then call Unwrap per secret.
type Custody struct {
	// provider is nil for the noop case (raw env), non-nil for command.
	provider KMSProvider
}

// NewCustody builds a Custody from config. provider=="command" requires a
// non-empty Command (NewCommandProvider enforces this). An unrecognised
// provider is an error so a typo fails the boot rather than silently falling
// back to raw-env (which would defeat custody).
func NewCustody(cfg CustodyConfig) (*Custody, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "noop":
		return &Custody{provider: nil}, nil
	case "command":
		p, err := NewCommandProvider(cfg.Command, time.Duration(cfg.TimeoutSec)*time.Second)
		if err != nil {
			return nil, fmt.Errorf("kms custody: command provider: %w", err)
		}
		return &Custody{provider: p}, nil
	default:
		return nil, fmt.Errorf("kms custody: unknown provider %q (want \"noop\" or \"command\")", cfg.Provider)
	}
}

// IsNoop reports whether this Custody passes raw env through unchanged (no KMS).
func (c *Custody) IsNoop() bool { return c.provider == nil }

// Unwrap resolves the root secret carried by env var `name`:
//   - noop:    returns the raw env value (today's behavior; dev).
//   - command: base64-decodes the env value (a wrapped blob) and Decrypts it.
//
// An EMPTY env value returns "" with no error — the caller's own required-check
// (e.g. config.validate) decides whether the secret is mandatory, so an optional
// secret like CREDENTIAL_KEY_MAP stays optional. A NON-empty value that fails to
// base64-decode or to Decrypt is a fail-closed error (boot aborts) rather than a
// silent fallback to treating the ciphertext as plaintext.
func (c *Custody) Unwrap(ctx context.Context, name string) (string, error) {
	raw := os.Getenv(name)
	if c.provider == nil { // noop: the raw env value IS the plaintext
		return raw, nil
	}
	if raw == "" {
		return "", nil
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("kms custody: env %s is not valid base64 (provider=command expects a wrapped blob): %w", name, err)
	}
	pt, err := c.provider.Decrypt(ctx, blob)
	if err != nil {
		return "", fmt.Errorf("kms custody: unwrap env %s failed: %w", name, err)
	}
	// Strip a trailing newline from the decrypt command's stdout. CLIs almost
	// universally append "\n" (e.g. `aws kms decrypt … --output text | base64 -d`
	// run through a shell), and the root secrets this loader resolves (AES hex
	// keys, the HMAC secret, tokens) are consumed BYTE-FOR-BYTE — the HMAC secret
	// with no format check at all — so a stray trailing newline would silently
	// diverge the unwrapped value from the noop/plaintext form and break the
	// cross-service [MUST MATCH]. The noop branch returns os.Getenv, which can
	// never carry a trailing newline (bootenv / systemd EnvironmentFile parse env
	// line-by-line), so trimming here only tightens the noop==command identity,
	// never loosens it. Scope: this is the env-secret loader ONLY — the
	// compliance-proxy CA-PEM path calls Provider.Decrypt directly (not Unwrap),
	// so PEM trailing newlines are untouched.
	return strings.TrimRight(string(pt), "\r\n"), nil
}
