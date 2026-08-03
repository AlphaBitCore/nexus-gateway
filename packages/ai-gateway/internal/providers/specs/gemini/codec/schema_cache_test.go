package codec

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"
)

// resetSchemaCache gives a test an empty cache to reason about, and restores
// the generation afterwards so tests cannot leak entries into each other.
func resetSchemaCache(t *testing.T) {
	t.Helper()
	previous := schemaCache.Load()
	schemaCache.Store(&schemaCacheGeneration{})
	t.Cleanup(func() { schemaCache.Store(previous) })
}

// toolSchema is a small nested declaration of the shape an agent resends on
// every turn: object → properties → nested object → array items, carrying keys
// the proto rejects ($comment, additionalProperties) so the sanitizer has real
// work to do.
const toolSchema = `{
	"type": "object",
	"$comment": "dropped: not a proto field",
	"additionalProperties": false,
	"properties": {
		"city": {"type": "string", "description": "city name"},
		"when": {"type": ["string", "null"], "format": "date-time"},
		"opts": {
			"type": "object",
			"properties": {
				"units": {"type": "string", "enum": ["c", "f"]},
				"days":  {"type": "array", "items": {"type": "integer"}}
			}
		}
	},
	"required": ["city"]
}`

// The cache exists because a tool declaration is immutable and an agent resends
// it on every turn. Re-running the decode + walk per turn is the cost being
// removed, so the same bytes must come back as the same finished schema rather
// than an equal one rebuilt from scratch. Bypassing the cache returns a freshly
// built value each call and fails this.
func TestPrepareGeminiSchema_ReusesTheFinishedSchemaAcrossTurns(t *testing.T) {
	resetSchemaCache(t)

	first, err := prepareGeminiSchema([]byte(toolSchema), false)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	for turn := 2; turn <= 5; turn++ {
		again, err := prepareGeminiSchema([]byte(toolSchema), false)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if again != first {
			t.Fatalf("turn %d rebuilt the schema instead of reusing it: the walk is running once per turn", turn)
		}
	}
}

// A conversation resends its declarations verbatim, so a repeat turn must not
// pay for the schema again. The hit path hashes the bytes and reads one entry;
// the walk it replaces allocates a map per schema node. Pinning the hit at a
// small constant is what makes a bypassed cache — which allocates in
// proportion to the schema — fail here.
func TestPrepareGeminiSchema_RepeatTurnDoesNotRebuild(t *testing.T) {
	resetSchemaCache(t)

	raw := []byte(toolSchema)
	if _, err := prepareGeminiSchema(raw, false); err != nil {
		t.Fatalf("priming turn: %v", err)
	}

	hit := testing.AllocsPerRun(200, func() {
		if _, err := prepareGeminiSchema(raw, false); err != nil {
			t.Fatalf("repeat turn: %v", err)
		}
	})
	build := testing.AllocsPerRun(200, func() {
		if _, err := buildPreparedSchema(raw, false); err != nil {
			t.Fatalf("uncached build: %v", err)
		}
	})

	// The hit path's own allocations are the digest key and its boxing — a
	// handful, independent of schema size.
	const hitCeiling = 8
	if hit > hitCeiling {
		t.Errorf("repeat turn allocated %.0f objects (ceiling %d): the schema is being rebuilt per turn", hit, hitCeiling)
	}
	// Guard the premise: if the walk were already cheap there would be nothing
	// to cache, and the ceiling above would be passing for the wrong reason.
	if build <= hit*2 {
		t.Errorf("uncached build allocated %.0f vs %.0f cached — the test is not discriminating", build, hit)
	}
	t.Logf("allocs/op: cached hit %.0f, uncached build %.0f", hit, build)
}

// The cache must never change the answer: a hit has to be indistinguishable
// from running the pipeline, or it trades allocations for a wrong request on
// the wire.
func TestPrepareGeminiSchema_MatchesTheUncachedPipeline(t *testing.T) {
	resetSchemaCache(t)

	for _, raw := range []string{
		toolSchema,
		`{"type":"object","properties":{"a":{"type":"string"}}}`,
		`{"type":["string","null"]}`,
		`{"const":"fixed"}`,
		`{"anyOf":[{"type":"string"},{"type":"integer","$comment":"drop me"}]}`,
		`{"$comment":"nothing proto-expressible survives"}`,
		`{"type":"array","items":[{"type":"string"},{"type":"integer"}]}`,
	} {
		want, err := buildPreparedSchema([]byte(raw), false)
		if err != nil {
			t.Fatalf("uncached build of %s: %v", raw, err)
		}
		got, err := prepareGeminiSchema([]byte(raw), false)
		if err != nil {
			t.Fatalf("cached prepare of %s: %v", raw, err)
		}
		if string(got.encoded) != string(want.encoded) {
			t.Errorf("cache changed the schema for %s:\n got: %s\nwant: %s", raw, got.encoded, want.encoded)
		}
		if got.object != want.object {
			t.Errorf("cache changed the fallback decision for %s: object=%v want %v", raw, got.object, want.object)
		}
	}
}

// The key is a digest of the caller's bytes, so two different schemas must not
// share an entry. A key that ignored content (a tool name, a model, a constant)
// would serve one declaration's schema in answer to another's — a wrong request
// upstream with no error raised anywhere.
func TestPrepareGeminiSchema_DistinctSchemasDoNotShareAnEntry(t *testing.T) {
	resetSchemaCache(t)

	city, err := prepareGeminiSchema([]byte(`{"type":"object","properties":{"city":{"type":"string"}}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := prepareGeminiSchema([]byte(`{"type":"object","properties":{"amount":{"type":"integer"}}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(amount.encoded), "city") {
		t.Fatalf("second schema served the first schema's entry: %s", amount.encoded)
	}
	if !strings.Contains(string(city.encoded), "city") || !strings.Contains(string(amount.encoded), "amount") {
		t.Fatalf("schemas crossed: city=%s amount=%s", city.encoded, amount.encoded)
	}
}

// A schema whose every key is unexpressible sanitizes to an empty object. That
// is the signal the call sites fall back on, so it must survive caching — a
// cached `object: true` here would put an empty schema on the wire.
func TestPrepareGeminiSchema_EmptyResultReportsNotAnObject(t *testing.T) {
	resetSchemaCache(t)

	prepared, err := prepareGeminiSchema([]byte(`{"$comment":"only unsupported keys"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.object {
		t.Errorf("a schema with nothing proto-expressible must not report as an object: %s", prepared.encoded)
	}
}

// Malformed input must surface as an error rather than a cached empty schema:
// the call sites fall back to the default declaration on error, and caching a
// failure would let one bad request poison the budget.
func TestPrepareGeminiSchema_MalformedSchemaErrorsAndIsNotCached(t *testing.T) {
	resetSchemaCache(t)

	malformed := []byte(`{"type":`)
	if _, err := prepareGeminiSchema(malformed, false); err == nil {
		t.Fatal("a truncated schema must error")
	}

	digest := sha256.Sum256(malformed)
	gen := schemaCache.Load()
	if _, cached := gen.entries.Load(string(digest[:])); cached {
		t.Error("the failed schema was cached: a later turn would read a result the pipeline never produced")
	}
	if got := gen.bytes.Load(); got != 0 {
		t.Errorf("the failed schema was charged %d bytes to the budget", got)
	}
	// It must keep failing rather than resolve to a cached empty schema.
	if _, err := prepareGeminiSchema(malformed, false); err == nil {
		t.Error("the second attempt at a truncated schema must still error")
	}
}

// The cached pipeline must include the inliner, not sit downstream of it. If
// reuse ever answered with a schema built by sanitization alone, the second
// turn of every nested model would ship the reference dropped — the exact
// silent degradation the inliner exists to end, reintroduced through the cache.
func TestPrepareGeminiSchema_ReusedSchemaIsTheInlinedOne(t *testing.T) {
	resetSchemaCache(t)

	nested := []byte(`{"$defs":{"Address":{"type":"object","properties":{"city":{"type":"string"}}}},
		"type":"object","properties":{"addr":{"$ref":"#/$defs/Address"}},"required":["addr"]}`)

	first, err := prepareGeminiSchema(nested, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareGeminiSchema(nested, false)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("the second turn rebuilt the schema instead of reusing it, so this test is not reading a cached value")
	}
	for _, turn := range []*preparedSchema{first, second} {
		body := string(turn.encoded)
		if !strings.Contains(body, `"city"`) {
			t.Errorf("the referenced model was lost: %s", body)
		}
		if strings.Contains(body, `"$ref"`) || strings.Contains(body, `"$defs"`) {
			t.Errorf("a reference reached the wire: %s", body)
		}
	}
}

// A reference that cannot be folded in must fail the request on every turn.
// Caching it — as a failure, or worse as the empty schema it sanitizes to —
// would let turn one ring the bell and turn two silently ship the caller a
// contract Gemini was never given.
func TestPrepareGeminiSchema_UnresolvableRefFailsEveryTurnAndIsNeverCached(t *testing.T) {
	resetSchemaCache(t)

	broken := []byte(`{"type":"object","properties":{"a":{"$ref":"#/$defs/Gone"}}}`)

	for turn := 1; turn <= 2; turn++ {
		prepared, err := prepareGeminiSchema(broken, false)
		if err == nil {
			t.Fatalf("turn %d: an unresolvable $ref must fail, got schema %s", turn, prepared.encoded)
		}
		if !isSchemaRefFailure(err) {
			t.Errorf("turn %d: the failure must reach the call sites as the must-surface class, got %v", turn, err)
		}
	}

	digest := sha256.Sum256(broken)
	gen := schemaCache.Load()
	if _, cached := gen.entries.Load(string(digest[:])); cached {
		t.Error("the rejected schema was cached: a later turn would read a result the pipeline never produced")
	}
}

// The two error classes must stay distinguishable. Malformed bytes are not a
// usable schema and not something the caller can act on — the call sites answer
// them with the default declaration. Were they to join the must-surface class,
// input that has always been tolerated would start failing requests.
func TestPrepareGeminiSchema_MalformedSchemaIsNotTheMustSurfaceClass(t *testing.T) {
	resetSchemaCache(t)

	_, err := prepareGeminiSchema([]byte(`{"type":`), false)
	if err == nil {
		t.Fatal("a truncated schema must error")
	}
	if isSchemaRefFailure(err) {
		t.Errorf("malformed bytes must not fail the request as a reference failure does: %v", err)
	}
}

// A scalar is valid JSON but not a schema. The sanitizer returns it unchanged,
// so `object` must be false and the call sites must fall back rather than send
// a bare scalar where the proto expects a Schema.
func TestPrepareGeminiSchema_NonObjectSchemaReportsNotAnObject(t *testing.T) {
	resetSchemaCache(t)

	prepared, err := prepareGeminiSchema([]byte(`"just a string"`), false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.object {
		t.Error("a non-object schema must not report as an object")
	}
}

// bigSchema builds a declaration with enough properties to exceed size, used to
// drive the two bounds.
func bigSchema(properties int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i := range properties {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"field_%06d":{"type":"string","description":"a description long enough to add weight %06d"}`, i, i)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// The per-entry cap keeps one oversized declaration from claiming most of the
// budget. It still has to encode correctly — the cap withholds reuse, not
// service.
func TestPrepareGeminiSchema_OversizeSchemaIsServedButNotCached(t *testing.T) {
	resetSchemaCache(t)

	raw := bigSchema(3000)
	if len(raw) <= schemaCacheMaxEntryBytes {
		t.Fatalf("fixture is %d bytes, needs to exceed the %d-byte entry cap", len(raw), schemaCacheMaxEntryBytes)
	}
	prepared, err := prepareGeminiSchema(raw, false)
	if err != nil {
		t.Fatalf("an oversize schema must still encode: %v", err)
	}
	if !prepared.object || !strings.Contains(string(prepared.encoded), "field_000000") {
		t.Fatalf("oversize schema encoded wrongly: object=%v", prepared.object)
	}
	// Not admitted, so it is rebuilt rather than reused — one request cannot
	// park a quarter of the budget on a schema nobody is resending.
	again, err := prepareGeminiSchema(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if again == prepared {
		t.Error("an oversize schema was admitted to the cache")
	}
	if string(again.encoded) != string(prepared.encoded) {
		t.Error("the uncached rebuild disagreed with the first encode")
	}
}

// The key is a digest of bytes the caller controls, so a caller can mint
// unlimited distinct keys. The budget is what stops that from being a
// memory-exhaustion vector: past it the cache retires its generation rather
// than growing, and keeps serving correct schemas throughout.
func TestSchemaCache_RetiresTheGenerationAtTheBudget(t *testing.T) {
	resetSchemaCache(t)

	raw := bigSchema(2000)
	if len(raw) > schemaCacheMaxEntryBytes {
		t.Fatalf("fixture is %d bytes, must fit under the %d-byte entry cap", len(raw), schemaCacheMaxEntryBytes)
	}

	retired := false
	before := schemaCache.Load()
	// Each schema is distinct, so nothing is ever reused — the flood case.
	for i := range 2*(schemaCacheMaxTotalBytes/len(raw)) + 2 {
		unique := append([]byte(nil), raw...)
		unique = append(unique[:len(unique)-1], []byte(fmt.Sprintf(`,"title":"%d"}`, i))...)

		prepared, err := prepareGeminiSchema(unique, false)
		if err != nil {
			t.Fatalf("schema %d: %v", i, err)
		}
		if !prepared.object {
			t.Fatalf("schema %d encoded wrongly under budget pressure", i)
		}
		gen := schemaCache.Load()
		if gen != before {
			retired = true
			before = gen
		}
		if got := gen.bytes.Load(); got > schemaCacheMaxTotalBytes {
			t.Fatalf("generation holds %d bytes, over the %d-byte budget", got, schemaCacheMaxTotalBytes)
		}
	}
	if !retired {
		t.Error("the cache never retired a generation: a caller minting distinct schemas grows it without bound")
	}
}

// Every request on the process encodes through this cache, so concurrent turns
// must agree on the schema. Run under -race, this is what proves the pointer
// swap and the shared entries are safe.
func TestPrepareGeminiSchema_ConcurrentTurnsAgreeOnTheSchema(t *testing.T) {
	resetSchemaCache(t)

	want, err := buildPreparedSchema([]byte(toolSchema), false)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				got, err := prepareGeminiSchema([]byte(toolSchema), false)
				if err != nil {
					errs <- err.Error()
					return
				}
				if string(got.encoded) != string(want.encoded) {
					errs <- "concurrent turn saw a different schema: " + string(got.encoded)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// The encoder embeds the cached bytes where it used to embed a map. Marshalling
// a map emits its keys in sorted order, so the cached form has to produce the
// identical body — otherwise the cache would quietly reshape the request.
func TestPreparedSchema_EmbedsIdenticallyToTheMap(t *testing.T) {
	resetSchemaCache(t)

	var decoded any
	if err := json.Unmarshal([]byte(toolSchema), &decoded); err != nil {
		t.Fatal(err)
	}
	viaMap, err := json.Marshal(map[string]any{"parameters": sanitizeGeminiSchema(decoded)})
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := prepareGeminiSchema([]byte(toolSchema), false)
	if err != nil {
		t.Fatal(err)
	}
	viaCache, err := json.Marshal(map[string]any{"parameters": prepared.encoded})
	if err != nil {
		t.Fatal(err)
	}

	if string(viaMap) != string(viaCache) {
		t.Errorf("cached schema does not embed identically to the map it replaced:\n map:   %s\n cache: %s", viaMap, viaCache)
	}
}

// One entry is handed to every concurrent encoder for as long as the tool set
// is in use, so building a request body out of it must leave it untouched. An
// encoder that compacted or rewrote the shared bytes in place would corrupt the
// schema for every turn that followed.
func TestPreparedSchema_IsNotDisturbedByBeingEncoded(t *testing.T) {
	resetSchemaCache(t)

	prepared, err := prepareGeminiSchema([]byte(toolSchema), false)
	if err != nil {
		t.Fatal(err)
	}
	before := string(prepared.encoded)

	for range 3 {
		if _, err := json.Marshal(map[string]any{
			"functionDeclarations": []map[string]any{{"name": "f", "parameters": prepared.encoded}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if string(prepared.encoded) != before {
		t.Errorf("encoding mutated the shared schema:\n before: %s\n after:  %s", before, prepared.encoded)
	}
	next, err := prepareGeminiSchema([]byte(toolSchema), false)
	if err != nil {
		t.Fatal(err)
	}
	if string(next.encoded) != before {
		t.Errorf("the next turn read a corrupted entry: %s", next.encoded)
	}
}

// BenchmarkGeminiSchema contrasts the per-turn cost the encoder pays now with
// the cost it paid when every turn re-ran the pipeline. The uncached cost
// scales with the schema, the cached cost does not, so both a small
// declaration and an agent-sized tool set are measured.
func BenchmarkGeminiSchema(b *testing.B) {
	for _, size := range []struct {
		name string
		raw  []byte
	}{
		{"small_declaration", []byte(toolSchema)},
		{"agent_tool_set", bigSchema(200)},
	} {
		b.Run(size.name, func(b *testing.B) {
			b.Logf("schema is %d bytes", len(size.raw))

			b.Run("uncached_rebuild_per_turn", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := buildPreparedSchema(size.raw, false); err != nil {
						b.Fatal(err)
					}
				}
			})

			b.Run("cached_repeat_turn", func(b *testing.B) {
				if _, err := prepareGeminiSchema(size.raw, false); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := prepareGeminiSchema(size.raw, false); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// The reference-handling mode is part of the cache key: the same bytes must
// yield the tool-path degradation under lenient mode and the loud failure
// under strict mode, in either order, without one answer being served for the
// other.
func TestPrepareGeminiSchema_ModeIsPartOfTheCacheKey(t *testing.T) {
	raw := []byte(`{"type":"object","properties":{"ctx":{"$ref":"#/components/schemas/ModeKeyProbe"}}}`)
	lenientFirst, err := prepareGeminiSchema(raw, true)
	if err != nil {
		t.Fatalf("lenient: %v", err)
	}
	if len(lenientFirst.droppedRefs) != 1 {
		t.Fatalf("lenient must degrade and report: %v", lenientFirst.droppedRefs)
	}
	if _, err := prepareGeminiSchema(raw, false); !isSchemaRefFailure(err) {
		t.Fatalf("strict on the same bytes must still fail loudly, not read the lenient entry: %v", err)
	}
	again, err := prepareGeminiSchema(raw, true)
	if err != nil {
		t.Fatalf("lenient again: %v", err)
	}
	if len(again.droppedRefs) != 1 {
		t.Fatalf("the lenient entry must survive the strict miss: %v", again.droppedRefs)
	}
}
