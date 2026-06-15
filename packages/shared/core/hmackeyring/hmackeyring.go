// Package hmackeyring parses the versioned HMAC key map that lets the
// admin-API-key + virtual-key HMAC secret rotate WITHOUT a fleet lockstep flip.
// It is the HMAC counterpart of the credential keyring
// (control-plane/internal/platform/crypto.MultiVault): a comma-separated
// "[*]vN:secret" map with exactly one "*"-marked CURRENT version.
//
// Why a keyring at all: an HMAC is ONE-WAY. Unlike an AES-encrypted credential
// (decrypt-old → reseal-new), a stored key_hash cannot be recomputed under a new
// secret without the raw key (which only the holder has). So rotating the HMAC
// secret must be try-all-versions on admission + lazy re-hash on the matching
// auth — never an in-place re-seal. This package owns only the parse + version
// lookup; the per-class HKDF derivation (keyderive.DeriveSubkey) and the HMAC
// itself stay in each service's auth layer, so the Control Plane and AI Gateway
// derive byte-identical VK hashes ([MUST MATCH]) from the same version's secret.
//
// Secret format: unlike the credential keyring (64-hex AES masters) an HMAC
// secret is an arbitrary high-entropy string, so there is NO hex/length check.
// Because the map is comma-and-colon delimited, a secret MUST NOT contain a
// comma; it MAY contain colons (the version:secret split is on the FIRST colon).
// Operator-generated secrets (`openssl rand -hex 32` / -base64) are comma-free by
// construction; this constraint is documented in .env.example.
package hmackeyring

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/keymap"
)

// Entry is one keyring version: its id and raw secret bytes. The caller HKDFs
// Secret under its trust-domain class (keyderive.DeriveSubkey) before HMACing.
type Entry struct {
	Version string
	Secret  []byte
}

// Keyring holds the parsed versions and the designated current version.
type Keyring struct {
	current   string
	secrets   map[string][]byte
	insertion []string // version ids in map order, for a deterministic try sequence
}

// New parses an ADMIN_KEY_HMAC_KEY_MAP value, e.g.
//
//	"v1:<secret>,*v2:<secret>,v3:<secret>"  → current = v2
//
// The "*"-marked entry hashes NEWLY issued keys; all versions are tried on
// admission. If no entry is marked "*", the LAST entry wins (mirrors the
// credential-keyring fallback, so a single-entry or append-only map needs no
// marker). More than one "*", a duplicate version id, an empty version id, an
// empty secret, or an empty map are all errors (fail-closed: a malformed keyring
// must abort boot, never silently admit under a partial set).
func New(keyMap string) (*Keyring, error) {
	// Delegate the "[*]vN:value" wire parse + current-version selection to the
	// shared leaf (keymap.Parse): comma-split, first-colon id:secret split,
	// "*"-strip current marker, single-"*" / dup-id / empty-map fail-closed.
	// The only HMAC-specific value rule is non-empty (unlike the AES vaults
	// there is NO hex/length check — an HMAC secret is an arbitrary
	// high-entropy string), supplied as the validator.
	entries, current, order, _, err := keymap.Parse(keyMap, func(id, secret string) error {
		if secret == "" {
			return fmt.Errorf("empty secret for version %q", id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	kr := &Keyring{
		current:   current,
		secrets:   make(map[string][]byte, len(entries)),
		insertion: order,
	}
	for id, secret := range entries {
		kr.secrets[id] = []byte(secret)
	}
	return kr, nil
}

// Single builds a one-version keyring from a single non-rotating secret — the
// ADMIN_KEY_HMAC_SECRET path (no map). The version defaults to "v1" to match the
// schema's key_version default. The secret is whitespace-trimmed EXACTLY like a
// map entry's secret in New (keymap.Parse trims both sides of the first colon),
// so a key issued here hashes identically when the operator later migrates the
// same value to "*v1:<same-secret>" — a trailing newline from a file-sourced
// env var would otherwise silently 401 every existing key at the migration
// step. An empty (or all-whitespace) secret is rejected (the boot gate
// requires a non-empty HMAC secret).
func Single(secret string) (*Keyring, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("hmackeyring: empty secret")
	}
	return &Keyring{
		current:   "v1",
		secrets:   map[string][]byte{"v1": []byte(secret)},
		insertion: []string{"v1"},
	}, nil
}

// Current returns the version id + secret used to hash NEWLY issued keys.
func (k *Keyring) Current() (version string, secret []byte) {
	return k.current, k.secrets[k.current]
}

// CurrentVersion returns just the current version id (for stamping key_version).
func (k *Keyring) CurrentVersion() string { return k.current }

// All returns every version with the CURRENT version first, then the remaining
// versions in map order. This is the try-all-versions admission sequence: hash
// under Current first (the steady-state common case is a one-hash hit), then
// fall through to older versions on a miss.
func (k *Keyring) All() []Entry {
	out := make([]Entry, 0, len(k.secrets))
	out = append(out, Entry{Version: k.current, Secret: k.secrets[k.current]})
	for _, id := range k.insertion {
		if id == k.current {
			continue
		}
		out = append(out, Entry{Version: id, Secret: k.secrets[id]})
	}
	return out
}

// Len reports the number of versions in the keyring.
func (k *Keyring) Len() int { return len(k.secrets) }

// Versions returns the version ids in map order — and ONLY the ids, never
// secret bytes — for boot-time operator visibility ("which versions are
// loaded, which is current").
func (k *Keyring) Versions() []string {
	out := make([]string, len(k.insertion))
	copy(out, k.insertion)
	return out
}
