// Anthropic prompt-cache double-count regression (S-079).
//
// Closes the gap left by the 2026-05 Anthropic cache cost double-count
// incident. The bug lived at two stamp sites in
// packages/ai-gateway/internal/ingress/proxy/proxy_cache.go (line 718 in
// the broker-leader live-stream path; line 919 in the broker non-stream
// MISS path): when our own gateway cache served a follow-up request as
// HIT_INFLIGHT, the joiner row still carried `cache_read_savings_usd`
// computed by computeCacheCosts AND the leader row also carried the
// same provider prompt-cache savings — so fleet rollups counted the
// same dollar twice.
//
// The fix zeroes the joiner's provider-side savings (CacheReadSavingsUsd,
// CacheWriteCostUsd, CacheNetSavingsUsd, CacheCreationTokens,
// CacheReadTokens, EstimatedCostUsd) and stamps GatewayCacheSavingsUsd
// with the full cost the joiner would have paid. The scenario asserts
// that contract end-to-end against an Anthropic-shape /v1/messages
// request: same prompt twice, second row is a gateway HIT (matching
// upstream `id`), and the second row's provider-cache savings are zero
// while the gateway-cache savings carry the avoided spend.
package scenarios_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS079_AnthropicDoubleCountFix — PM-grade regression guard.
//
// BRAINSTORM (pre): the bug double-billed Anthropic provider prompt-cache
// savings because the joiner row (gateway cache HIT_INFLIGHT / HIT) used
// the same upstream usage envelope the leader already accounted for. The
// fix clears the provider-side savings on the joiner and instead stamps
// `gateway_cache_savings_usd` with the full cost. So the cross-row
// invariant we can independently verify is:
//
//	row1.cache_read_savings_usd (leader)  + row2.cache_read_savings_usd (joiner)
//	  == row1.cache_read_savings_usd      // joiner contribution must be 0
//
// AND, independently:
//
//	row2.gateway_cache_savings_usd > 0    // the cache_status='hit'
//	row2.estimated_cost_usd == row2.gateway_cache_savings_usd
//	                                      // 2026-05-21 cost-stamping rule:
//	                                      // joiner row carries the
//	                                      // would-have-paid spend as
//	                                      // EstimatedCostUsd, and an equal
//	                                      // GatewayCacheSavingsUsd —
//	                                      // actual customer spend is
//	                                      // computed downstream as
//	                                      // EstimatedCostUsd −
//	                                      // GatewayCacheSavingsUsd, which
//	                                      // on a full HIT == 0.
//	row2.cache_read_savings_usd == 0      // the double-count canary
//	row2.cache_write_cost_usd   == 0
//
// The "independent calc" half of the test takes the leader row's
// cache_read_tokens + the Model row's input/cache-read prices and
// recomputes the canonical savings: it must match row1's value to within
// rounding. Pre-fix, row2 also carried that same savings, so a SUM
// across the two rows was ~2× the canonical value — the regression
// fingerprint.
//
// Arms:
//
//  1. Setup — CPLogin + CreateMyVK; pick the catalogue Anthropic model
//     `claude-haiku-4-5` (cheapest Claude on the dev DB). Hardened
//     scenario (2026-05-22): t.Fatalf if the model is missing/disabled
//     locally — the env must be properly seeded.
//
//  2. Warm cache — POST /v1/messages with a deterministic, per-test
//     nonce'd prompt. Wait for the leader's traffic_event row to land.
//
//  3. Cache hit — POST the same body again. Assert the returned upstream
//     `id` matches the leader's (cache served the second turn). Wait for
//     the joiner row to land and assert the double-count invariants.
//
// Hardened-precondition rationale: if the env cannot produce a gateway
// cache HIT on the second request (no Redis, gateway-cache disabled for
// /v1/messages, or Anthropic prompt-cache disabled for this model), the
// scenario fails fast (t.Fatalf) instead of silently skipping — the bug
// surface MUST be reachable in any env we ship the scenario to.
func TestS079_AnthropicDoubleCountFix(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	vkName := fmt.Sprintf("s079-%d", time.Now().UnixNano())
	vk, err := helpers.CreateMyVK(ctx, sc.Env, token, vkName)
	if err != nil {
		t.Fatalf("CreateMyVK: %v", err)
	}
	sc.Cleanup.Register("DeleteMyVK("+vk.ID+")", func() error {
		return helpers.DeleteMyVK(context.Background(), sc.Env, token, vk.ID)
	})

	// Confirm the Anthropic test model is reachable from this env BEFORE
	// burning an upstream call — saves a 30-s upstream timeout on
	// envs without Anthropic credentials wired. The dev/local catalogue
	// seeds models with the provider-versioned code suffix (e.g.
	// "claude-haiku-4-5-20251001"); the bare "claude-haiku-4-5" is the
	// alias the smoke shell test uses, but Model.code resolution is
	// exact-match so we look up the seeded code directly.
	const modelCode = "claude-haiku-4-5-20251001"
	var (
		inputPricePerM     float64
		cacheReadPricePerM *float64 // null = no provider cache discount configured
		modelEnabled       bool
	)
	{
		var readPriceRaw *float64
		err := sc.DB.QueryRow(ctx, `
			SELECT "inputPricePerMillion"::float8,
			       "cachedInputReadPricePerMillion"::float8,
			       enabled
			FROM "Model"
			WHERE code = $1
			LIMIT 1
		`, modelCode).Scan(&inputPricePerM, &readPriceRaw, &modelEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("S-079 precondition: Model.code=%q must exist in local catalogue; not seeded — "+
				"hardened scenario requires Anthropic claude-haiku-4-5 seeded in Model table "+
				"(re-run `cd tools/db-migrate && npx prisma db seed` against the local DB)", modelCode)
		}
		if err != nil {
			t.Fatalf("Model lookup for %q: %v", modelCode, err)
		}
		if !modelEnabled {
			t.Fatalf("S-079 precondition: Model.code=%q must be enabled; got enabled=false — "+
				"hardened scenario requires the model row enabled "+
				"(UPDATE \"Model\" SET enabled=true WHERE code='%s')",
				modelCode, modelCode)
		}
		cacheReadPricePerM = readPriceRaw
	}

	// Deterministic prompt — per-test nonce isolates this scenario's
	// gateway-cache key from any sibling cache scenario. Keep max_tokens
	// small so the upstream cost stays in the cents-per-test budget.
	//
	// Nonce is encoded as hex to keep it under 16 contiguous digits — the
	// PII scanner's credit_card pattern `\b(?:\d{4}[-\s]?){3}\d{4}\b` will
	// match any 16-digit run, and a unix-nano integer is 19 digits which
	// trips the hook with a 403 PII verdict before the request reaches
	// the cache-aware ingress path.
	//
	// 2026-05-22 upgrade: to actually exercise the Anthropic *provider*
	// prompt-cache surface (so leader.cache_creation_tokens > 0 — proves
	// the upstream cache was written), the request must carry an explicit
	// `cache_control: ephemeral` marker on a system block, AND the cached
	// prefix must exceed Anthropic's per-model minimum (1024 tokens for
	// haiku-class). We send a deterministic long system prompt (~2k tokens
	// of repeated boilerplate) with the marker on the last block. The
	// per-test nonce stays on the user content so the gateway-cache key
	// is unique per run — req2 still gets a fresh gateway HIT_INFLIGHT /
	// HIT and we keep the cross-row double-count invariant in scope.
	const longSystemPrefix = "You are a meticulous senior software engineering assistant " +
		"with deep expertise in distributed systems, database design, query " +
		"optimization, indexing strategies, OLAP vs OLTP trade-offs, data lake " +
		"architectures, Kafka, RabbitMQ, NATS, Redis Streams, Pulsar, SQS, " +
		"Docker, Kubernetes, Helm, service mesh, Istio, Envoy, container " +
		"security hardening, microservices patterns, API gateway design, " +
		"event sourcing, CQRS, saga patterns, circuit breakers, bulkheads, " +
		"rate limiting strategies, observability with OpenTelemetry, Prometheus, " +
		"Grafana, Loki, Tempo, Jaeger, Zipkin, and structured logging best " +
		"practices. You provide clean, idiomatic Go code, surface hidden " +
		"trade-offs, and back claims with citations from the std lib or RFCs. "
	// 30 copies × ~120 tokens ≈ 3600 tokens — well above the 1024-token
	// minimum for claude-haiku-4-5 cache_control engagement.
	var sysBuf strings.Builder
	for i := 0; i < 30; i++ {
		sysBuf.WriteString(longSystemPrefix)
	}
	longSystem := sysBuf.String()

	prompt := fmt.Sprintf(
		"Reply with exactly: ANTH_S079. nonce=%x",
		time.Now().UnixNano(),
	)
	body := mustMarshal(t, map[string]any{
		"model":      modelCode,
		"max_tokens": 16,
		"system": []map[string]any{
			{
				"type":          "text",
				"text":          longSystem,
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	})

	envForCall := *sc.Env
	envForCall.TestVK = vk.RawKey
	client := intg.LocalHTTPClient()

	parseAnthropicID := func(b []byte) string {
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(b, &out)
		return out.ID
	}

	// ─── Arm 2: warm cache ──────────────────────────────────────────────
	status1, body1, err := intg.AIGwPostJSON(&envForCall, client, "/v1/messages", body)
	if err != nil {
		t.Fatalf("req1 AIGwPostJSON: %v", err)
	}
	if status1 != 200 {
		// 401/403 = creds missing; 404 = /v1/messages not mounted; 502 =
		// upstream unreachable from this env. Hardened scenario treats all
		// of these as env precondition failures — the test env must have
		// a working Anthropic credential + /v1/messages route mounted.
		t.Fatalf("S-079 precondition: /v1/messages must return 200 for %q; got status=%d (%q) — "+
			"hardened scenario requires a live Anthropic credential and the /v1/messages ingress "+
			"mounted on the AI Gateway (check Credential row for provider=anthropic + "+
			"AI Gateway route registration)",
			modelCode, status1, truncate(body1, 200))
	}
	id1 := parseAnthropicID(body1)
	if id1 == "" {
		t.Fatalf("req1 missing Anthropic message id (body=%q)", truncate(body1, 200))
	}

	// Brief pause so the cache write commits before request 2 lookups it.
	// Matches the 2-second guard used across the cache scenario family
	// (cache_test.go S-060, cache_negative_feedback_test.go S-066).
	time.Sleep(2 * time.Second)

	// ─── Arm 3: cache hit ───────────────────────────────────────────────
	status2, body2, err := intg.AIGwPostJSON(&envForCall, client, "/v1/messages", body)
	if err != nil {
		t.Fatalf("req2 AIGwPostJSON: %v", err)
	}
	if status2 != 200 {
		t.Fatalf("req2 expected 200, got %d (%q)", status2, truncate(body2, 200))
	}
	id2 := parseAnthropicID(body2)
	if id2 == "" {
		t.Fatalf("req2 missing Anthropic message id (body=%q)", truncate(body2, 200))
	}
	if id1 != id2 {
		// Without a gateway-cache HIT we can't expose the joiner stamp
		// path the fix lives in. Hardened scenario treats this as a real
		// regression: the gateway response cache for /v1/messages must
		// serve req2 from the leader's entry.
		t.Fatalf("S-079 precondition: gateway cache must HIT on the second /v1/messages call; "+
			"got distinct ids (id1=%s id2=%s) — cache did not serve req2. "+
			"Hardened scenario requires the gateway response cache enabled for /v1/messages "+
			"AND Anthropic prompt cache wired in this env (check ResponseCacheConfig + Redis up + "+
			"per-VK cache routing rule does not opt out).",
			id1, id2)
	}

	// ─── Cross-check: pull the two traffic_event rows ───────────────────
	// Both rows attribute to this VK via identity->'vk'->>'id'; the leader
	// (req1) is the MISS row, the joiner (req2) is the HIT/HIT_INFLIGHT
	// row. Poll up to 30 s for both rows to land — the broker's terminal
	// chunk drives the audit insert, which races the HTTP response.
	type ev struct {
		id                     string
		gatewayCacheStatus     string
		providerCacheStatus    string
		promptTokens           int64
		cacheCreationTokens    int64
		cacheReadTokens        int64
		estimatedCostUsd       float64
		gatewayCacheSavingsUsd float64
		cacheReadSavingsUsd    float64
		cacheWriteCostUsd      float64
		cacheNetSavingsUsd     float64
	}
	var leader, joiner ev
	{
		const tries = 6
		const interval = 5 * time.Second
		// We need at least two rows for this VK+model in the last 120 s,
		// ordered by created_at. The earliest is the leader (MISS), the
		// latest is the joiner (HIT / HIT_INFLIGHT). pgx scans NUMERIC
		// into *float64 cleanly via ::float8 in the SELECT list.
		const query = `
			SELECT id,
			       COALESCE(gateway_cache_status, ''),
			       COALESCE(provider_cache_status, ''),
			       COALESCE(prompt_tokens, 0),
			       COALESCE(cache_creation_tokens, 0),
			       COALESCE(cache_read_tokens, 0),
			       COALESCE(estimated_cost_usd::float8, 0),
			       COALESCE(gateway_cache_savings_usd::float8, 0),
			       COALESCE(cache_read_savings_usd::float8, 0),
			       COALESCE(cache_write_cost_usd::float8, 0),
			       COALESCE(cache_net_savings_usd::float8, 0)
			FROM traffic_event
			WHERE source = 'ai-gateway'
			  AND identity->'vk'->>'id' = $1
			  AND path = '/v1/messages'
			  AND "timestamp" > NOW() - INTERVAL '300 seconds'
			ORDER BY created_at ASC
			LIMIT 4`
		var rows []ev
		for i := 0; i < tries; i++ {
			rows = rows[:0]
			r, qErr := sc.DB.Query(ctx, query, vk.ID)
			if qErr != nil {
				t.Fatalf("traffic_event poll (attempt %d): %v", i+1, qErr)
			}
			for r.Next() {
				var e ev
				if scanErr := r.Scan(
					&e.id, &e.gatewayCacheStatus, &e.providerCacheStatus,
					&e.promptTokens, &e.cacheCreationTokens, &e.cacheReadTokens,
					&e.estimatedCostUsd, &e.gatewayCacheSavingsUsd,
					&e.cacheReadSavingsUsd, &e.cacheWriteCostUsd, &e.cacheNetSavingsUsd,
				); scanErr != nil {
					r.Close()
					t.Fatalf("traffic_event scan: %v", scanErr)
				}
				rows = append(rows, e)
			}
			r.Close()
			if len(rows) >= 2 {
				break
			}
			if i < tries-1 {
				time.Sleep(interval)
			}
		}
		if len(rows) < 2 {
			t.Fatalf("S-079 requires 2 traffic_event rows for VK %s on /v1/messages within 30 s; got %d. "+
				"Audit pipeline did not stamp both leader + joiner rows — broker chunk delivery may be lagged.",
				vk.ID, len(rows))
		}
		leader = rows[0]
		joiner = rows[len(rows)-1]
	}

	// ─── Joiner invariants (the regression surface) ─────────────────────
	// On HIT / HIT_INFLIGHT the joiner row MUST have provider-side savings
	// columns cleared — the leader already accounted for them. Pre-fix,
	// these all carried the same upstream usage envelope as the leader,
	// inflating fleet rollups by 2×.
	joinerHit := joiner.gatewayCacheStatus == "hit" || joiner.gatewayCacheStatus == "hit_inflight"
	if !joinerHit {
		t.Fatalf("S-079 joiner row gateway_cache_status=%q (want hit/hit_inflight) — "+
			"upstream ids matched but audit row stamped as MISS. Hardened scenario requires "+
			"the gateway cache path actually exercised: the audit pipeline must stamp the "+
			"joiner row with cache_status=hit/hit_inflight to expose the double-count regression "+
			"surface (proxy_cache.go joiner stamping).", joiner.gatewayCacheStatus)
	}

	if joiner.cacheReadSavingsUsd != 0 {
		t.Errorf("joiner cache_read_savings_usd=%.8f (want 0) — the 2026-05 double-count "+
			"regression: joiner is carrying provider prompt-cache savings the leader "+
			"already accounted for (proxy_cache.go:718,919 fix did not clear it)",
			joiner.cacheReadSavingsUsd)
	}
	if joiner.cacheWriteCostUsd != 0 {
		t.Errorf("joiner cache_write_cost_usd=%.8f (want 0) — joiner did not call upstream; "+
			"cache write cost belongs to the leader row only",
			joiner.cacheWriteCostUsd)
	}
	if joiner.cacheNetSavingsUsd != 0 {
		t.Errorf("joiner cache_net_savings_usd=%.8f (want 0) — derived from cleared "+
			"read_savings - write_cost; non-zero means the clear didn't fire",
			joiner.cacheNetSavingsUsd)
	}
	if joiner.cacheCreationTokens != 0 {
		t.Errorf("joiner cache_creation_tokens=%d (want 0) — joiner never wrote to cache",
			joiner.cacheCreationTokens)
	}
	if joiner.cacheReadTokens != 0 {
		t.Errorf("joiner cache_read_tokens=%d (want 0) — joiner never read from provider cache",
			joiner.cacheReadTokens)
	}
	if joiner.gatewayCacheSavingsUsd <= 0 {
		t.Errorf("joiner gateway_cache_savings_usd=%.8f (want > 0) — HIT joiner must "+
			"stamp the avoided full upstream cost into gateway_cache_savings_usd",
			joiner.gatewayCacheSavingsUsd)
	}
	// 2026-05-21 cost-stamping rule (proxy_cache.go:299-316): on HIT
	// EstimatedCostUsd carries the would-have-paid spend and equals
	// GatewayCacheSavingsUsd. The customer's actual spend is computed
	// downstream as `EstimatedCostUsd − GatewayCacheSavingsUsd` and
	// resolves to 0 on a full HIT. The double-count regression is about
	// CACHE_READ_SAVINGS, not about whether EstimatedCostUsd is zero —
	// that field is now the "predicted spend at current prices" KPI.
	if absFloat(joiner.estimatedCostUsd-joiner.gatewayCacheSavingsUsd) > 1e-8 {
		t.Errorf("joiner estimated_cost_usd=%.8f gateway_cache_savings_usd=%.8f "+
			"(want equal — HIT joiner stamps would-have-paid into both so net spend = 0)",
			joiner.estimatedCostUsd, joiner.gatewayCacheSavingsUsd)
	}

	// ─── Cross-row independence check ───────────────────────────────────
	// SUM of provider-cache savings across the two rows must equal the
	// leader's contribution alone — that's the 1× canonical value, not
	// 2×. Captures any future regression that re-introduces the joiner
	// stamp without us having to know the exact dollar amount upfront.
	sumReadSavings := leader.cacheReadSavingsUsd + joiner.cacheReadSavingsUsd
	if absFloat(sumReadSavings-leader.cacheReadSavingsUsd) > 1e-8 {
		t.Errorf("cross-row cache_read_savings_usd double-count: leader=%.8f joiner=%.8f sum=%.8f "+
			"(want sum == leader, i.e. joiner contribution == 0)",
			leader.cacheReadSavingsUsd, joiner.cacheReadSavingsUsd, sumReadSavings)
	}

	// ─── Provider prompt-cache write proof on the leader ────────────────
	// With explicit cache_control + a >1024-token system prefix the
	// upstream Anthropic call MUST report cache_creation_input_tokens > 0
	// the first time it sees that prefix. That's our load-bearing proof
	// that the bug surface (leader writing to provider cache, joiner
	// inheriting the cleared columns) was actually touched on this run,
	// not just the gateway-cache layer.
	//
	// Skip (rather than fail) if cache_creation_tokens is 0 — the prefix
	// may have been cached on a sibling run within the 5-minute TTL,
	// turning the leader into a provider-cache HIT (cache_read_tokens > 0
	// instead of cache_creation_tokens). We accept either as proof the
	// provider cache surface was touched.
	leaderTouchedProviderCache := leader.cacheCreationTokens > 0 || leader.cacheReadTokens > 0
	if !leaderTouchedProviderCache {
		t.Fatalf("S-079 leader did not engage Anthropic prompt cache "+
			"(cache_creation_tokens=%d cache_read_tokens=%d). "+
			"Hardened scenario requires provider prompt cache engaged on the leader: either "+
			"cache_control survives canonical bridge intact, the long system prefix is above "+
			"the 1024-token minimum, AND the model supports prompt cache. Without provider "+
			"cache engagement the regression surface (joiner mirroring leader savings) is "+
			"unreachable — the scenario would not catch a future re-introduction of the bug.",
			leader.cacheCreationTokens, leader.cacheReadTokens)
	}
	// On a fresh prefix the leader writes (cache_creation_tokens > 0); on
	// a warm prefix it reads (cache_read_tokens > 0). The double-count
	// regression was about the JOINER mirroring whichever the leader had,
	// so the joiner must still be zero on both columns — already asserted
	// above.

	// ─── Independent canonical calc (leader only) ───────────────────────
	// When the leader did read from provider prompt cache (Anthropic
	// returned cache_read_input_tokens > 0), we can independently recompute
	// the canonical savings from Model pricing and assert the leader row
	// matches. If the leader's cache_read_tokens is 0 — provider
	// prompt-cache wasn't engaged on the upstream call — we log and skip
	// this half. The cross-row invariant above still ran.
	if leader.cacheReadTokens > 0 && cacheReadPricePerM != nil {
		const million = 1_000_000.0
		canonical := float64(leader.cacheReadTokens) * (inputPricePerM - *cacheReadPricePerM) / million
		// Allow for NUMERIC(12,8) rounding on either side.
		if absFloat(leader.cacheReadSavingsUsd-canonical) > 1e-6 {
			t.Errorf("leader cache_read_savings_usd=%.8f vs canonical %.8f "+
				"(cache_read_tokens=%d × (input_price=%.6f - cache_read_price=%.6f) / 1e6) "+
				"— leader stamp drifted from Model pricing",
				leader.cacheReadSavingsUsd, canonical,
				leader.cacheReadTokens, inputPricePerM, *cacheReadPricePerM)
		}
	}
	// Else branch (leader.cacheCreationTokens > 0 fired): the leader wrote
	// to provider cache rather than reading. The cross-row invariant
	// (joiner.cache_read_savings_usd == 0) above is the load-bearing
	// regression guard for that arm.

	// Joiner row must NOT mirror leader's provider_cache_status. Pre-fix,
	// the joiner stamped the same upstream usage envelope the leader had,
	// so provider_cache_status='hit' leaked onto the joiner row. Post-fix
	// the joiner never inspects provider usage so the column should be
	// empty / NA. Hardened scenario fails the run if this pre-fix leak
	// fingerprint reappears, even though cost columns are already covered
	// by the zero-assertions above.
	if strings.EqualFold(joiner.providerCacheStatus, "hit") {
		t.Errorf("joiner provider_cache_status=%q — pre-fix leak fingerprint reappeared: "+
			"joiner row is mirroring leader's provider_cache_status. Cost columns are zeroed "+
			"(asserted above) but this status leak still indicates the joiner stamp path is "+
			"reading upstream usage when it should not (proxy_cache.go joiner stamp).",
			joiner.providerCacheStatus)
	}

	t.Logf("S-079 OK: leader id=%s status=%s read_savings=%.8f cache_read_tok=%d cache_creation_tok=%d | "+
		"joiner id=%s status=%s read_savings=0 cache_read_tok=0 cache_creation_tok=0 gateway_savings=%.8f",
		leader.id, leader.gatewayCacheStatus, leader.cacheReadSavingsUsd,
		leader.cacheReadTokens, leader.cacheCreationTokens,
		joiner.id, joiner.gatewayCacheStatus, joiner.gatewayCacheSavingsUsd)
}
