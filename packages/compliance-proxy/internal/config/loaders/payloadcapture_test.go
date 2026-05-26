package loaders

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
)

// The DB-bound LoadPayloadCaptureConfig is a thin shell that delegates to
// decodePayloadCaptureResult; the interesting decision tree (missing row,
// transient err, malformed JSON, success-with-coercion) is unit-tested
// here without a live database. The nil-DB short-circuit is the one
// branch that lives in the outer wrapper, so we cover it via the wrapper.

// TestLoadPayloadCaptureConfig_NilDBReturnsDefault locks in the nil-DB
// short-circuit. Callers that boot before they have a working DB handle
// must still receive a usable Config (default caps + capture off) so the
// proxy can serve traffic.
func TestLoadPayloadCaptureConfig_NilDBReturnsDefault(t *testing.T) {
	got, err := LoadPayloadCaptureConfig(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil DB must NOT error; got: %v", err)
	}
	want := payloadcapture.DefaultConfig()
	if got != want {
		t.Errorf("nil-DB Config drifted from DefaultConfig: got %+v, want %+v", got, want)
	}
}

// TestDecodePayloadCaptureResult_MissingRowReturnsDefault — fresh deploy
// where system_metadata["payload_capture.config"] has not been seeded.
// The conservative DefaultConfig (capture flags OFF + 10 MiB read caps)
// must come back with a nil error so service startup proceeds.
func TestDecodePayloadCaptureResult_MissingRowReturnsDefault(t *testing.T) {
	got, err := decodePayloadCaptureResult(nil, sql.ErrNoRows)
	if err != nil {
		t.Fatalf("ErrNoRows must not propagate; got: %v", err)
	}
	want := payloadcapture.DefaultConfig()
	if got != want {
		t.Errorf("missing-row result drifted: %+v vs %+v", got, want)
	}
	// Explicit invariants — capture must be OFF on a fresh deploy so an
	// admin never gets payloads persisted by accident.
	if got.StoreRequestBody || got.StoreResponseBody {
		t.Errorf("fresh-deploy default must keep both capture flags OFF: %+v", got)
	}
}

// TestDecodePayloadCaptureResult_GenericQueryErrorReturnsDefaultWrapped
// — any transient DB error must wrap with a `payload capture: query
// system_metadata` prefix so operators see the failure attribution in
// service logs. The fallback Config must still be DefaultConfig so the
// caller can choose whether to surface the err or run on defaults.
func TestDecodePayloadCaptureResult_GenericQueryErrorReturnsDefaultWrapped(t *testing.T) {
	want := errors.New("simulated DB outage")
	got, err := decodePayloadCaptureResult(nil, want)
	if err == nil {
		t.Fatal("generic err must propagate")
	}
	if !errors.Is(err, want) {
		t.Errorf("err must wrap original via %%w; got: %v", err)
	}
	if !strings.Contains(err.Error(), "payload capture: query system_metadata") {
		t.Errorf("err must carry attribution prefix; got: %q", err.Error())
	}
	if got != payloadcapture.DefaultConfig() {
		t.Errorf("err path must still return DefaultConfig fallback: %+v", got)
	}
}

// TestDecodePayloadCaptureResult_MalformedJSONReturnsDefaultWrapped — an
// operator hand-editing the row to invalid JSON must surface a wrapped
// `payload capture:` error AND keep the conservative default so the
// proxy does not crash on a typo.
func TestDecodePayloadCaptureResult_MalformedJSONReturnsDefaultWrapped(t *testing.T) {
	got, err := decodePayloadCaptureResult([]byte(`{"broken":`), nil)
	if err == nil {
		t.Fatal("malformed JSON must surface a decode error")
	}
	if !strings.Contains(err.Error(), "payload capture:") {
		t.Errorf("err must carry attribution prefix; got: %q", err.Error())
	}
	if got != payloadcapture.DefaultConfig() {
		t.Errorf("decode err must return DefaultConfig fallback: %+v", got)
	}
}

// TestDecodePayloadCaptureResult_SuccessDecodesAllFields — a valid blob
// rounds through DecodeConfigJSON's coercion rules. Use values inside the
// "stays as-written" range so we know each field threaded through (not
// silently coerced to a default).
func TestDecodePayloadCaptureResult_SuccessDecodesAllFields(t *testing.T) {
	raw := []byte(`{
		"storeRequestBody":true,
		"storeResponseBody":true,
		"maxInlineBodyBytes":131072,
		"maxRequestBytes":5242880,
		"maxResponseBytes":7340032
	}`)
	got, err := decodePayloadCaptureResult(raw, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !got.StoreRequestBody || !got.StoreResponseBody {
		t.Errorf("capture flags not threaded: %+v", got)
	}
	if got.MaxInlineBodyBytes != 131072 {
		t.Errorf("MaxInlineBodyBytes not threaded: got %d, want 131072", got.MaxInlineBodyBytes)
	}
	if got.MaxRequestBytes != 5242880 {
		t.Errorf("MaxRequestBytes not threaded: got %d", got.MaxRequestBytes)
	}
	if got.MaxResponseBytes != 7340032 {
		t.Errorf("MaxResponseBytes not threaded: got %d", got.MaxResponseBytes)
	}
}

// TestDecodePayloadCaptureResult_ZeroCapsCoercedToDefaults — a half-
// written row (storeRequestBody set, but no caps populated) must NOT
// collapse the network caps to zero; that would 413 every request.
// DecodeConfigJSON's coercion rules backfill DefaultMaxInlineBodyBytes /
// DefaultMaxRequestBytes / DefaultMaxResponseBytes.
func TestDecodePayloadCaptureResult_ZeroCapsCoercedToDefaults(t *testing.T) {
	raw := []byte(`{"storeRequestBody":true}`)
	got, err := decodePayloadCaptureResult(raw, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !got.StoreRequestBody {
		t.Errorf("explicit field lost: %+v", got)
	}
	if got.MaxRequestBytes != payloadcapture.DefaultMaxRequestBytes {
		t.Errorf("zero MaxRequestBytes must coerce to DefaultMaxRequestBytes, got %d", got.MaxRequestBytes)
	}
	if got.MaxResponseBytes != payloadcapture.DefaultMaxResponseBytes {
		t.Errorf("zero MaxResponseBytes must coerce to DefaultMaxResponseBytes, got %d", got.MaxResponseBytes)
	}
	if got.MaxInlineBodyBytes != payloadcapture.DefaultMaxInlineBodyBytes {
		t.Errorf("zero MaxInlineBodyBytes must coerce to DefaultMaxInlineBodyBytes, got %d", got.MaxInlineBodyBytes)
	}
}
