package payloadcapture

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestStore_GetReturnsDefaultWhenNotInitialised(t *testing.T) {
	var s Store
	got := s.Get()
	want := DefaultConfig()
	if got != want {
		t.Errorf("zero-value Store.Get(): want %+v, got %+v", want, got)
	}
}

func TestNewStore_PrimesInitialValue(t *testing.T) {
	initial := Config{
		StoreRequestBody:   true,
		StoreResponseBody:  false,
		MaxInlineBodyBytes: 4096,
		MaxRequestBytes:    2 * 1024 * 1024,
		MaxResponseBytes:   3 * 1024 * 1024,
	}
	s := NewStore(initial)
	got := s.Get()
	if got != initial {
		t.Errorf("Get() after NewStore: want %+v, got %+v", initial, got)
	}
}

func TestStore_SetGetRoundtrip(t *testing.T) {
	s := NewStore(DefaultConfig())
	next := Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 128 * 1024,
		MaxRequestBytes:    25 * 1024 * 1024,
		MaxResponseBytes:   20 * 1024 * 1024,
	}
	s.Set(next)
	got := s.Get()
	if got != next {
		t.Errorf("after Set: want %+v, got %+v", next, got)
	}
}

func TestStore_SetCopiesValue(t *testing.T) {
	s := NewStore(DefaultConfig())
	cfg := Config{
		StoreRequestBody:   true,
		MaxInlineBodyBytes: 2048,
		MaxRequestBytes:    4096,
		MaxResponseBytes:   8192,
	}
	s.Set(cfg)

	cfg.StoreRequestBody = false
	cfg.MaxInlineBodyBytes = 1
	cfg.MaxRequestBytes = 1
	cfg.MaxResponseBytes = 1

	got := s.Get()
	if !got.StoreRequestBody {
		t.Errorf("StoreRequestBody: caller mutation leaked into Store")
	}
	if got.MaxInlineBodyBytes != 2048 {
		t.Errorf("MaxInlineBodyBytes: want 2048, got %d", got.MaxInlineBodyBytes)
	}
	if got.MaxRequestBytes != 4096 {
		t.Errorf("MaxRequestBytes: want 4096, got %d", got.MaxRequestBytes)
	}
	if got.MaxResponseBytes != 8192 {
		t.Errorf("MaxResponseBytes: want 8192, got %d", got.MaxResponseBytes)
	}
}

// TestStore_ConcurrentSetAndGet exercises the atomic pointer swap under
// the race detector. The assertion is that every Get observes one of
// the valid configurations — no torn reads, no data race reported by
// `go test -race`.
func TestStore_ConcurrentSetAndGet(t *testing.T) {
	s := NewStore(DefaultConfig())
	a := Config{StoreRequestBody: true, MaxInlineBodyBytes: 1024, MaxRequestBytes: 4096, MaxResponseBytes: 4096}
	b := Config{StoreResponseBody: true, MaxInlineBodyBytes: 8192, MaxRequestBytes: 4096, MaxResponseBytes: 4096}

	const iters = 2000
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range iters {
			if i%2 == 0 {
				s.Set(a)
			} else {
				s.Set(b)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range iters {
			s.Set(b)
		}
	}()

	wg.Add(4)
	for range 4 {
		go func() {
			defer wg.Done()
			for range iters {
				got := s.Get()
				if got != a && got != b && got != DefaultConfig() {
					t.Errorf("torn read: %+v", got)
					return
				}
			}
		}()
	}

	wg.Wait()
}

func TestStore_ApplyShadowState_EmptyIsNoop(t *testing.T) {
	initial := Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 4096,
		MaxRequestBytes:    5 * 1024 * 1024,
		MaxResponseBytes:   6 * 1024 * 1024,
	}

	for name, raw := range map[string]json.RawMessage{
		"nil":          nil,
		"empty":        json.RawMessage(""),
		"null":         json.RawMessage("null"),
		"empty object": json.RawMessage("{}"),
	} {
		t.Run(name, func(t *testing.T) {
			s := NewStore(initial)
			if err := s.ApplyShadowState(context.Background(), raw); err != nil {
				t.Fatalf("ApplyShadowState(%s): unexpected err %v", name, err)
			}
			if got := s.Get(); got != initial {
				t.Errorf("Get() after no-op apply: want %+v, got %+v", initial, got)
			}
		})
	}
}

func TestStore_ApplyShadowState_WellFormedRoundtrip(t *testing.T) {
	s := NewStore(DefaultConfig())
	raw := json.RawMessage(`{"storeRequestBody":true,"storeResponseBody":false,"maxInlineBodyBytes":8192,"maxRequestBytes":15728640,"maxResponseBytes":20971520}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := Config{
		StoreRequestBody:   true,
		StoreResponseBody:  false,
		MaxInlineBodyBytes: 8192,
		MaxRequestBytes:    15 * 1024 * 1024,
		MaxResponseBytes:   20 * 1024 * 1024,
	}
	if got := s.Get(); got != want {
		t.Errorf("Get(): want %+v, got %+v", want, got)
	}
}

// TestStore_ApplyShadowState_PartialRowFillsNetworkCapDefaults pins the
// behaviour for a system_metadata row that omits maxRequestBytes /
// maxResponseBytes. Missing network caps must coerce to defaults so an
// incomplete row never collapses the proxy's read cap to zero (which
// would 413 every request).
func TestStore_ApplyShadowState_PartialRowFillsNetworkCapDefaults(t *testing.T) {
	s := NewStore(DefaultConfig())
	raw := json.RawMessage(`{"storeRequestBody":true,"storeResponseBody":true,"maxInlineBodyBytes":131072}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got := s.Get()
	if got.MaxInlineBodyBytes != 131072 {
		t.Errorf("MaxInlineBodyBytes: want 131072, got %d", got.MaxInlineBodyBytes)
	}
	if got.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Errorf("MaxRequestBytes (partial row): want %d, got %d",
			DefaultMaxRequestBytes, got.MaxRequestBytes)
	}
	if got.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Errorf("MaxResponseBytes (partial row): want %d, got %d",
			DefaultMaxResponseBytes, got.MaxResponseBytes)
	}
}

func TestStore_ApplyShadowState_MalformedReturnsError(t *testing.T) {
	initial := Config{
		StoreRequestBody:   true,
		MaxInlineBodyBytes: 2048,
		MaxRequestBytes:    4 * 1024 * 1024,
		MaxResponseBytes:   4 * 1024 * 1024,
	}
	s := NewStore(initial)
	err := s.ApplyShadowState(context.Background(), json.RawMessage(`{not-json`))
	if err == nil {
		t.Fatal("ApplyShadowState(malformed): want error, got nil")
	}
	if !strings.Contains(err.Error(), "payloadcapture") {
		t.Errorf("error should be wrapped with payloadcapture prefix; got %v", err)
	}
	if got := s.Get(); got != initial {
		t.Errorf("malformed apply must not mutate Store; got %+v", got)
	}
}

// TestStore_ApplyShadowState_ZeroInlineCoercesToDefault pins that
// MaxInlineBodyBytes == 0 coerces to DefaultMaxInlineBodyBytes. The
// previous "0 = unlimited" semantic is removed; the inline-vs-spill
// threshold is always finite.
func TestStore_ApplyShadowState_ZeroInlineCoercesToDefault(t *testing.T) {
	s := NewStore(DefaultConfig())
	raw := json.RawMessage(`{"storeRequestBody":true,"storeResponseBody":true,"maxInlineBodyBytes":0}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got := s.Get()
	if got.MaxInlineBodyBytes != DefaultMaxInlineBodyBytes {
		t.Errorf("maxInlineBodyBytes=0 must coerce to DefaultMaxInlineBodyBytes (%d); got %d",
			DefaultMaxInlineBodyBytes, got.MaxInlineBodyBytes)
	}
	if !got.StoreRequestBody || !got.StoreResponseBody {
		t.Errorf("boolean fields must still round-trip; got %+v", got)
	}
}

func TestStore_ApplyShadowState_NegativeInlineCoercesToDefault(t *testing.T) {
	s := NewStore(DefaultConfig())
	raw := json.RawMessage(`{"maxInlineBodyBytes":-1}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := s.Get().MaxInlineBodyBytes; got != DefaultMaxInlineBodyBytes {
		t.Errorf("negative maxInlineBodyBytes must coerce to default; got %d", got)
	}
}

func TestStore_ApplyShadowState_NegativeNetworkCapsCoerceToDefault(t *testing.T) {
	s := NewStore(DefaultConfig())
	raw := json.RawMessage(`{"maxRequestBytes":-1,"maxResponseBytes":0}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got := s.Get()
	if got.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Errorf("negative maxRequestBytes must coerce to default; got %d", got.MaxRequestBytes)
	}
	if got.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Errorf("zero maxResponseBytes must coerce to default; got %d", got.MaxResponseBytes)
	}
}

func TestDecodeConfigJSON_EmptyReturnsDefault(t *testing.T) {
	cfg, err := DecodeConfigJSON(nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg != DefaultConfig() {
		t.Errorf("empty bytes should yield DefaultConfig, got %+v", cfg)
	}
}

func TestDecodeConfigJSON_NullStringReturnsDefault(t *testing.T) {
	cfg, err := DecodeConfigJSON([]byte("null"))
	if err != nil {
		t.Fatalf("null: %v", err)
	}
	if cfg != DefaultConfig() {
		t.Errorf("null payload should yield DefaultConfig: %+v", cfg)
	}
}

func TestDecodeConfigJSON_EmptyObjectReturnsDefault(t *testing.T) {
	cfg, err := DecodeConfigJSON([]byte("{}"))
	if err != nil {
		t.Fatalf("{}: %v", err)
	}
	if cfg != DefaultConfig() {
		t.Errorf("{} payload should yield DefaultConfig: %+v", cfg)
	}
}

func TestDecodeConfigJSON_MalformedReturnsDefaultPlusErr(t *testing.T) {
	cfg, err := DecodeConfigJSON([]byte(`{not-json`))
	if err == nil {
		t.Fatal("malformed JSON should produce error")
	}
	// Even on error, callers receive DefaultConfig — not a partially-
	// initialized zero-value Config (which would 413 every request).
	if cfg != DefaultConfig() {
		t.Errorf("malformed JSON must still return DefaultConfig: %+v", cfg)
	}
}

func TestDecodeConfigJSON_AllNegativesCoerceToDefault(t *testing.T) {
	raw := []byte(`{"maxInlineBodyBytes":-1,"maxRequestBytes":-1,"maxResponseBytes":-1}`)
	cfg, err := DecodeConfigJSON(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.MaxInlineBodyBytes != DefaultMaxInlineBodyBytes {
		t.Errorf("inline: %d want default", cfg.MaxInlineBodyBytes)
	}
	if cfg.MaxRequestBytes != DefaultMaxRequestBytes {
		t.Errorf("req: %d", cfg.MaxRequestBytes)
	}
	if cfg.MaxResponseBytes != DefaultMaxResponseBytes {
		t.Errorf("resp: %d", cfg.MaxResponseBytes)
	}
}

// TestEncodeDecode_Roundtrip confirms a config encoded by
// EncodeConfigJSON parses back identically through DecodeConfigJSON,
// including the new MaxInlineBodyBytes wire key.
func TestEncodeDecode_Roundtrip(t *testing.T) {
	in := Config{
		StoreRequestBody:   true,
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 65536,
		MaxRequestBytes:    7 * 1024 * 1024,
		MaxResponseBytes:   8 * 1024 * 1024,
	}
	b, err := EncodeConfigJSON(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeConfigJSON(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Errorf("roundtrip: got %+v, want %+v", out, in)
	}
}
