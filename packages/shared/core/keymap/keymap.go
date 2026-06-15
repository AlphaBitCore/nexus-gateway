// Package keymap is the single, stdlib-only leaf parser for the versioned
// "[*]vN:value" key-map wire format shared by every rotating-secret keyring in
// the gateway: the credential-encryption vault (control-plane
// crypto.MultiVault + ai-gateway creddecrypt.MultiDecryptor, the
// CREDENTIAL_KEY_MAP [MUST MATCH] pair) and the admin/virtual-key HMAC keyring
// (shared/core/hmackeyring, ADMIN_KEY_HMAC_KEY_MAP).
//
// Why one parser (F-0390 root cause): the format was previously hand-rolled in
// three independent copies that drifted. The ai-gateway copy did NOT strip the
// leading "*" current-marker, so an operator using the documented "*v2:" syntax
// (recommended in .env.example) made the Control Plane stamp ciphertext id "v2"
// while the gateway stored the same key under literal id "*v2" — every
// credential decrypt on the gateway then failed with "unknown key ID". A single
// leaf parser makes the CP minting side and the gateway opening side agree on
// the stored id byte-for-byte, which is exactly the [MUST MATCH] guarantee.
//
// Wire format: comma-separated entries; each entry is "id:value" split on the
// FIRST colon (so a value may itself contain colons — e.g. a base64 HMAC
// secret). Surrounding whitespace on the whole entry, the id, and the value is
// trimmed. A leading "*" on the id marks that entry as CURRENT; the "*" is
// STRIPPED from the stored id, so the current id is the same string a non-marked
// entry would carry. Blank entries (empty or whitespace-only, e.g. a trailing
// comma) are skipped.
//
// Current-version semantics (matches the pre-existing Control Plane and
// hmackeyring rule exactly): at most ONE entry may be "*"-marked — a second "*"
// is a configuration error (NOT last-*-wins; an ambiguous double-marked map must
// fail closed rather than silently pick one). If NO entry is marked, the LAST
// entry in textual order is current (the historical default that lets a
// single-entry or append-only map need no marker).
//
// Fail-closed errors: a missing colon, an empty id (including a bare "*"), a
// duplicate id, an empty map (no usable entries), more than one "*" marker, or
// any value rejected by the caller-supplied validate func all abort the parse.
// A malformed keyring must never silently admit under a partial set.
package keymap

import (
	"errors"
	"fmt"
	"strings"
)

// Parse splits a "[*]vN:value" key-map wire string into its entries and the
// designated current id.
//
//   - entries maps stored id (with any leading "*" stripped) to its raw value,
//     in caller-validated form.
//   - currentID is the id used for NEW operations: the unique "*"-marked entry,
//     or — when none is marked — the last entry in textual order.
//   - order preserves textual insertion order of the stored ids, so callers that
//     need a deterministic try-all sequence (e.g. the HMAC keyring) can use it.
//   - currentExplicit reports whether currentID was chosen by an explicit "*"
//     marker (true) or fell back to last-entry-wins (false) — for boot
//     telemetry / diagnostics only, never for behavior.
//
// validate is invoked once per entry as validate(id, value); a non-nil return
// aborts the parse with that error wrapped under the entry context. Callers
// supply their value-shape check there — a hex-64 master-key check for the AES
// vaults, a non-empty check for the HMAC keyring. validate may be nil to accept
// any non-structural value.
func Parse(env string, validate func(id, value string) error) (entries map[string]string, currentID string, order []string, currentExplicit bool, err error) {
	entries = make(map[string]string)
	var lastID, markedCurrent string
	for _, pair := range strings.Split(env, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			// Tolerate trailing/duplicate commas and whitespace-only entries.
			continue
		}
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			// Never echo the entry: the values these maps carry are secrets
			// (HMAC keyring, AES master-key map), and a pasted bare secret
			// with no "id:" prefix would land verbatim in boot logs.
			return nil, "", nil, false, errors.New("keymap: invalid entry without ':' separator (want \"[*]id:value\"; entry redacted)")
		}
		id, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])

		// A leading "*" designates the current entry; strip it so the stored id
		// is identical to what a non-marked entry would carry. This is the
		// F-0390 fix: every consumer keys by the stripped id.
		isCurrent := false
		if strings.HasPrefix(id, "*") {
			isCurrent = true
			id = strings.TrimSpace(strings.TrimPrefix(id, "*"))
		}
		if id == "" {
			// The entry's remainder is its secret value — never echo it.
			return nil, "", nil, false, errors.New("keymap: empty id in entry (want \"[*]id:value\"; entry redacted)")
		}
		if validate != nil {
			if verr := validate(id, value); verr != nil {
				return nil, "", nil, false, fmt.Errorf("keymap: id %q: %w", id, verr)
			}
		}
		if _, dup := entries[id]; dup {
			return nil, "", nil, false, fmt.Errorf("keymap: duplicate id %q", id)
		}
		if isCurrent {
			if markedCurrent != "" {
				return nil, "", nil, false, fmt.Errorf("keymap: more than one entry marked current ('*'): %q and %q", markedCurrent, id)
			}
			markedCurrent = id
		}
		entries[id] = value
		order = append(order, id)
		lastID = id
	}
	if len(entries) == 0 {
		return nil, "", nil, false, errors.New("keymap: empty key map")
	}
	if markedCurrent != "" {
		currentID = markedCurrent
		currentExplicit = true
	} else {
		currentID = lastID
	}
	return entries, currentID, order, currentExplicit, nil
}
