package configstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestFinalizeSemanticCacheGet_ErrNoRows covers the fresh-DB branch:
// a missing singleton row must produce the conservative schema defaults.
func TestFinalizeSemanticCacheGet_ErrNoRows(t *testing.T) {
	got, err := finalizeSemanticCacheGet(&SemanticCacheConfigRow{}, pgx.ErrNoRows)
	if err != nil {
		t.Fatalf("ErrNoRows must not propagate; got: %v", err)
	}
	want := defaultSemanticCacheRow()
	if got.ID != want.ID {
		t.Errorf("ID: got %q, want %q", got.ID, want.ID)
	}
	if got.RedisIndexName != want.RedisIndexName {
		t.Errorf("RedisIndexName: got %q, want %q", got.RedisIndexName, want.RedisIndexName)
	}
	if got.Enabled != want.Enabled {
		t.Errorf("Enabled: got %v, want %v", got.Enabled, want.Enabled)
	}
	if got.EmbeddingFingerprint != "" {
		t.Errorf("EmbeddingFingerprint default must be empty; got %q", got.EmbeddingFingerprint)
	}
}

// TestFinalizeSemanticCacheGet_GenericError covers any other DB error path —
// must surface a wrapped "configstore: load semantic_cache_config:" prefix.
func TestFinalizeSemanticCacheGet_GenericError(t *testing.T) {
	want := errors.New("simulated DB outage")
	got, err := finalizeSemanticCacheGet(&SemanticCacheConfigRow{}, want)
	if err == nil {
		t.Fatal("generic err must propagate")
	}
	if !errors.Is(err, want) {
		t.Errorf("error must wrap original via %%w; got: %v", err)
	}
	if !strings.Contains(err.Error(), "configstore: load semantic_cache_config") {
		t.Errorf("missing package-attribution prefix: %q", err.Error())
	}
	if got != nil {
		t.Errorf("result must be nil on err; got: %+v", got)
	}
}

// TestFinalizeSemanticCacheGet_Success covers the happy path — the row
// pointer is returned unchanged.
func TestFinalizeSemanticCacheGet_Success(t *testing.T) {
	fp := "abcdef"
	row := &SemanticCacheConfigRow{
		ID:                   "singleton",
		EmbeddingFingerprint: fp,
		RedisIndexName:       "nexus:semantic-cache:v3",
		Enabled:              true,
	}
	got, err := finalizeSemanticCacheGet(row, nil)
	if err != nil {
		t.Fatalf("success path: %v", err)
	}
	if got != row {
		t.Errorf("must return the same pointer; got %p, want %p", got, row)
	}
	if got.EmbeddingFingerprint != fp {
		t.Errorf("fingerprint lost: %q", got.EmbeddingFingerprint)
	}
}

// TestDefaultSemanticCacheRow_PinsSchemaDefaults guards the contract between
// the Go-side fallback and the Prisma schema DEFAULTs. If the migration
// schema drifts, the fallback row a fresh DB returns would differ from a
// seeded row — causing subtle differences right after a clean install.
func TestDefaultSemanticCacheRow_PinsSchemaDefaults(t *testing.T) {
	row := defaultSemanticCacheRow()
	if row.ID != "singleton" {
		t.Errorf("ID: %q", row.ID)
	}
	if row.RedisIndexName != "nexus:semantic-cache:v1" {
		t.Errorf("RedisIndexName: %q", row.RedisIndexName)
	}
	if row.Enabled {
		t.Error("Enabled default: want false")
	}
	if row.EmbeddingFingerprint != "" {
		t.Errorf("EmbeddingFingerprint default: %q (want empty)", row.EmbeddingFingerprint)
	}
	if row.EmbeddingProviderID != nil || row.EmbeddingModelID != nil || row.EmbeddingDimension != nil {
		t.Error("provider/model/dim defaults must be nil")
	}
}

// TestComputeSemanticFingerprint_AllPresent covers the normal path where
// all three components are set — result must be a non-empty hex string.
func TestComputeSemanticFingerprint_AllPresent(t *testing.T) {
	p := "prov-1"
	m := "model-1"
	d := 1536
	fp := computeSemanticFingerprint(&p, &m, &d)
	if fp == "" {
		t.Fatal("expected non-empty fingerprint")
	}
	// Must be deterministic.
	fp2 := computeSemanticFingerprint(&p, &m, &d)
	if fp != fp2 {
		t.Errorf("fingerprint not deterministic: %q vs %q", fp, fp2)
	}
	// Different inputs must produce different fingerprints.
	m2 := "model-2"
	fp3 := computeSemanticFingerprint(&p, &m2, &d)
	if fp == fp3 {
		t.Errorf("same fingerprint for different models: %q", fp)
	}
}

// TestComputeSemanticFingerprint_NilProvider returns "" when providerID is nil.
func TestComputeSemanticFingerprint_NilProvider(t *testing.T) {
	m := "model-1"
	d := 1536
	if fp := computeSemanticFingerprint(nil, &m, &d); fp != "" {
		t.Errorf("nil providerID: want empty, got %q", fp)
	}
}

// TestComputeSemanticFingerprint_NilModel returns "" when modelID is nil.
func TestComputeSemanticFingerprint_NilModel(t *testing.T) {
	p := "prov-1"
	d := 1536
	if fp := computeSemanticFingerprint(&p, nil, &d); fp != "" {
		t.Errorf("nil modelID: want empty, got %q", fp)
	}
}

// TestComputeSemanticFingerprint_NilDim returns "" when dim is nil.
func TestComputeSemanticFingerprint_NilDim(t *testing.T) {
	p := "prov-1"
	m := "model-1"
	if fp := computeSemanticFingerprint(&p, &m, nil); fp != "" {
		t.Errorf("nil dim: want empty, got %q", fp)
	}
}

// TestComputeSemanticFingerprint_ZeroDim returns "" when dim is zero
// (signals "not yet probed / unknown").
func TestComputeSemanticFingerprint_ZeroDim(t *testing.T) {
	p := "prov-1"
	m := "model-1"
	zero := 0
	if fp := computeSemanticFingerprint(&p, &m, &zero); fp != "" {
		t.Errorf("zero dim: want empty, got %q", fp)
	}
}

// TestBumpIndexVersion_StandardCase covers the common v1 → v2 bump.
func TestBumpIndexVersion_StandardCase(t *testing.T) {
	got := bumpIndexVersion("nexus:semantic-cache:v1")
	if got != "nexus:semantic-cache:v2" {
		t.Errorf("v1→v2: got %q", got)
	}
}

// TestBumpIndexVersion_MultiDigit covers double-digit → triple-digit bump (v9→v10).
func TestBumpIndexVersion_MultiDigit(t *testing.T) {
	got := bumpIndexVersion("nexus:semantic-cache:v9")
	if got != "nexus:semantic-cache:v10" {
		t.Errorf("v9→v10: got %q", got)
	}
}

// TestBumpIndexVersion_NoTrailingVersion appends :v2 when no :vN suffix present.
func TestBumpIndexVersion_NoTrailingVersion(t *testing.T) {
	got := bumpIndexVersion("nexus:semantic-cache")
	if got != "nexus:semantic-cache:v2" {
		t.Errorf("no version → :v2: got %q", got)
	}
}

// TestBumpIndexVersion_LargeVersion covers a large version number.
func TestBumpIndexVersion_LargeVersion(t *testing.T) {
	got := bumpIndexVersion("nexus:semantic-cache:v99")
	if got != "nexus:semantic-cache:v100" {
		t.Errorf("v99→v100: got %q", got)
	}
}

// TestDefaultIndexName covers the singleton index-name default.
func TestDefaultIndexName(t *testing.T) {
	got := defaultIndexName()
	if got != "nexus:semantic-cache:v1" {
		t.Errorf("default: got %q", got)
	}
}
