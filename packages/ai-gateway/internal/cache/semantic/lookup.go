package semantic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cachecore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/embeddings"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// defaultSearchTimeout is the per-FT.SEARCH hard timeout per
// response-cache-architecture.md §3.11.
const defaultSearchTimeout = 20 * time.Millisecond

// cosineSimilarity converts a valkey-search COSINE distance to a similarity
// score in [0, 1].
//
// valkey-search reports COSINE distance in [0, 2] where:
//   - 0 = identical vectors (maximum similarity)
//   - 2 = diametrically opposite vectors (minimum similarity)
//
// The relationship to cosine similarity is:
//
//	distance   = 1 − cos(θ)     (valkey-search internal formula)
//	distance_2 = distance / 2   (scaled to [0, 1] — NOT what valkey returns)
//
// BUT valkey-search COSINE actually returns distances in the [0, 2] range
// (0=identical, 2=opposite), so the correct inversion is:
//
//	similarity = 1 − distance/2
//
// Theoretical bounds [0, 1] may be exceeded by floating-point rounding in
// the HNSW approximate search; we clamp the result to [0, 1] to ensure the
// threshold comparison in the caller is always well-defined.
func cosineSimilarity(valkeyDist float32) float32 {
	s := 1.0 - valkeyDist/2.0
	if s < 0 {
		s = 0
	}
	if s > 1 {
		s = 1
	}
	return s
}

// ErrSearchTimeout is returned by Lookup when FT.SEARCH exceeds the per-query
// hard timeout (20 ms per §3.11).  The caller stamps
// GatewayCacheSkipReasonSemanticSearchTimeout.
var ErrSearchTimeout = fmt.Errorf("%w: FT.SEARCH hard timeout (20ms)", ErrSearchUnavailable)

// Lookup executes a KNN vector search (k=1) against the named HNSW index and
// returns the closest entry when cosine similarity ≥ in.Threshold.
//
// Filter applied:
//
//	(@vk_scope:{<vk>} @response_kind:{<kind>} @fingerprint:{<fp>}
//	 [@upstream_provider:{<p>} @upstream_model:{<m>}])
//	 =>[KNN 1 @vector $vec AS __vector_score]
//
// Returns (nil, nil) on a miss (0 results or similarity below threshold).
// Returns a typed ErrSearch* on FT.SEARCH failures.
func (c *Client) Lookup(ctx context.Context, indexName string, in *LookupInput) (*Entry, error) {
	if indexName == "" {
		return nil, fmt.Errorf("semantic/lookup: indexName is empty")
	}
	if len(in.Embedding) == 0 {
		return nil, fmt.Errorf("semantic/lookup: embedding is empty")
	}

	// Apply per-FT.SEARCH hard timeout.
	searchCtx, cancel := context.WithTimeout(ctx, defaultSearchTimeout)
	defer cancel()

	// Build tag filter parts.
	filterParts := []string{
		fmt.Sprintf("@vk_scope:{%s}", escapeTagValue(in.VKScope)),
		fmt.Sprintf("@response_kind:{%s}", escapeTagValue(in.ResponseKind)),
		fmt.Sprintf("@fingerprint:{%s}", escapeTagValue(in.Fingerprint)),
	}
	if !in.AllowCrossModel {
		filterParts = append(filterParts,
			fmt.Sprintf("@upstream_provider:{%s}", escapeTagValue(in.UpstreamProvider)),
			fmt.Sprintf("@upstream_model:{%s}", escapeTagValue(in.UpstreamModel)),
		)
	}
	filter := strings.Join(filterParts, " ")
	query := fmt.Sprintf("(%s)=>[KNN 1 @vector $vec AS __vector_score]", filter)

	// Encode embedding as FLOAT32 little-endian bytes.
	vecBytes := float32sToBytes(in.Embedding)

	// Issue FT.SEARCH via the low-level Do interface so we can pass the binary
	// PARAMS blob without base64 encoding.
	// FT.SEARCH <index> <query> PARAMS 2 vec <bytes> DIALECT 2
	// RETURN 9 __vector_score response_body usage upstream_provider upstream_model fingerprint cached_at origin_endpoint origin_body_format
	//
	// SORTBY intentionally omitted — Valkey-search's KNN query returns
	// results pre-sorted by __vector_score ascending; the explicit
	// SORTBY clause is rejected with "Unexpected argument `SORTBY`"
	// on Valkey 8.x. RedisSearch tolerates the redundant clause, but
	// the open-source Valkey search module does not. The top-N
	// ordering is preserved either way because KNN itself sorts.
	//
	// origin_wire_shape added for the B2 cross-ingress reshape gate —
	// the cache HIT reader compares the entry's ingress shape to the
	// requesting ingress's and calls canonicalbridge.ResponseAcrossFormats
	// to reshape when they differ. Pre-fix entries return empty strings
	// here; the reader's legacy branch handles them by falling back to
	// the prior canonical-assuming reshape behavior.
	args := []interface{}{
		"FT.SEARCH", indexName,
		query,
		"PARAMS", "2", "vec", string(vecBytes),
		"DIALECT", "2",
		"RETURN", "8",
		"__vector_score",
		"response_body",
		"usage",
		"upstream_provider",
		"upstream_model",
		"fingerprint",
		"cached_at",
		"origin_wire_shape",
	}

	raw, err := c.rdb.Do(searchCtx, args...).Result()
	if err != nil {
		if isIndexMissingError(err) {
			return nil, fmt.Errorf("%w: FT.SEARCH %q: index missing", ErrSearchUnavailable, indexName)
		}
		if searchCtx.Err() != nil {
			c.log.Warn("semantic/lookup: FT.SEARCH timed out",
				"index", indexName, "timeout_ms", 20)
			return nil, ErrSearchTimeout
		}
		if isNetworkError(err) {
			return nil, fmt.Errorf("%w: FT.SEARCH %q: %w", ErrValkeyUnavailable, indexName, err)
		}
		c.log.Warn("semantic/lookup: FT.SEARCH error", "index", indexName, "error", err)
		return nil, fmt.Errorf("%w: FT.SEARCH %q: %w", ErrSearchUnavailable, indexName, err)
	}

	// Parse the RESP2 reply: [<total_count>, <key>, [<field> <value> ...], ...]
	entry, err := parseSearchResult(raw, in.Threshold)
	if err != nil {
		c.log.Warn("semantic/lookup: parse error", "index", indexName, "error", err)
		return nil, fmt.Errorf("%w: parse %q result: %w", ErrSearchUnavailable, indexName, err)
	}
	return entry, nil
}

// parseSearchResult parses the raw RESP2 FT.SEARCH reply and returns an Entry
// when the best result's cosine similarity meets threshold.
// Returns (nil, nil) on 0 results or below threshold.
func parseSearchResult(raw interface{}, threshold float32) (*Entry, error) {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected RESP type %T; expected []interface{}", raw)
	}
	if len(arr) == 0 {
		return nil, nil
	}

	total, err := toInt64(arr[0])
	if err != nil {
		return nil, fmt.Errorf("parse total count: %w", err)
	}
	if total == 0 {
		return nil, nil
	}

	if len(arr) < 3 {
		return nil, fmt.Errorf("short RESP array: len=%d want >=3", len(arr))
	}

	// arr[1] is the Redis HASH key for the best match; capture it for the poison-list check.
	entryKey, _ := arr[1].(string)

	fieldArr, ok := arr[2].([]interface{})
	if !ok {
		return nil, fmt.Errorf("field array has type %T", arr[2])
	}

	fields := flatArrayToMap(fieldArr)

	distStr, ok := fields["__vector_score"]
	if !ok {
		return nil, fmt.Errorf("missing __vector_score in result")
	}
	dist, err := parseFloat32(distStr)
	if err != nil {
		return nil, fmt.Errorf("parse __vector_score: %w", err)
	}
	sim := cosineSimilarity(dist)

	if sim < threshold {
		// Threshold miss.
		return nil, nil
	}

	var usageMap map[string]any
	if usageStr, ok := fields["usage"]; ok && usageStr != "" && usageStr != "{}" {
		_ = json.Unmarshal([]byte(usageStr), &usageMap)
	}

	var cachedAt time.Time
	if catStr, ok := fields["cached_at"]; ok && catStr != "" {
		var epochSec int64
		if _, scanErr := fmt.Sscanf(catStr, "%d", &epochSec); scanErr == nil {
			cachedAt = time.Unix(epochSec, 0).UTC()
		}
	}

	return &Entry{
		ResponseBody:     []byte(fields["response_body"]),
		Usage:            usageMap,
		Similarity:       sim,
		CachedAt:         cachedAt,
		UpstreamProvider: fields["upstream_provider"],
		UpstreamModel:    fields["upstream_model"],
		Fingerprint:      fields["fingerprint"],
		EntryKey:         entryKey,
		OriginWireShape:  typology.WireShape(fields["origin_wire_shape"]),
	}, nil
}

// flatArrayToMap converts a flat [key, value, key, value, ...] RESP array
// to a map[string]string.
func flatArrayToMap(arr []interface{}) map[string]string {
	m := make(map[string]string, len(arr)/2)
	for i := 0; i+1 < len(arr); i += 2 {
		key, _ := arr[i].(string)
		val, _ := arr[i+1].(string)
		m[key] = val
	}
	return m
}

func toInt64(v interface{}) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	case string:
		var out int64
		_, err := fmt.Sscanf(n, "%d", &out)
		return out, err
	default:
		return 0, fmt.Errorf("unexpected type %T for count", v)
	}
}

func parseFloat32(s string) (float32, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return 0, err
	}
	return float32(f), nil
}

// escapeTagValue escapes characters with special meaning in Valkey-search TAG
// queries. Valkey-search treats `,` as the default TAG separator and `|` as
// the query OR-operator; both need escaping for literal matches. Hyphens
// and spaces are NOT special in Valkey-search TAG queries (unlike the
// RediSearch dialect, where `\-` was needed). Escaping `-` would cause every UUID-shaped TAG query
// (vk_scope, fingerprint, provider/model IDs) to miss indexed entries.
func escapeTagValue(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, ",", "\\,")
	return s
}

// isNetworkError returns true for errors indicating a Valkey connection failure.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "dial") ||
		strings.Contains(msg, "broken pipe")
}

// Reader — high-level L2 read orchestration

// ReadRequest carries all parameters the Reader needs to perform an L2 lookup.
type ReadRequest struct {
	// Scope / routing (required).
	VKScope          string
	UpstreamProvider string
	UpstreamModel    string
	ResponseKind     string // "response" | "stream"

	// Pre-computed embedding input text (via inputstaging.Plan).
	EmbeddingInput string

	// Embedding provider credentials.
	ProviderBaseURL string
	EmbeddingAPIKey string

	// EmbeddingWireModel is the upstream-facing model code (Model.providerModelId,
	// e.g. "text-embedding-3-small") to send in the embedding HTTP request.
	// When empty, falls back to ConfigSnapshot.EmbeddingModelID — which in
	// production is a Model row UUID, so the wire call would 404. Callers
	// MUST populate this from the catalog (h.deps.Models.GetModel(...).
	// ProviderModelID) before invoking Read.
	EmbeddingWireModel string

	// Policy.
	Threshold       float32
	AllowCrossModel bool

	// Cost accounting: cost per input token in USD, pre-computed by the caller.
	CostPerInputTokenUSD float64
}

// ReadResult is the output of Reader.Read.
type ReadResult struct {
	// Entry is non-nil on a hit.  Nil on miss, skip, or error.
	Entry *Entry

	// Outcome is one of:
	//   "hit"                    — similarity ≥ threshold.
	//   "threshold_miss"         — nearest neighbour below threshold (should not
	//                              reach here — Lookup returns nil on threshold miss).
	//   "miss"                   — 0 KNN results.
	//   "skip_disabled"          — L2 fleet-wide disabled.
	//   "skip_<reason>"          — embedding/search failure.
	Outcome string

	// SkipReason is the audit constant when Outcome starts with "skip_".
	SkipReason audit.GatewayCacheSkipReason

	// EmbeddingCostUSD is the cost (USD) of the embedding call, or 0.0 when
	// no embedding call was issued.
	EmbeddingCostUSD float64

	// EmbeddingModelID is the fleet-wide embedding model that produced the
	// embedding for this request.  Empty when no embedding call was issued
	// (e.g. skip_disabled path).  Stamped on traffic_event.embedding_model_id.
	EmbeddingModelID string
}

// Reader orchestrates the L2 read path.
type Reader struct {
	cache   *ConfigCache
	client  *Client
	sf      *EmbeddingSingleflight
	metrics *Metrics
	poison  PoisonList // negative-feedback poison list; never nil (nopPoisonList when not configured)
}

// NewReader constructs a Reader.  metrics may be nil (all metric calls are no-ops).
// poison may be nil; a nopPoisonList is used so the reader never crashes.
func NewReader(
	cache *ConfigCache,
	client *Client,
	sf *EmbeddingSingleflight,
	metrics *Metrics,
) *Reader {
	return &Reader{
		cache:   cache,
		client:  client,
		sf:      sf,
		metrics: metrics,
		poison:  nopPoisonList{},
	}
}

// NewReaderWithPoison constructs a Reader with a real PoisonList for negative-feedback filtering.
// poison may be nil; a nopPoisonList is substituted in that case.
func NewReaderWithPoison(
	cache *ConfigCache,
	client *Client,
	sf *EmbeddingSingleflight,
	metrics *Metrics,
	poison PoisonList,
) *Reader {
	pl := PoisonList(nopPoisonList{})
	if poison != nil {
		pl = poison
	}
	return &Reader{
		cache:   cache,
		client:  client,
		sf:      sf,
		metrics: metrics,
		poison:  pl,
	}
}

// Read executes the full L2 lookup for a single request.
//
// Flow:
//  1. Check EffectiveEnabled.
//  2. EmbeddingSingleflight.Embed; translate errors to skip-reasons.
//  3. Verify embedding dimension.
//  4. Client.Lookup; translate Valkey errors.
//  5. Check poison list — treat poisoned entries as miss.
//  6. Return ReadResult.
func (r *Reader) Read(ctx context.Context, req ReadRequest) (ReadResult, error) {
	// Step 1: fleet-wide enabled.
	if !r.cache.EffectiveEnabled() {
		r.metrics.IncLookup("skip_disabled")
		return ReadResult{
			Outcome:    "skip_disabled",
			SkipReason: audit.GatewayCacheSkipReasonSemanticUnavailable,
		}, nil
	}

	snap := r.cache.Get()

	// Step 2: embedding via singleflight. Use req.EmbeddingWireModel for
	// the upstream call (provider model id, e.g. "text-embedding-3-small"),
	// fall back to snap.EmbeddingModelID which is the DB UUID — that fallback
	// only works for tests that wire the snapshot with a string code.
	wireModel := req.EmbeddingWireModel
	if wireModel == "" {
		wireModel = snap.EmbeddingModelID
	}
	embedReq := embeddings.Request{
		Model:          wireModel,
		Input:          req.EmbeddingInput,
		Dimensions:     snap.EmbeddingDimension,
		EncodingFormat: "float",
	}

	resp, err := r.sf.Embed(ctx, snap.EmbeddingProviderID, req.ProviderBaseURL, wireModel, req.EmbeddingAPIKey, embedReq)
	if err != nil {
		reason := translateEmbedError(err)
		outcome := skipReasonToMetricLabel(reason)
		r.metrics.IncLookup(outcome)
		return ReadResult{
			Outcome:    outcome,
			SkipReason: reason,
		}, nil
	}

	costUSD := float64(resp.PromptTokens) * req.CostPerInputTokenUSD

	// Step 3: dimension verification.
	if snap.EmbeddingDimension > 0 && len(resp.Embedding) != snap.EmbeddingDimension {
		r.metrics.IncLookup("skip_embedding_dim_mismatch")
		return ReadResult{
			Outcome:          "skip_embedding_dim_mismatch",
			SkipReason:       audit.GatewayCacheSkipReasonEmbeddingDimMismatch,
			EmbeddingCostUSD: costUSD,
			EmbeddingModelID: snap.EmbeddingModelID,
		}, nil
	}

	// Step 4: FT.SEARCH.
	lookupIn := &LookupInput{
		VKScope:          req.VKScope,
		UpstreamProvider: req.UpstreamProvider,
		UpstreamModel:    req.UpstreamModel,
		ResponseKind:     req.ResponseKind,
		Fingerprint:      snap.Fingerprint,
		Embedding:        resp.Embedding,
		Threshold:        req.Threshold,
		AllowCrossModel:  req.AllowCrossModel,
	}

	entry, err := r.client.Lookup(ctx, snap.RedisIndexName, lookupIn)
	if err != nil {
		var reason audit.GatewayCacheSkipReason
		var outcome string
		switch {
		case isErrSearchTimeout(err):
			reason = audit.GatewayCacheSkipReasonSemanticSearchTimeout
			outcome = "skip_search_timeout"
		case isErrValkeyUnavailable(err):
			reason = audit.GatewayCacheSkipReasonValkeyUnavailable
			outcome = "skip_valkey_unavailable"
		default:
			reason = audit.GatewayCacheSkipReasonSemanticSearchError
			outcome = "skip_search_error"
		}
		r.metrics.IncLookup(outcome)
		r.metrics.ObserveLookupSimilarity(0)
		return ReadResult{
			Outcome:          outcome,
			SkipReason:       reason,
			EmbeddingCostUSD: costUSD,
			EmbeddingModelID: snap.EmbeddingModelID,
		}, nil
	}

	if entry == nil {
		// 0 KNN results or threshold miss.
		r.metrics.IncLookup("miss")
		r.metrics.ObserveLookupSimilarity(0)
		return ReadResult{
			Outcome:          "miss",
			EmbeddingCostUSD: costUSD,
			EmbeddingModelID: snap.EmbeddingModelID,
		}, nil
	}

	// Step 5: poison list check — treat poisoned entries as misses.
	if poisoned, poisonErr := r.poison.IsPoisoned(ctx, entry.EntryKey, req.VKScope); poisonErr != nil {
		// Fail-open: log the error but do not block the hit.
		// A poison-list availability failure must not degrade normal cache ops.
		_ = poisonErr // best-effort; the hit proceeds
	} else if poisoned {
		r.metrics.IncPoisonHits()
		r.metrics.IncLookup("skip_poisoned")
		r.metrics.ObserveLookupSimilarity(0)
		return ReadResult{
			Outcome:          "skip_poisoned",
			SkipReason:       audit.GatewayCacheSkipReasonPoisoned,
			EmbeddingCostUSD: costUSD,
			EmbeddingModelID: snap.EmbeddingModelID,
		}, nil
	}

	r.metrics.IncLookup("hit")
	r.metrics.ObserveLookupSimilarity(entry.Similarity)
	return ReadResult{
		Entry:            entry,
		Outcome:          "hit",
		EmbeddingCostUSD: costUSD,
		EmbeddingModelID: snap.EmbeddingModelID,
	}, nil
}

// isErrSearchTimeout returns true when err wraps ErrSearchTimeout.
func isErrSearchTimeout(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "hard timeout")
}

// isErrValkeyUnavailable returns true when err wraps ErrValkeyUnavailable.
func isErrValkeyUnavailable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Valkey unavailable")
}

// Entry → cache/core conversion helpers

// ToCacheStreamEntry converts an L2 Entry (response_kind=stream) to the
// *cachecore.StreamEntry shape expected by handleStreamHit.
func ToCacheStreamEntry(e *Entry) (*cachecore.StreamEntry, error) {
	if e == nil {
		return nil, fmt.Errorf("semantic/lookup: nil entry")
	}
	var chunks []cachecore.ChunkRecord
	if err := json.Unmarshal(e.ResponseBody, &chunks); err != nil {
		return nil, fmt.Errorf("semantic/lookup: decode stream chunks: %w", err)
	}
	usage := decodeUsage(e.Usage)
	return &cachecore.StreamEntry{
		Provider:        e.UpstreamProvider,
		Model:           e.UpstreamModel,
		Chunks:          chunks,
		Usage:           usage,
		CachedAt:        e.CachedAt,
		OriginWireShape: e.OriginWireShape,
	}, nil
}

// ToCacheResponseEntry converts an L2 Entry (response_kind=response) to the
// *cachecore.ResponseEntry shape expected by handleNonStreamHit.
func ToCacheResponseEntry(e *Entry) (*cachecore.ResponseEntry, error) {
	if e == nil {
		return nil, fmt.Errorf("semantic/lookup: nil entry")
	}
	usage := decodeUsage(e.Usage)
	return &cachecore.ResponseEntry{
		Provider:          e.UpstreamProvider,
		Model:             e.UpstreamModel,
		CanonicalResponse: e.ResponseBody,
		Usage:             usage,
		CachedAt:          e.CachedAt,
		OriginWireShape:   e.OriginWireShape,
	}, nil
}

// decodeUsage maps the JSON usage map stored in Valkey to provcore.Usage.
// provcore.Usage is an alias for normcore.Usage which uses *int pointer fields.
func decodeUsage(m map[string]any) provcore.Usage {
	if m == nil {
		return provcore.Usage{}
	}
	pt := anyToIntPtr(m["prompt_tokens"])
	ct := anyToIntPtr(m["completion_tokens"])
	tt := anyToIntPtr(m["total_tokens"])
	return provcore.Usage{
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      tt,
	}
}

func anyToIntPtr(v any) *int {
	switch n := v.(type) {
	case float64:
		i := int(n)
		return &i
	case int:
		return &n
	case int64:
		i := int(n)
		return &i
	}
	return nil
}
