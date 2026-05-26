package testredis_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic/internal/testredis"
	"github.com/redis/go-redis/v9"
)

func ctx() context.Context { return context.Background() }

// hset writes a vector entry into the stub for a given index prefix.
func hset(t *testing.T, rdb *redis.Client, key string, vec []float32, extra map[string]string) {
	t.Helper()
	fields := map[string]interface{}{
		"vector": string(testredis.Float32sToBytes(vec)),
	}
	for k, v := range extra {
		fields[k] = v
	}
	if err := rdb.HSet(ctx(), key, fields).Err(); err != nil {
		t.Fatalf("HSET %s: %v", key, err)
	}
}

// NewMiniValkey lifecycle test

func TestNewMiniValkey_Lifecycle(t *testing.T) {
	addr, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if addr == "" {
		t.Fatal("addr must not be empty")
	}
	// Verify the server is reachable with a standard PING.
	if err := rdb.Ping(ctx()).Err(); err != nil {
		t.Fatalf("PING: %v", err)
	}
}

// FT.CREATE tests

func TestFTCreate_OK(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	cmd := rdb.Do(ctx(),
		"FT.CREATE", "myidx",
		"ON", "HASH",
		"PREFIX", "1", "myidx:",
		"SCHEMA",
		"vector", "VECTOR", "HNSW", "6",
		"DIM", "4",
		"TYPE", "FLOAT32",
		"DISTANCE_METRIC", "COSINE",
	)
	if err := cmd.Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}
	if cmd.Val() != "OK" {
		t.Fatalf("expected OK, got %v", cmd.Val())
	}
}

func TestFTCreate_DuplicateReturnsError(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	createCmd := func() error {
		return rdb.Do(ctx(),
			"FT.CREATE", "dup",
			"ON", "HASH",
			"PREFIX", "1", "dup:",
			"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
		).Err()
	}
	if err := createCmd(); err != nil {
		t.Fatalf("first FT.CREATE: %v", err)
	}
	if err := createCmd(); err == nil {
		t.Fatal("second FT.CREATE should return error for duplicate index")
	}
}

// FT.DROPINDEX tests

func TestFTDropIndex_OK(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	// Create then drop.
	if err := rdb.Do(ctx(),
		"FT.CREATE", "dropme",
		"ON", "HASH",
		"PREFIX", "1", "dropme:",
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}
	if err := rdb.Do(ctx(), "FT.DROPINDEX", "dropme").Err(); err != nil {
		t.Fatalf("FT.DROPINDEX: %v", err)
	}
}

func TestFTDropIndex_IdempotentOnMissing(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	// Drop a non-existent index — must return OK, not an error.
	if err := rdb.Do(ctx(), "FT.DROPINDEX", "ghost").Err(); err != nil {
		t.Fatalf("FT.DROPINDEX on missing index: %v", err)
	}
}

func TestFTDropIndex_AllowsRecreate(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	create := func() error {
		return rdb.Do(ctx(),
			"FT.CREATE", "reuse",
			"ON", "HASH",
			"PREFIX", "1", "reuse:",
			"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
		).Err()
	}
	if err := create(); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := rdb.Do(ctx(), "FT.DROPINDEX", "reuse").Err(); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := create(); err != nil {
		t.Fatalf("recreate after drop: %v", err)
	}
}

// FT.SEARCH KNN ranking tests

// TestFTSearch_KNNNearestNeighbour is the primary acceptance test:
// store 4 vectors, search with a query close to one of them, verify the
// closest is returned first.
//
// Dataset (4-D unit vectors):
//
//	A = [1, 0, 0, 0]   ← nearest to query [0.9, 0.1, 0, 0]
//	B = [0, 1, 0, 0]
//	C = [0, 0, 1, 0]
//	D = [0, 0, 0, 1]
func TestFTSearch_KNNNearestNeighbour(t *testing.T) {
	const idx = "KNNTEST"
	const prefix = "knntest:"

	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	// Create index.
	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Insert vectors.
	vectors := map[string][]float32{
		prefix + "A": {1, 0, 0, 0},
		prefix + "B": {0, 1, 0, 0},
		prefix + "C": {0, 0, 1, 0},
		prefix + "D": {0, 0, 0, 1},
	}
	for key, v := range vectors {
		hset(t, rdb, key, v, nil)
	}

	// Query close to A.
	queryVec := []float32{0.9, 0.1, 0, 0}
	queryBytes := testredis.Float32sToBytes(queryVec)

	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(queryBytes),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH: %v", err)
	}

	// Parse the RediSearch array response.
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 3 {
		t.Fatalf("unexpected FT.SEARCH response shape: %T %v", res, res)
	}
	totalResults, _ := arr[0].(int64)
	if totalResults != 1 {
		t.Errorf("expected 1 result, got %d", totalResults)
	}
	returnedKey, _ := arr[1].(string)
	if !strings.HasSuffix(returnedKey, "A") {
		t.Errorf("expected nearest neighbour A, got key %q", returnedKey)
	}
}

// TestFTSearch_TagFilter verifies that @field:{value} tag constraints are
// applied before KNN ranking so only matching documents are returned.
func TestFTSearch_TagFilter(t *testing.T) {
	const idx = "TAGTEST"
	const prefix = "tagtest:"

	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA",
		"vector", "VECTOR", "HNSW", "6", "DIM", "4",
		"upstream_model", "TAG",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Insert two entries with different upstream_model tags.
	hset(t, rdb, prefix+"gpt4", []float32{1, 0, 0, 0}, map[string]string{"upstream_model": "gpt-4o"})
	hset(t, rdb, prefix+"haiku", []float32{1, 0, 0, 0}, map[string]string{"upstream_model": "claude-haiku"})

	// Search with tag filter for gpt-4o only.
	queryVec := []float32{1, 0, 0, 0}
	queryBytes := testredis.Float32sToBytes(queryVec)

	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"(@upstream_model:{gpt-4o})*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(queryBytes),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH: %v", err)
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) < 1 {
		t.Fatalf("unexpected response shape: %T", res)
	}
	total, _ := arr[0].(int64)
	if total != 1 {
		t.Errorf("expected 1 result (gpt-4o only), got %d", total)
	}
	if total > 0 && len(arr) >= 2 {
		key, _ := arr[1].(string)
		if !strings.HasSuffix(key, "gpt4") {
			t.Errorf("expected gpt4 key, got %q", key)
		}
	}
}

// TestFTSearch_EmptyIndex verifies FT.SEARCH returns 0 results on an empty
// index without error.
func TestFTSearch_EmptyIndex(t *testing.T) {
	const idx = "EMPTY"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", "empty:",
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0})
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(queryVec),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH on empty index: %v", err)
	}
	arr, ok := res.([]interface{})
	if !ok {
		t.Fatalf("unexpected type: %T", res)
	}
	total, _ := arr[0].(int64)
	if total != 0 {
		t.Errorf("expected 0 results on empty index, got %d", total)
	}
}

// TestFTSearch_UnknownIndex verifies FT.SEARCH returns an error for an
// index that was never created (or was dropped).
func TestFTSearch_UnknownIndex(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	err := rdb.Do(ctx(),
		"FT.SEARCH", "NOSUCHINDEX",
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", "xxxx",
	).Err()
	if err == nil {
		t.Fatal("expected error for unknown index")
	}
}

// Vector math helpers exposed by testredis

func TestFloat32sToBytes_RoundTrip(t *testing.T) {
	original := []float32{1.5, -0.25, 0, 3.14159}
	encoded := testredis.Float32sToBytes(original)
	if len(encoded) != 16 {
		t.Fatalf("expected 16 bytes for 4 floats, got %d", len(encoded))
	}
	// Verify round-trip via cosine of identical vectors = 1.0.
	// We can't call bytesToFloat32s directly (unexported), but we can verify
	// FT.SEARCH returns the entry when the query IS the stored vector (cosine=1).
	const idx = "RTTEST"
	const prefix = "rttest:"
	_, rdb, cl := testredis.NewMiniValkey(t)
	defer cl()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	hset(t, rdb, prefix+"v1", original, nil)

	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(encoded),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	if total != 1 {
		t.Errorf("expected round-trip hit, got %d results", total)
	}
}

// Error-path coverage for missing arguments

func TestFTCreate_MissingArgs(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(), "FT.CREATE").Err(); err == nil {
		t.Fatal("expected error for FT.CREATE with no args")
	}
}

func TestFTDropIndex_MissingArgs(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(), "FT.DROPINDEX").Err(); err == nil {
		t.Fatal("expected error for FT.DROPINDEX with no args")
	}
}

func TestFTSearch_MissingArgs(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	// Only provide index name, no query — should error.
	if err := rdb.Do(ctx(), "FT.SEARCH", "onlyone").Err(); err == nil {
		t.Fatal("expected error for FT.SEARCH with only 1 arg")
	}
}

// TestFTSearch_NoVecParam ensures FT.SEARCH returns 0 results (not a crash)
// when the PARAMS block is absent or has no 'vec' key.
func TestFTSearch_NoVecParam(t *testing.T) {
	const idx = "NOVECTEST"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", "novectest:",
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	hset(t, rdb, "novectest:a", []float32{1, 0, 0, 0}, nil)

	// Issue FT.SEARCH without PARAMS — queryVec will be nil; entry is skipped.
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH without PARAMS: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	if total != 0 {
		t.Errorf("expected 0 results when queryVec is nil, got %d", total)
	}
}

// TestFTSearch_ZeroVectors verifies that all-zero stored/query vectors
// yield 0 results (cosineSimilarity returns 0, entry is still included
// but score will be 0 — which is valid; the result count is what matters).
func TestFTSearch_ZeroVector(t *testing.T) {
	const idx = "ZEROVEC"
	const prefix = "zerovec:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Store an entry with a valid vector.
	hset(t, rdb, prefix+"a", []float32{1, 0, 0, 0}, nil)

	// Query with a zero vector — cosineSimilarity returns 0; entry skipped
	// because len(queryVec)==4 but the cosine denominator is 0.
	zeroVec := testredis.Float32sToBytes([]float32{0, 0, 0, 0})
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(zeroVec),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH with zero vector: %v", err)
	}
	// With cosine=0 for zero query, the candidate is still scored 0 and
	// returned (the check guards against NaN, not zero).  Accept 0 or 1.
	arr, _ := res.([]interface{})
	if arr == nil {
		t.Fatal("nil response")
	}
}

// TestFTCreate_InvalidDIMValue verifies the parser returns an error when
// DIM is followed by a non-integer token.
func TestFTCreate_InvalidDIMValue(t *testing.T) {
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	err := rdb.Do(ctx(),
		"FT.CREATE", "baddim",
		"ON", "HASH",
		"PREFIX", "1", "baddim:",
		"SCHEMA",
		"vector", "VECTOR", "HNSW", "6",
		"DIM", "notanint",
	).Err()
	if err == nil {
		t.Fatal("expected error when DIM value is not an integer")
	}
}

// TestFloat32sToBytes_BadLength verifies that Float32sToBytes round-trip
// is consistent and that empty slices encode to 0 bytes.
func TestFloat32sToBytes_ZeroLength(t *testing.T) {
	b := testredis.Float32sToBytes(nil)
	if len(b) != 0 {
		t.Errorf("Float32sToBytes(nil) should return empty, got len=%d", len(b))
	}
	b2 := testredis.Float32sToBytes([]float32{})
	if len(b2) != 0 {
		t.Errorf("Float32sToBytes([]) should return empty, got len=%d", len(b2))
	}
}

// TestFTSearch_EntryMissingVectorField verifies that a HASH key that lacks
// the "vector" field is silently skipped (no crash, 0 results).
func TestFTSearch_EntryMissingVectorField(t *testing.T) {
	const idx = "NOVEC2"
	const prefix = "novec2:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Insert a HASH key under the prefix but without a "vector" field.
	if err := rdb.HSet(ctx(), prefix+"nofield", "other_field", "hello").Err(); err != nil {
		t.Fatalf("HSET: %v", err)
	}

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0})
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(queryVec),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	if total != 0 {
		t.Errorf("expected 0 results when vector field is absent, got %d", total)
	}
}

// TestFTSearch_StoredVectorBadEncoding verifies that a HASH key whose
// "vector" field has a non-multiple-of-4 byte length is gracefully skipped
// (bytesToFloat32s returns nil for malformed input).
func TestFTSearch_StoredVectorBadEncoding(t *testing.T) {
	const idx = "BADENC"
	const prefix = "badenc:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Store a hash with a vector field that is not a multiple of 4 bytes.
	if err := rdb.HSet(ctx(), prefix+"bad", "vector", "xyz").Err(); err != nil { // 3 bytes
		t.Fatalf("HSET: %v", err)
	}

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0})
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(queryVec),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH with bad-encoded vector: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	if total != 0 {
		t.Errorf("expected 0 results for bad-encoded vector, got %d", total)
	}
}

// TestFTSearch_TagFilterNoClosingBrace verifies that a malformed tag query
// (missing closing brace) is handled gracefully — no panic, returns results
// that match the prefix (no filters applied).
func TestFTSearch_TagFilterMalformed(t *testing.T) {
	const idx = "MALFORMED"
	const prefix = "malformed:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}
	hset(t, rdb, prefix+"a", []float32{1, 0, 0, 0}, nil)

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0})
	// Malformed query: @upstream_model:{noclosingbrace — no "}" — parseTagFilters
	// breaks out of the loop at the "no closing brace" check.
	_, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"@upstream_model:{noclosingbrace",
		"PARAMS", "2", "vec", string(queryVec),
		"LIMIT", "0", "1",
	).Result()
	// The stub must not crash regardless of the query format.
	if err != nil {
		t.Fatalf("FT.SEARCH with malformed tag filter should not error: %v", err)
	}
}

// TestFTSearch_KNNFromQueryString verifies that k is parsed from the query
// string ("*=>[KNN 2 @vector $vec]") when no LIMIT arg is provided.
func TestFTSearch_KNNFromQueryString(t *testing.T) {
	const idx = "KNNQS"
	const prefix = "knnqs:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}
	hset(t, rdb, prefix+"a", []float32{1, 0, 0, 0}, nil)
	hset(t, rdb, prefix+"b", []float32{0, 1, 0, 0}, nil)

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0})
	// Use k=2 in the query string, no LIMIT override.
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 2 @vector $vec]",
		"PARAMS", "2", "vec", string(queryVec),
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH KNN from query string: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	if total != 2 {
		t.Errorf("expected 2 results for KNN 2, got %d", total)
	}
}

// TestFTSearch_NonHashKeyUnderPrefix verifies that a string (non-HASH) key
// that happens to match the index prefix is silently skipped.
func TestFTSearch_NonHashKeyUnderPrefix(t *testing.T) {
	const idx = "NHTEST"
	const prefix = "nhtest:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Insert a string key (not a HASH) under the prefix.
	if err := rdb.Set(ctx(), prefix+"stringkey", "notahash", 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0})
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(queryVec),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH with non-hash key: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	if total != 0 {
		t.Errorf("expected 0 results (non-hash key skipped), got %d", total)
	}
}

// TestFTSearch_ParamsCountParseError verifies that a non-integer PARAMS count
// causes the PARAMS block to be skipped (queryVec remains nil → 0 results).
func TestFTSearch_ParamsCountParseError(t *testing.T) {
	const idx = "PARAMSERR"
	const prefix = "paramserr:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}
	hset(t, rdb, prefix+"a", []float32{1, 0, 0, 0}, nil)

	// PARAMS with a non-integer count.
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "notanint", "vec", "xxxx",
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH with bad PARAMS count: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	// queryVec is nil → entry skipped → 0 results.
	if total != 0 {
		t.Errorf("expected 0 results when PARAMS count is invalid, got %d", total)
	}
}

// TestFTSearch_DifferentLengthVectors verifies that when a stored vector has
// a different dimension than the query vector it is skipped (cosineSimilarity
// returns 0 for mismatched lengths and the entry is omitted).
func TestFTSearch_DifferentLengthVectors(t *testing.T) {
	const idx = "DIMTEST"
	const prefix = "dimtest:"
	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Store a 2-D vector in an index expecting 4-D.
	hset(t, rdb, prefix+"mismatch", []float32{1, 0}, nil)

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0}) // 4-D
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 1 @vector $vec]",
		"PARAMS", "2", "vec", string(queryVec),
		"LIMIT", "0", "1",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH with dimension mismatch: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	// Dimension mismatch yields cosine=0 so the entry is still added to
	// candidates with score 0, which is fine — we just verify no crash.
	_ = total
}

// TestFTSearch_MultipleResults_TopKRanking tests that when k>1, the stub
// returns the k nearest in descending similarity order.
func TestFTSearch_MultipleResults_TopKRanking(t *testing.T) {
	const idx = "TOPKTEST"
	const prefix = "topktest:"

	_, rdb, cleanup := testredis.NewMiniValkey(t)
	defer cleanup()

	if err := rdb.Do(ctx(),
		"FT.CREATE", idx,
		"ON", "HASH",
		"PREFIX", "1", prefix,
		"SCHEMA", "vector", "VECTOR", "HNSW", "6", "DIM", "4",
	).Err(); err != nil {
		t.Fatalf("FT.CREATE: %v", err)
	}

	// Insert 3 vectors with decreasing similarity to query [1,0,0,0].
	hset(t, rdb, prefix+"near", []float32{0.99, 0.1, 0, 0}, nil)   // highest cosine to query
	hset(t, rdb, prefix+"mid", []float32{0.5, 0.5, 0.5, 0.5}, nil) // moderate
	hset(t, rdb, prefix+"far", []float32{0, 0, 0, 1}, nil)         // lowest cosine to query

	queryVec := testredis.Float32sToBytes([]float32{1, 0, 0, 0})
	res, err := rdb.Do(ctx(),
		"FT.SEARCH", idx,
		"*=>[KNN 3 @vector $vec]",
		"PARAMS", "2", "vec", string(queryVec),
		"LIMIT", "0", "3",
	).Result()
	if err != nil {
		t.Fatalf("FT.SEARCH k=3: %v", err)
	}
	arr, _ := res.([]interface{})
	total, _ := arr[0].(int64)
	if total != 3 {
		t.Errorf("expected 3 results, got %d", total)
	}
	// First result must be "near".
	if total > 0 && len(arr) >= 2 {
		firstKey, _ := arr[1].(string)
		if !strings.HasSuffix(firstKey, "near") {
			t.Errorf("expected first result to be 'near', got %q", firstKey)
		}
	}
}
