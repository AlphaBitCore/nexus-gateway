package overrides

import (
	"bytes"
	"encoding/json"
	"errors"
)

// OverrideState wraps a JSON-object payload that fully replaces the template
// state for one (thing, config_key) pair. The wrapper enforces the spec
// invariant ("state MUST be a JSON object at top level") at construction
// time so any downstream caller — store, manager, future CLI/recovery
// scripts — cannot smuggle in `null`, an array, or a scalar.
//
// The underlying bytes are kept private so callers can only obtain a copy
// via Bytes(); this guarantees a value-typed OverrideState's bytes cannot
// be mutated in-flight by an unrelated caller, which would otherwise
// corrupt the merge cache (overrides ⊕ templates).
type OverrideState struct {
	raw json.RawMessage
}

// Construction errors. Mapped to HTTP 400 by the override write path.
var (
	// ErrEmptyState — no bytes supplied. The caller is expected to pass an
	// explicit state for every set-override call; clearing an override is a
	// separate code path.
	ErrEmptyState = errors.New("override state cannot be empty")

	// ErrInvalidJSONState — bytes are not well-formed JSON. Catches the
	// trivially-broken case before we look at the top-level type.
	ErrInvalidJSONState = errors.New("override state is not valid JSON")

	// ErrNonObjectState — bytes are well-formed JSON but the top-level type
	// is not an object (array, scalar, or null). The spec models override
	// state as a full replacement of the template state for one key, and
	// every templated state in the platform is currently a JSON object;
	// allowing a scalar would break the inSync byte-equal compare and the
	// per-key merge in recomputeDesiredTx.
	ErrNonObjectState = errors.New("override state must be a JSON object at top level")
)

// NewOverrideState validates that b is well-formed JSON whose top-level
// type is `object`, then returns a value-typed wrapper. Empty input is
// rejected.
//
// The returned OverrideState owns its own copy of b — caller mutation of
// the input slice after this call does not affect the wrapper.
func NewOverrideState(b []byte) (OverrideState, error) {
	if len(b) == 0 {
		return OverrideState{}, ErrEmptyState
	}
	if !json.Valid(b) {
		return OverrideState{}, ErrInvalidJSONState
	}
	// Decoder + Token gives us the top-level JSON type without doing the
	// full nested unmarshal — and crucially it distinguishes `null` from
	// `{}` (Unmarshal of `null` into map[string]json.RawMessage succeeds
	// silently, leaving probe == nil; we'd otherwise accept it).
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return OverrideState{}, ErrInvalidJSONState
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return OverrideState{}, ErrNonObjectState
	}
	out := OverrideState{raw: append(json.RawMessage(nil), b...)}
	return out, nil
}

// Bytes returns a copy of the underlying JSON bytes for storage / hashing.
// Callers MUST NOT mutate the returned slice — but the copy means a
// mistaken mutation cannot corrupt the wrapper's invariants.
func (s OverrideState) Bytes() []byte {
	if len(s.raw) == 0 {
		return nil
	}
	out := make([]byte, len(s.raw))
	copy(out, s.raw)
	return out
}

// MarshalJSON makes OverrideState round-trip cleanly through encoding/json.
// A zero-value OverrideState marshals to JSON null; a constructed one
// emits the underlying object bytes verbatim.
func (s OverrideState) MarshalJSON() ([]byte, error) {
	if len(s.raw) == 0 {
		return []byte("null"), nil
	}
	return s.Bytes(), nil
}

// UnmarshalJSON validates the bytes through the same path as
// NewOverrideState. This makes OverrideState safe to embed directly in
// any struct that decodes a request body — a non-object state on the
// wire surfaces as ErrNonObjectState rather than silent corruption.
func (s *OverrideState) UnmarshalJSON(b []byte) error {
	v, err := NewOverrideState(b)
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// overrideStateFromDB wraps DB-scanned bytes into an OverrideState without
// re-running the JSON-object check. The CHECK / NOT NULL constraints on
// thing_config_override.state guarantee valid object JSON at the row level,
// so this path is the canonical scan-time constructor. External callers
// must use NewOverrideState — this helper is package-internal.
func overrideStateFromDB(b []byte) OverrideState {
	if len(b) == 0 {
		return OverrideState{}
	}
	out := make([]byte, len(b))
	copy(out, b)
	return OverrideState{raw: out}
}
