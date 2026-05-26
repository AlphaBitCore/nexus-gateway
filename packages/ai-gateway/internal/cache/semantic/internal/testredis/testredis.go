// Package testredis provides a miniredis-backed test helper that stubs the
// valkey-search wire commands (FT.CREATE, FT.DROPINDEX, FT.SEARCH) used by
// the semantic cache client.
//
// # Purpose
//
// The real valkey-search module is a compiled C++ shared library loaded into a
// Valkey server at startup.  It cannot run inside a Go test binary.
// miniredis (github.com/alicebob/miniredis/v2) is a pure-Go in-process Redis
// server — it handles standard Redis commands natively but has no knowledge of
// FT.* commands.
//
// This package registers stub handlers for the three FT.* commands that the
// semantic cache client issues:
//
//   - FT.CREATE  — records an index schema (name, dimension) in memory.
//   - FT.DROPINDEX — removes the schema record; idempotent (OK on missing).
//   - FT.SEARCH  — brute-force cosine KNN search over all HASH keys whose
//     name matches <indexPrefix>:*, reading the "vector" field
//     (packed FLOAT32 little-endian) from each key, applying any
//     TAG filters, and returning the top-k result(s) in the
//     RediSearch wire format expected by go-redis/v9.
//
// The stub is intentionally not production-grade: it is O(n) on the number of
// stored entries and performs no persistence, no HNSW approximation, and no
// TTL enforcement.  Its sole purpose is to let the semantic cache unit tests
// exercise the write/read path without a running Valkey + valkey-search.
//
// # Usage
//
//	addr, rdb, cleanup := testredis.NewMiniValkey(t)
//	defer cleanup()
//	// rdb is a *redis.Client connected to the stub server.
//
// # Dependencies
//
// This package is test-only; it must only be imported from _test.go files or
// other test helper packages.  It depends on:
//   - github.com/alicebob/miniredis/v2  (test-infra, not production)
//   - github.com/redis/go-redis/v9      (same dep as production code)
//
// Do not add this package to any production import path.
package testredis

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// indexMeta holds the metadata recorded by FT.CREATE for one index.
type indexMeta struct {
	name   string
	dim    int
	prefix string // e.g. "nexus:semantic-cache:v1:"
}

// MiniValkey is a miniredis instance extended with valkey-search stub handlers.
// Obtain one via NewMiniValkey.
type MiniValkey struct {
	mr      *miniredis.Miniredis
	log     *slog.Logger
	mu      sync.RWMutex
	indexes map[string]*indexMeta // keyed by index name (uppercase)
}

// NewMiniValkey starts a new MiniValkey and registers FT.* stub handlers.
// The returned cleanup func must be called when the test finishes (or use
// t.Cleanup).  The returned *redis.Client is pre-configured and ready to use.
//
//	addr, rdb, cleanup := testredis.NewMiniValkey(t)
//	t.Cleanup(cleanup)
func NewMiniValkey(tb testing.TB) (addr string, rdb *redis.Client, cleanup func()) {
	tb.Helper()
	addr, rdb, _, cleanup = NewMiniValkeyWithServer(tb)
	return addr, rdb, cleanup
}

// NewMiniValkeyWithServer is like NewMiniValkey but also returns the underlying
// *miniredis.Miniredis so callers can call FastForward to simulate time passage.
// Use this when testing TTL-based expiry.
//
//	addr, rdb, mr, cleanup := testredis.NewMiniValkeyWithServer(t)
//	mr.FastForward(time.Hour)
func NewMiniValkeyWithServer(tb testing.TB) (addr string, rdb *redis.Client, mr *miniredis.Miniredis, cleanup func()) {
	tb.Helper()

	var err error
	mr, err = miniredis.Run()
	if err != nil {
		tb.Fatalf("testredis: start miniredis: %v", err)
	}

	mv := &MiniValkey{
		mr:      mr,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		indexes: make(map[string]*indexMeta),
	}
	mv.registerHandlers()

	addr = mr.Addr()
	rdb = redis.NewClient(&redis.Options{Addr: addr})

	cleanup = func() {
		_ = rdb.Close()
		mr.Close()
	}
	return addr, rdb, mr, cleanup
}

// Command handler registration

func (mv *MiniValkey) registerHandlers() {
	srv := mv.mr.Server()
	mustRegister(srv, "FT.CREATE", mv.cmdFTCreate)
	mustRegister(srv, "FT.DROPINDEX", mv.cmdFTDropIndex)
	mustRegister(srv, "FT.SEARCH", mv.cmdFTSearch)
}

// mustRegister panics if cmd registration fails.  This is a programming
// error (duplicate command name in testredis itself), not a runtime failure.
func mustRegister(srv *server.Server, cmd string, f server.Cmd) {
	if err := srv.Register(cmd, f); err != nil {
		panic(fmt.Sprintf("testredis: register %s: %v", cmd, err))
	}
}

// FT.CREATE handler

// cmdFTCreate parses the minimal fields needed for the stub:
//
//	FT.CREATE <name> ON HASH PREFIX 1 <prefix> SCHEMA … DIM <d> …
//
// It replies OK on success, or an error if the index already exists.
func (mv *MiniValkey) cmdFTCreate(c *server.Peer, _ string, args []string) {
	if len(args) < 1 {
		c.WriteError("ERR FT.CREATE requires at least one argument")
		return
	}
	name := strings.ToUpper(args[0])

	// Parse PREFIX value.
	prefix := name + ":"
	for i := 1; i < len(args)-1; i++ {
		if strings.ToUpper(args[i]) == "PREFIX" && i+2 < len(args) {
			// PREFIX <count> <p1> [p2 …]
			count := 1
			if n, err := fmt.Sscanf(args[i+1], "%d", &count); n == 1 && err == nil && i+1+count < len(args) {
				prefix = args[i+2]
			}
			break
		}
	}

	// Parse DIM value from SCHEMA section.
	dim := 0
	for i := 1; i < len(args)-1; i++ {
		if strings.ToUpper(args[i]) == "DIM" {
			if _, err := fmt.Sscanf(args[i+1], "%d", &dim); err != nil {
				c.WriteError("ERR FT.CREATE: invalid DIM value")
				return
			}
			break
		}
	}

	mv.mu.Lock()
	defer mv.mu.Unlock()

	if _, exists := mv.indexes[name]; exists {
		c.WriteError("Index already exists")
		return
	}
	mv.indexes[name] = &indexMeta{name: name, dim: dim, prefix: prefix}
	mv.log.Debug("FT.CREATE", "index", name, "dim", dim, "prefix", prefix)
	c.WriteOK()
}

// FT.DROPINDEX handler

// cmdFTDropIndex removes an index record.  It is idempotent: a missing index
// returns OK (matching the valkey-search behaviour used by the flush job).
func (mv *MiniValkey) cmdFTDropIndex(c *server.Peer, _ string, args []string) {
	if len(args) < 1 {
		c.WriteError("ERR FT.DROPINDEX requires at least one argument")
		return
	}
	name := strings.ToUpper(args[0])

	mv.mu.Lock()
	delete(mv.indexes, name)
	mv.mu.Unlock()

	mv.log.Debug("FT.DROPINDEX", "index", name)
	c.WriteOK()
}

// FT.SEARCH handler

// scored holds a candidate HASH key with its cosine similarity score and
// field map, used during FT.SEARCH ranking.
type scored struct {
	key    string
	score  float64
	fields map[string]string
}

// cmdFTSearch implements the RediSearch KNN wire format subset used by the
// semantic cache client:
//
//	FT.SEARCH <indexName> "*=>[KNN <k> @vector $vec]"
//	  PARAMS 2 vec <blob>
//	  RETURN <n> <field1> … [SORTBY …] [LIMIT 0 <k>]
//
// It iterates every HASH key whose name starts with the index prefix, decodes
// the "vector" field as FLOAT32 little-endian, computes cosine similarity
// against the query vector, and returns the top-k result(s) in the format:
//
//	Array[
//	  Integer(total_results),
//	  BulkString(key1),
//	  Array[ BulkString(field), BulkString(value), … ],
//	  …
//	]
//
// Tag filters of the form "@tag:{value}" are parsed and applied before
// cosine ranking.
func (mv *MiniValkey) cmdFTSearch(c *server.Peer, _ string, args []string) {
	if len(args) < 2 {
		c.WriteError("ERR FT.SEARCH requires at least index and query")
		return
	}
	name := strings.ToUpper(args[0])
	query := args[1]

	mv.mu.RLock()
	meta, ok := mv.indexes[name]
	mv.mu.RUnlock()
	if !ok {
		c.WriteError(fmt.Sprintf("Unknown index name (first: %s)", name))
		return
	}

	// --- Parse PARAMS for query vector ---
	var queryVec []float32
	for i := 2; i < len(args)-2; i++ {
		if strings.ToUpper(args[i]) == "PARAMS" {
			// PARAMS <count> <k1> <v1> …
			count := 0
			if _, err := fmt.Sscanf(args[i+1], "%d", &count); err != nil {
				break
			}
			for j := 0; j < count; j += 2 {
				keyIdx := i + 2 + j
				valIdx := keyIdx + 1
				if valIdx >= len(args) {
					break
				}
				if strings.EqualFold(args[keyIdx], "vec") {
					queryVec = bytesToFloat32s([]byte(args[valIdx]))
				}
			}
			break
		}
	}

	// --- Parse KNN k from the query string ---
	// The KNN spec is embedded in the query: "*=>[KNN 3 @vector $vec]".
	// Extract the integer after the "KNN " token.
	k := 1
	if idx2 := strings.Index(strings.ToUpper(query), "KNN "); idx2 >= 0 {
		rest := query[idx2+4:]
		var parsed int
		if n, _ := fmt.Sscanf(rest, "%d", &parsed); n == 1 && parsed > 0 {
			k = parsed
		}
	}

	// --- Parse LIMIT (overrides KNN k when provided) ---
	for i := 2; i < len(args)-2; i++ {
		if strings.ToUpper(args[i]) == "LIMIT" && i+2 < len(args) {
			var limit int
			if _, err := fmt.Sscanf(args[i+2], "%d", &limit); err == nil && limit > 0 {
				k = limit
			}
		}
	}

	// --- Parse tag filters from query string ---
	// e.g. "(@upstream_provider:{openai} @upstream_model:{gpt-4o})*=>[KNN 1 @vector $vec]"
	tagFilters := parseTagFilters(query)

	// --- Scan matching HASH keys from miniredis ---
	// Use the Miniredis-level Keys() + HKeys() + HGet() API; HGetAll is not
	// part of the public API.
	allKeys := mv.mr.Keys()
	prefix := meta.prefix

	var candidates []scored

	for _, key := range allKeys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		// Build field map from individual HGet calls.
		fields, err := mv.mr.HKeys(key)
		if err != nil {
			continue // not a hash key or missing
		}
		hmap := make(map[string]string, len(fields))
		for _, f := range fields {
			hmap[f] = mv.mr.HGet(key, f)
		}
		// Apply tag filters.
		if !matchTagFilters(hmap, tagFilters) {
			continue
		}
		// Decode stored vector.
		vecBytes, ok2 := hmap["vector"]
		if !ok2 {
			continue
		}
		storedVec := bytesToFloat32s([]byte(vecBytes))
		if len(storedVec) == 0 || len(queryVec) == 0 {
			continue
		}
		sim := cosineSimilarity(queryVec, storedVec)
		candidates = append(candidates, scored{key: key, score: sim, fields: hmap})
	}

	// Sort descending by cosine similarity.
	sortByScoreDesc(candidates)

	// Cap at k.
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	// --- Write RediSearch response ---
	// Format: [total_count, key1, [field, val, ...], key2, ...]
	c.WriteLen(1 + len(candidates)*2)
	c.WriteInt(len(candidates))
	for _, cand := range candidates {
		c.WriteBulk(cand.key)
		// Return all fields.
		fields := make([]string, 0, len(cand.fields)*2)
		for fk, fv := range cand.fields {
			fields = append(fields, fk, fv)
		}
		// Append __vector_score for KNN result compatibility.
		// valkey-search returns cosine distance (1 − cos(θ)) in [0, 2];
		// the production Client.Lookup reads this field as __vector_score
		// (aliased via "AS __vector_score" in the FT.SEARCH query) and
		// converts it to similarity via cosineSimilarity(). The test stub
		// returns 1 − cosine_similarity which equals the cosine distance
		// when vectors are unit-normalised, matching valkey-search semantics.
		fields = append(fields, "__vector_score", fmt.Sprintf("%f", 1.0-cand.score))
		c.WriteStrings(fields)
	}
}

// Vector math helpers

// bytesToFloat32s decodes a byte slice as FLOAT32 little-endian values.
// Returns nil if the slice length is not a multiple of 4 or is empty.
func bytesToFloat32s(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		bits := binary.LittleEndian.Uint32(b[i*4 : i*4+4])
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// Float32sToBytes encodes a []float32 as FLOAT32 little-endian bytes.
// Exported so test code can build HSET payloads without reimplementing
// the encoding.
func Float32sToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// cosineSimilarity returns the cosine similarity of two float32 slices.
// Returns 0 if either vector is zero or lengths differ.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// Tag filter helpers

// tagFilter is a parsed "@field:{value}" constraint.
type tagFilter struct {
	field string
	value string
}

// parseTagFilters extracts "@field:{value}" constraints from a FT.SEARCH
// query string.  It handles the subset used by the semantic cache client:
//
//	(@upstream_provider:{openai} @upstream_model:{gpt-4o})*=>[KNN …]
func parseTagFilters(query string) []tagFilter {
	var filters []tagFilter
	// Scan for @word:{value} patterns.
	s := query
	for {
		at := strings.Index(s, "@")
		if at < 0 {
			break
		}
		rest := s[at+1:]
		brace := strings.Index(rest, ":{")
		if brace < 0 {
			break
		}
		field := rest[:brace]
		// field must be a single token (no spaces).
		if strings.ContainsAny(field, " \t\n()*") {
			s = rest
			continue
		}
		after := rest[brace+2:]
		end := strings.Index(after, "}")
		if end < 0 {
			break
		}
		value := after[:end]
		filters = append(filters, tagFilter{field: field, value: value})
		s = after[end+1:]
	}
	return filters
}

// unescapeTagValue removes escape backslashes inserted by the client's
// escapeTagValue helper so the stored field value can be compared to the
// query value verbatim. The production escapeTagValue escapes |, space, ,
// and - with a leading backslash; the stored HASH value is the raw string.
func unescapeTagValue(s string) string {
	return strings.NewReplacer(
		`\|`, "|",
		`\ `, " ",
		`\,`, ",",
		`\-`, "-",
	).Replace(s)
}

// matchTagFilters returns true if all tag filters match the HASH fields.
// Filter values are unescaped before comparison to handle the backslash
// escaping that escapeTagValue applies to hyphens and other special chars.
func matchTagFilters(hmap map[string]string, filters []tagFilter) bool {
	for _, f := range filters {
		v, ok := hmap[f.field]
		if !ok || !strings.EqualFold(v, unescapeTagValue(f.value)) {
			return false
		}
	}
	return true
}

// Sort helper (insertion sort — n is always small in unit tests)

// sortByScoreDesc sorts candidates in-place, highest cosine similarity first.
func sortByScoreDesc(s []scored) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j].score > s[j-1].score {
			s[j], s[j-1] = s[j-1], s[j]
			j--
		}
	}
}
