package policy

import (
	"database/sql"
	"errors"
	"github.com/goccy/go-json"
	"strings"
	"testing"
)

// TestPolicyFromQueryResult_MissingRowReturnsDefault covers the
// sql.ErrNoRows branch — a fresh deploy with no
// system_metadata.streaming_compliance row must run with the
// conservative DefaultPolicy() baseline, not crash.
func TestPolicyFromQueryResult_MissingRowReturnsDefault(t *testing.T) {
	got, err := policyFromQueryResult(nil, sql.ErrNoRows)
	if err != nil {
		t.Fatalf("ErrNoRows must not propagate; got: %v", err)
	}
	if got != DefaultPolicy() {
		t.Errorf("expected DefaultPolicy on ErrNoRows; got: %+v", got)
	}
}

// TestPolicyFromQueryResult_GenericErrorWraps covers the transient-
// error branch — any other DB error (timeout, conn-refused, planner
// error) must surface a wrapped "policy.LoadGlobalDefault:" prefix so
// operators can attribute the failure in service logs without
// re-deriving the package path.
func TestPolicyFromQueryResult_GenericErrorWraps(t *testing.T) {
	want := errors.New("simulated DB outage")
	got, err := policyFromQueryResult(nil, want)
	if err == nil {
		t.Fatal("generic DB err must propagate")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original via %%w; got: %v", err)
	}
	if !strings.Contains(err.Error(), "policy.LoadGlobalDefault") {
		t.Errorf("error must carry package-attribution prefix; got: %q", err.Error())
	}
	if got != DefaultPolicy() {
		t.Errorf("expected DefaultPolicy fallback on DB err; got: %+v", got)
	}
}

// TestPolicyFromQueryResult_SuccessfulDecode covers the happy path —
// a valid raw payload threads through to DecodeGlobalPolicy and the
// resulting Policy reflects the override.
func TestPolicyFromQueryResult_SuccessfulDecode(t *testing.T) {
	raw := json.RawMessage(`{"default_mode":"chunked_async","chunk_bytes":4096}`)
	got, err := policyFromQueryResult(raw, nil)
	if err != nil {
		t.Fatalf("expected clean decode; got: %v", err)
	}
	if got.Mode != ModeChunkedAsync {
		t.Errorf("decoded Mode = %q, want chunked_async", got.Mode)
	}
	if got.ChunkBytes != 4096 {
		t.Errorf("decoded ChunkBytes = %d, want 4096", got.ChunkBytes)
	}
}

// TestPolicyFromQueryResult_InvalidJSONFallsBackToDefault covers the
// JSON-parse-error path inside DecodeGlobalPolicy — operators who
// hand-edit a malformed row must not silently push a bad policy.
func TestPolicyFromQueryResult_InvalidJSONFallsBackToDefault(t *testing.T) {
	got, err := policyFromQueryResult(json.RawMessage("not json"), nil)
	if err == nil {
		t.Fatal("malformed JSON must surface a decode error")
	}
	if got != DefaultPolicy() {
		t.Errorf("decode err must return DefaultPolicy fallback; got: %+v", got)
	}
}
