package payloadcapture

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"sync/atomic"
)

// configWire is the JSON shape exchanged between admin write
// (system_metadata["payload_capture.config"]), Hub Cat B aggregation,
// and the data-plane shadow reducers / startup loaders. Centralised here
// so a future field addition is a one-line change instead of a
// four-site coordination dance.
type configWire struct {
	StoreRequestBody   bool  `json:"storeRequestBody"`
	StoreResponseBody  bool  `json:"storeResponseBody"`
	MaxInlineBodyBytes int64 `json:"maxInlineBodyBytes"`
	MaxRequestBytes    int64 `json:"maxRequestBytes"`
	MaxResponseBytes   int64 `json:"maxResponseBytes"`
}

// DecodeConfigJSON is the canonical decoder for the
// system_metadata["payload_capture.config"] wire format. Empty or
// "null"/"{}" payloads return DefaultConfig() with a nil error so
// callers can use the result unconditionally. Malformed JSON returns
// DefaultConfig() with an error so callers can decide whether to log
// and proceed (loaders today) or surface the failure (admin-write path).
//
// Per-field coercion: any byte cap <= 0 is replaced with its
// DefaultConfig() counterpart so a partially-written row (e.g. a
// pre-iteration system_metadata blob without maxRequestBytes /
// maxResponseBytes) cannot collapse the data plane's read cap to zero,
// which would 413 every request. MaxInlineBodyBytes <= 0 coerces to
// DefaultMaxInlineBodyBytes — there is no "unlimited" semantic; the
// inline-vs-spill threshold is always finite.
func DecodeConfigJSON(raw []byte) (Config, error) {
	if len(raw) == 0 {
		return DefaultConfig(), nil
	}
	trimmed := string(raw)
	if trimmed == "null" || trimmed == "{}" {
		return DefaultConfig(), nil
	}
	var wire configWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return DefaultConfig(), fmt.Errorf("payloadcapture: decode wire: %w", err)
	}
	cfg := Config(wire)
	if cfg.MaxInlineBodyBytes <= 0 {
		cfg.MaxInlineBodyBytes = DefaultMaxInlineBodyBytes
	}
	// Network read caps must never be 0 — that would 413 every request.
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	return cfg, nil
}

// EncodeConfigJSON is the inverse of DecodeConfigJSON. Used by Hub Cat B
// aggregation and by control-plane admin writes to ensure the
// persisted/relayed shape always carries all five fields, so a stale
// reader on the data-plane side never has to rely on coercion to
// observe the admin-intended values.
func EncodeConfigJSON(cfg Config) ([]byte, error) {
	wire := configWire(cfg)
	b, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("payloadcapture: encode wire: %w", err)
	}
	return b, nil
}

// Store holds the currently active Config behind an atomic.Pointer so
// in-flight request goroutines can read the snapshot lock-free while an
// admin-driven shadow invalidation swaps in a new value from a different
// goroutine. Writers and readers never see a partially-updated Config: the
// pointer swap is atomic and each reader takes its own immutable copy.
type Store struct {
	cfg atomic.Pointer[Config]
}

// NewStore returns a Store primed with the given initial Config. Callers
// typically construct the store with DefaultConfig() at boot and then
// overwrite it with Set once system_metadata has been loaded.
func NewStore(initial Config) *Store {
	s := &Store{}
	s.Set(initial)
	return s
}

// Get returns a copy of the active Config. A nil internal pointer (which
// should only happen for a zero-value Store that was never initialised
// through NewStore) degrades to DefaultConfig() so hot-path consumers do
// not have to nil-check.
func (s *Store) Get() Config {
	p := s.cfg.Load()
	if p == nil {
		return DefaultConfig()
	}
	return *p
}

// Set atomically replaces the active Config. The supplied value is copied
// before being stored so the caller may continue to mutate its own local
// variable without affecting subsequent Get calls.
func (s *Store) Set(cfg Config) {
	c := cfg
	s.cfg.Store(&c)
}

// ApplyShadowState decodes a shadow-delivered JSON blob and swaps it into
// the Store. Wired into configsync.Manager's ShadowApplier dispatch table
// by data-plane services that pull payload_capture as a Cat B key.
//
// A nil, null, or "{}" payload is treated as a no-op so an initial
// shadow tick that lands before the Hub has aggregated Cat B state cannot
// wipe whatever the service already loaded at startup.
//
// The wire shape matches system_metadata["payload_capture.config"] —
// camelCase keys {storeRequestBody, storeResponseBody, maxInlineBodyBytes,
// maxRequestBytes, maxResponseBytes} — which is what Hub's
// AgentPayloadCaptureLoader returns and what admin_extras.go writes.
// All parsing and coercion is delegated to DecodeConfigJSON so the
// shadow path stays consistent with the data-plane startup loaders
// and Hub aggregation.
func (s *Store) ApplyShadowState(_ context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	trimmed := string(raw)
	if trimmed == "null" || trimmed == "{}" {
		return nil
	}
	cfg, err := DecodeConfigJSON(raw)
	if err != nil {
		return err
	}
	s.Set(cfg)
	return nil
}
