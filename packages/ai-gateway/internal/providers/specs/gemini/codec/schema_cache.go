package codec

import (
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/goccy/go-json"
)

// Turning a caller's JSON Schema into Gemini's proto-backed Schema costs a
// decode plus a full recursive walk that allocates a fresh map per node. The
// input that walk runs on is immutable and repetitive: a tool declaration is
// fixed for the life of a conversation, and an agent resends the same
// declarations on every turn, so the same bytes produce the same schema over
// and over. Keying the finished schema on a digest of the caller's bytes
// collapses that to once per distinct schema and amortizes the decode with it.
//
// The cache is content-addressed rather than keyed on the tool name or the
// model: the bytes are the only input the result depends on, and a caller is
// free to change a schema under a name it has already used. Two byte
// sequences that differ only in whitespace or key order miss each other, which
// costs a rebuild but can never return the wrong schema — and a client
// resending its own serialized declarations sends identical bytes each turn.

const (
	// schemaCacheMaxEntryBytes caps a single cached schema. A declaration this
	// large is not the repetitive agent traffic the cache exists for, and
	// admitting one would let a single request claim most of the budget.
	// Oversize schemas still encode correctly, just without reuse.
	schemaCacheMaxEntryBytes = 256 << 10

	// schemaCacheMaxTotalBytes caps a generation's live entries. The key is a
	// digest of caller-supplied bytes, so callers choose how many distinct keys
	// exist and an unbounded map would be a memory-exhaustion vector. The
	// budget is charged against the encoded schema's exact length, and holds
	// the cache to a small, fixed share of the process while sitting far above
	// any real workload's tool set (a few dozen declarations of a few KB).
	schemaCacheMaxTotalBytes = 8 << 20
)

// preparedSchema is the finished product of the caller-JSON → Gemini-wire
// pipeline, reduced to what the encoder actually consumes.
//
// The encoded bytes are cached rather than the intermediate map because they
// are immutable. A map[string]any handed to every concurrent request is shared
// mutable state: any later edit to the encoder that wrote into the returned
// schema would corrupt the entry for every other request, and the resulting
// cross-request contamination would not be visible at the call site making the
// change. Bytes cannot be walked into, and their length is exactly measurable,
// which is what the cache budget is charged against.
type preparedSchema struct {
	// encoded is the sanitized schema, marshalled. Map keys are emitted in
	// sorted order, so embedding these bytes yields the same request body as
	// marshalling the schema in place would.
	encoded json.RawMessage

	// object reports that the schema sanitized to a non-empty JSON object.
	// Anything else means nothing in the caller's schema survived into a shape
	// the proto can express, and the call sites fall back rather than send it.
	object bool

	// droppedRefs lists the references the lenient pipeline degraded to open
	// objects (never populated in strict mode). The call site reports them as
	// coercions so the loosened contract reaches x-nexus-coerced. Shared and
	// read-only like the rest of the entry.
	droppedRefs []string
}

// schemaCacheGeneration is one bounded set of cached schemas plus the byte
// count charged against the budget.
type schemaCacheGeneration struct {
	entries sync.Map // digest → *preparedSchema
	bytes   atomic.Int64
}

// schemaCache holds the live generation. It is a pointer swap rather than a
// map guarded by a lock so the hit path — the overwhelming majority of calls,
// on a key set that goes stable as soon as a conversation starts — reads
// without acquiring anything. Every request on this process encodes through
// here, so a shared lock would serialize the encoder on itself.
var schemaCache = func() *atomic.Pointer[schemaCacheGeneration] {
	p := new(atomic.Pointer[schemaCacheGeneration])
	p.Store(&schemaCacheGeneration{})
	return p
}()

// schemaRefError marks the one failure in this pipeline the caller has to be
// told about: a reference that could not be folded in.
//
// The distinction is not cosmetic. Sanitization drops what the proto cannot
// express, so an unresolved $ref would reach Gemini as an empty schema — a
// contract the model was never given, answered with a 200 that no client can
// detect. That silent degradation is the defect the inliner exists to end, so
// its failures must reach the call sites as errors that fail the request.
// Every other failure here means "these bytes are not a usable schema", which
// the call sites answer with their default declaration.
type schemaRefError struct{ err error }

func (e *schemaRefError) Error() string { return e.err.Error() }
func (e *schemaRefError) Unwrap() error { return e.err }

// isSchemaRefFailure reports whether err is the must-surface class. Call sites
// that prepare a schema decide on this and nothing else: a bare err != nil
// would fail requests that the pipeline is meant to answer with a default, and
// a bare err == nil would swallow the class this whole file's error channel
// exists to carry.
func isSchemaRefFailure(err error) bool {
	var refErr *schemaRefError
	return errors.As(err, &refErr)
}

// prepareGeminiSchema turns one caller-supplied JSON Schema into the finished
// Gemini schema, reusing an earlier result when the same bytes have been seen.
// The returned value is shared with every other caller holding the same bytes
// and must be treated as read-only.
//
// Its error channel carries two classes, and the call sites must tell them
// apart with isSchemaRefFailure: a *schemaRefError has to fail the request,
// while anything else means the bytes are not a usable schema and the caller's
// default declaration is the answer — the same answer given to a schema that
// decodes but has nothing the proto can express. A step added to this pipeline
// that fails for a reason the caller needs to SEE must join the first class, or
// the call sites will quietly swallow it.
func prepareGeminiSchema(raw []byte, lenientRefs bool) (*preparedSchema, error) {
	if len(raw) == 0 || len(raw) > schemaCacheMaxEntryBytes {
		return buildPreparedSchema(raw, lenientRefs)
	}

	// SHA-256 because the key is derived from bytes the caller controls. A
	// non-cryptographic hash can be collided on purpose, and a collision here
	// would serve one tool's schema in answer to another's — a wrong request
	// on the wire with no error anywhere. Its collision resistance makes the
	// digest a safe stand-in for the bytes, so no second comparison is needed,
	// and hashing costs a fraction of the decode it saves.
	//
	// The reference-handling mode joins the key: for a schema carrying an
	// un-shipped reference the two modes produce different results (a degraded
	// entry vs an error), and serving the tool-parameters degradation to the
	// strict responseSchema path would smuggle the loosened contract into the
	// one place it must fail loudly. Schemas without such references — all of
	// the traffic that repeats — encode identically under both keys.
	digest := sha256.Sum256(raw)
	mode := byte(0)
	if lenientRefs {
		mode = 1
	}
	key := string(digest[:]) + string(mode)

	gen := schemaCache.Load()
	if hit, ok := gen.entries.Load(key); ok {
		return hit.(*preparedSchema), nil
	}

	prepared, err := buildPreparedSchema(raw, lenientRefs)
	if err != nil {
		// Failures are not cached, and a rejected schema is not remembered as
		// anything at all. Memoizing one as an empty schema would hand the next
		// turn the silent degradation the inliner exists to end, so the cache
		// must have no way to answer for a schema it never built. Keeping
		// failures off the budget also stops malformed input from evicting
		// schemas that are being reused; the rebuild a repeat failure pays is
		// what the uncached path costs anyway.
		return nil, err
	}

	cost := int64(len(prepared.encoded))
	if gen.bytes.Load()+cost > schemaCacheMaxTotalBytes {
		// Retire the whole generation instead of evicting one entry. The budget
		// is only reachable through a flood of schemas nobody reuses, and at
		// that point every entry is equally cold, so there is no recency worth
		// tracking — and tracking it would put the bookkeeping a strict order
		// needs onto the read path this design exists to keep clear. Callers
		// still holding the retired generation keep reading correct values from
		// it until it falls out of scope; the cost of dropping an entry is the
		// rebuild the uncached path was already paying.
		schemaCache.CompareAndSwap(gen, &schemaCacheGeneration{})
		return prepared, nil
	}
	// Concurrent inserts can each observe room and then all charge, so the
	// budget can overshoot by roughly the number of in-flight misses times
	// schemaCacheMaxEntryBytes before the next insert retires the generation.
	// The bound stays a bound; only its constant is approximate.
	if _, loaded := gen.entries.LoadOrStore(key, prepared); !loaded {
		gen.bytes.Add(cost)
	}
	return prepared, nil
}

// buildPreparedSchema runs the pipeline that prepareGeminiSchema caches: decode
// the caller's bytes, fold in the schema's own references, rewrite what is left
// into the subset Gemini's proto accepts, and marshal the result.
//
// Inlining belongs inside the cached pipeline rather than in front of it: it is
// a pure function of the same bytes the cache is keyed on, so caching the
// inline+sanitize pair is sound, and it keeps the walk it costs from being paid
// per request — which caching only the sanitize half would have left behind.
func buildPreparedSchema(raw []byte, lenientRefs bool) (*preparedSchema, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	// References must be folded in before sanitization, which has no way to
	// keep them. A reference that cannot be resolved is the caller's contract
	// stated in terms this wire cannot carry, and there is no honest way to
	// send it — so it fails here, loudly, rather than sanitizing away. The
	// lenient mode (tool parameters only) degrades the one never-resolvable
	// class to reported open objects; see inlineSchemaRefs.
	inlined, dropped, err := inlineSchemaRefs(decoded, lenientRefs)
	if err != nil {
		return nil, &schemaRefError{err: err}
	}
	sanitized := sanitizeGeminiSchema(inlined)
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return nil, err
	}
	node, ok := sanitized.(map[string]any)
	return &preparedSchema{encoded: encoded, object: ok && len(node) > 0, droppedRefs: dropped}, nil
}
