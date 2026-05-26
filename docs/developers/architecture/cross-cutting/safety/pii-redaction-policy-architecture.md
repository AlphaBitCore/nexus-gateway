# PII redaction policy architecture

PII redaction in the Nexus Gateway is a **two-axis policy**: every match by a content-touching hook can independently change the **in-flight** body (the bytes Nexus forwards upstream) and the **storage** body (the bytes the audit pipeline persists to `traffic_event_normalized`). The two axes are encoded as `onMatch.inflightAction` and `onMatch.storageAction` on every hook config; pipeline aggregation picks the strictest storage policy across all matched hooks, and the proxy stamps a small closed set of standard `ReasonCode` values onto the audit row whenever the storage axis diverged from the inflight axis, or when an adapter could not honour an inflight redact.

Detection produces byte-addressed `TransformSpan` values against the canonical (post-normalize) payload. The same span set drives **both** rewrites: the adapter's `RewriteRequestBody` / `RewriteResponseBody` applies it on the wire, and the audit writer's `applyStorageAction` applies it on the persisted bytes. Spans address `messages.<i>.content.<j>` (chat), `messages.<i>.content.<j>.toolResult` (tool output), or `inputs.<i>` (embeddings), so the redaction set is wire-shape-independent.

Anchor packages:

- `packages/shared/policy/decision/` — `Decision`, `HookResult`, `CompliancePipelineResult`, `InflightAction`, `StorageAction`, and the four standard `Reason*` constants.
- `packages/shared/policy/hooks/core/onmatch.go` — `ParseOnMatch`, `DecisionForInflight`, `StrictestStorageAction`, `ResolveReplacement`.
- `packages/shared/policy/hooks/validators/pii_detector.go` — the `pii-detector` built-in: regex + Luhn detection, span emission, replacement template.
- `packages/shared/policy/hooks/builtins/builtins.go` — registry that wires `pii-detector` and the related content-touching hooks (`keyword-filter`, `content-safety`, `rulepack-engine`, `webhook-forward`).
- `packages/shared/policy/pipeline/pipeline.go` — pipeline aggregator that unions per-hook spans and reduces per-hook `StorageAction` to the strictest value.
- `packages/shared/transport/normalize/core/types.go` — `NormalizedPayload`, `TransformSpan`, `TransformSource`, `TransformAction`.
- `packages/shared/transport/normalize/core/apply_spans.go` — `ApplySpans` engine that walks the canonical payload and rewrites the addressed content blocks.
- `packages/shared/traffic/adapter.go` + `packages/shared/traffic/types.go` — adapter `RewriteRequestBody` / `RewriteResponseBody` contract and `ErrRewriteUnsupported` sentinel.
- `packages/ai-gateway/internal/platform/audit/audit.go` — `applyStorageAction` and the `Record` fields that ferry spans + storage policy from the hook pipeline to the audit writer.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — `Reason*` stamping, MODIFY decision dispatch, ErrRewriteUnsupported handling.
- `packages/ai-gateway/internal/policy/aiguard/types.go` — `aiguard.Redaction` (LLM-as-judge suggested span), the AI-Guard analogue of a hook-emitted span.

## 1. The two-axis policy

Every content-touching hook (pii-detector, keyword-filter, content-safety, rulepack-engine, quality-checker, webhook-forward) reads the same declarative `onMatch` block from its config:

```json
{
  "onMatch": {
    "inflightAction": "approve" | "block-hard" | "block-soft" | "redact",
    "storageAction":  "keep" | "redact" | "drop-content",
    "replacement":    "[REDACTED_<RULE_ID>]"
  }
}
```

`ParseOnMatch` validates the closed string sets and applies the compliance-default fallback (`block-hard` inflight + `redact` storage + `[REDACTED_<RULE_ID>]` template). The defaults are deliberately conservative: a config that omits the `onMatch` block still blocks on match inflight and redacts in the audit log, so no operator can accidentally persist sensitive bytes without explicitly choosing `keep`.

`DecisionForInflight` maps the inflight axis into the hook pipeline's `Decision` vocabulary on a match: `approve → Approve`, `block-hard → RejectHard`, `block-soft → BlockSoft`, `redact → Modify`. The storage axis is orthogonal — it travels separately on `HookResult.StorageAction` and is reduced across hooks by `StrictestStorageAction` (ordering: `drop-content > redact > keep > ""`).

## 2. Detection — the pii-detector hook

`pii-detector` is the canonical PII detection built-in. Its config is a list of `patternDefinitions` — each with `id`, `regex`, optional JavaScript-style `flags` (`g` collapsed; `i`, `m`, `s` honoured), optional `luhn` for credit-card patterns, and an optional per-pattern `replacement` that overrides the `onMatch.replacement` template for that pattern's hits.

The hook is registered in `builtins.Registry` alongside the other content-touching hooks. When `_rulePackInstalls` is present on the config, the factory delegates to the `rulepack-engine` so admin-managed rule packs flow through the same matcher.

Detection runs against the **canonical projection** of the input — text segments addressed against the `NormalizedPayload`, walked via the embedded `TextOnlyContentScanning` helper. For each match the hook may take either of two paths:

- **Reject path** (`executeReject`) — used when `onMatch.inflightAction` is `block-hard` / `block-soft` / `approve`. Short-circuits on the first match, sets `Decision = DecisionForInflight(InflightAction)`, `Reason = "PII detected: <id>"`, `ReasonCode = PII_DETECTED`, tags `compliance:pii` + `severity:confidential`, returns.

- **Redact path** (`executeRedact`) — used when `onMatch.inflightAction = "redact"`. Walks the projection, collects per-pattern match offsets in the **original** segment text, applies replacements in descending start-offset order so successive rewrites do not shift earlier offsets, emits one `TransformSpan` per match, and stamps `Decision = Modify`, `ReasonCode = PII_REDACTED`. Spans carry `Source = SourceHook`, `SourceID = pattern.id`, `Action = ActionRedact`, `ContentAddress` matching the projection slot (`messages.<i>.content.<j>` for chat text, `messages.<i>.content.<j>.toolResult` for tool-result output, `inputs.<i>` for `KindAIEmbedding` request payloads), and the resolved `Replacement` string.

The Luhn validator runs as a per-pattern filter — matches whose digits do not pass the Luhn checksum are dropped silently so a 16-digit number that is *not* a card number does not get redacted.

## 3. Span shape and the canonical address space

`TransformSpan` is the on-wire representation of one byte-level modification:

```go
type TransformSpan struct {
    Source         TransformSource // "hook" | "aiguard" | "cache-normaliser" | "cache-control-inject" | "cache-key-strip"
    SourceID       string          // rule ID, hook ID, normaliser rule ID
    Action         TransformAction // "redact" | "strip" | "inject" | "replace"
    ContentAddress string          // "messages.0.content.1" | "inputs.0" | "http.bodyView" | "http.bodyView.form.<key>"
    Start, End     int             // UTF-8 byte offsets into the addressed content's text
    Replacement    string
    Reason         string
}
```

A single span set serves both the in-flight rewrite (adapter applies it against the wire-shape body) and the storage rewrite (audit writer applies it against the canonical payload). The redact / strip / replace actions overwrite the `[Start, End)` byte range with `Replacement`; `inject` carries `Start == End` and inserts.

`ApplySpans` is the single rewrite engine — it groups spans by `ContentAddress`, sorts each group by descending start, walks the addressed content blocks in the canonical `NormalizedPayload`, and returns a fresh payload plus the list of spans that did not resolve. Unresolvable spans (e.g. addresses outside the payload's structure) are recorded so callers can surface them.

`RedactionSpan` is a backward-compatibility type alias for `TransformSpan` — narrow hook-result APIs may still mention it, but every producer and consumer in the live pipeline uses the full `TransformSpan` shape so non-redact sources (cache-normaliser strips, cache-control inject) flow through the same audit channel.

## 4. Decision precedence — admin policy first, AI-Guard suggestion second

The Nexus model is **admin policy is authoritative**; AI-driven detection (the AI-Guard judge) acts as a *suggestion* layer whose effect is bounded by what admin policy allows. The reconcile is wired inside the shared `webhook-forward` hook (`packages/shared/policy/hooks/webhook/webhook.go`): after parsing the webhook reply into a `Decision`, `WebhookForward.Execute` computes `core.StrictestDecision(suggested, core.DecisionForInflight(onMatch.InflightAction))` and adopts the stricter of the two. When the reconciled decision differs from the suggestion, the hook stamps `ReasonCode = ReasonAIGuardSuggestedVsPolicy` and rewrites `Reason` to `"webhook suggested X; policy ceiling: Y"` using `core.LabelForDecision` for both halves so the audit row reads in the same `InflightAction` vocabulary the operator wrote in the hook config.

The strictness ordering (`RejectHard > BlockSoft > Modify > Approve > Abstain`) matches the pipeline aggregator's `mergeResults` precedence, so reconcile and aggregation agree on relative strictness. Three behaviours emerge naturally:

- A hook whose `onMatch.inflightAction = "block-hard"` overrides any softer AI-Guard suggestion — the request rejects with 403 and the audit row carries `request_hook_reason_code = AIGUARD_SUGGESTED_VS_POLICY` with `Reason` carrying both values so operators can see the AI-Guard verdict the admin policy overrode.
- A hook whose `onMatch.inflightAction = "redact"` accepts an AI-Guard suggestion of `approve` as the redact ceiling (decision becomes `Modify`); the AI-Guard span set rides through on `HookResult.TransformSpans` and the strictest storage action across hook + AI-Guard still wins.
- A hook whose `onMatch.inflightAction = "approve"` passes the webhook's suggestion through verbatim when the webhook is stricter than approve, and short-circuits cleanly when the webhook returns `Abstain` (the per-hook decision stays `Abstain` so the pipeline aggregator can skip the hook without inheriting a manufactured opinion).

`webhook-forward` overrides the `ParseOnMatch` block-hard default to `approve` when the admin did not configure an explicit `onMatch.inflightAction` — the webhook's reply IS the decision, so a missing ceiling means "advisory mode" rather than "block by default". Match-only hooks (pii-detector, keyword-filter, content-safety) keep the block-hard default because for them the match itself is the decision.

The four standard `ReasonCode` constants record the divergences operators care about:

| ReasonCode | Meaning |
|---|---|
| `REDACT_INFLIGHT_UNSUPPORTED` | Hook asked for in-flight redact but the adapter returned `ErrRewriteUnsupported` — upstream received the original body, audit-side redact still applied per storage policy. |
| `REDACT_STORAGE_ONLY_BY_POLICY` | Admin chose `storageAction = "redact"` with `inflightAction = "approve"` — upstream received the original body; the audit row is redacted. |
| `STORAGE_DROPPED_BY_POLICY` | Admin chose `storageAction = "drop-content"` — the persisted body is replaced with a placeholder envelope; only the decision + metadata remain. |
| `AIGUARD_SUGGESTED_VS_POLICY` | Admin policy ceiling overrode AI-Guard's suggestion — both values carried in `Reason` for audit forensics. |

The codes are stamped onto `Record.HookReasonCode` (request stage) / `Record.ResponseHookReasonCode` (response stage) at the proxy boundary; both UI locales (`pages.json`) carry user-facing strings for the closed set.

## 5. In-flight rewrite — the Modify decision path

When the aggregated `CompliancePipelineResult.Decision = Modify` and `len(ModifiedContent) > 0`, the request handler calls `trafficAdapter.RewriteRequestBody(ctx, body, path, contentBlocksToNormalized(hookResult.ModifiedContent))`. Three outcomes:

1. **Success** — the adapter returns the rewritten bytes plus a count of overwritten slots. The proxy stamps `rec.HookRewriteCount` + `rec.HookRewritten = true` and forwards the rewritten body upstream.
2. **`ErrRewriteUnsupported`** — the adapter cannot reverse-encode its wire format (e.g. a passthrough wire shape Nexus does not own). The proxy emits a warn log and **forwards the original body unchanged** while stamping `rec.HookReasonCode = ReasonRedactInflightUnsupported` on the audit row. Storage-side rewrite still runs from the same span set, so the audit log remains redacted even when the upstream copy carries unredacted bytes.
3. **Any other error** — internal inconsistency (the body passed `ExtractRequest` but failed to round-trip). Surfaces as `500 request rewrite failed`.

The same three-arm pattern runs on the response side via `extractor.RewriteResponseBody` for non-streaming responses, cache-hit replays, and the streaming held-back SSE prefix.

Adapters MUST walk their schema in the same order as `ExtractRequest` / `ExtractResponse` emitted segments, so position `i` in `content.Segments` pairs with the i-th extractable slot — this is the canonical adapter contract documented on `Adapter.RewriteRequestBody`.

## 6. Storage-only rewrite — `applyStorageAction`

The audit writer's `recordToMessage` builds the normalized payload via the registered normalizer, then calls `applyStorageAction(raw, action, spans, ruleIDs)` to fold the storage policy into the persisted bytes:

```
applyStorageAction(raw, action, spans, ruleIDs) →
  ""/keep        → raw unchanged
  "redact"       → ApplySpans(unmarshal(raw), spans) → marshal
  "drop-content" → NormalizedPayload{Kind, NormalizeVersion, Protocol, Redacted:true, RuleIDs}
```

The `keep` arm preserves the original bytes — admin opt-in for environments where the audit log itself is the compliance boundary. The `redact` arm runs the same `ApplySpans` engine that drives in-flight rewrite, so the storage-side bytes match the inflight bytes byte-for-byte when both policies are active. The `drop-content` arm replaces the payload with a small envelope that preserves the `Kind` discriminator, the `NormalizeVersion`, the `Protocol`, sets `Redacted = true`, and lists the matching rule IDs — operators can still see *that* the policy dropped the body and *which rules* matched, just not the content itself.

If unmarshal / marshal fails the function falls back to the original bytes so the audit row still carries the normalized snapshot. Storage policy is observability discipline, not a runtime gate — a corrupted span set must not cause an audit row to vanish.

The audit `Record` carries three field pairs that ferry the pipeline result to the writer:

- `RequestStorageAction` / `ResponseStorageAction` (string form of `StorageAction`)
- `RequestTransformSpans` / `ResponseTransformSpans` (`[]TransformSpan`)
- `RequestRedactRuleIDs` / `ResponseRedactRuleIDs` (the union of rule IDs that produced redact spans)

The proxy stamps these from the pipeline result at the request boundary (request stage) and response hook boundary (response / streaming / cache-hit stages); `applyStorageAction` reads them inside `recordToMessage`.

## 7. Pipeline aggregation across multiple hooks

`pipeline.Execute` accumulates spans **from every hook that ran**, regardless of terminal decision — even Approve hooks may emit informational transforms (e.g. cache-normaliser strips wrapped as a hook integration). The aggregator:

- Appends `r.TransformSpans` to the union `allSpans` for every hook.
- Reduces per-hook `r.StorageAction` to the pipeline `storage` via `StrictestStorageAction` (drop-content beats redact beats keep).
- On `RejectHard`, returns immediately with the reject reason + the spans accumulated up to that point so the audit row sees what the rejected request *would have* redacted.
- On `Modify`, retains the last hook's `ModifiedContent` alongside the union `TransformSpans` — adapters that consume the span-driven rewrite read `TransformSpans` while the `ModifiedContent` slice serves narrow `RewriteRequestBody` callers that take a `NormalizedContent` argument.
- On `Approve`, optionally clears soft-reject accumulators when `clearSoftOnApprove` is set on the pipeline.

The final `CompliancePipelineResult` carries `TransformSpans` (the union) and `StorageAction` (the strictest) — the two fields that drive in-flight rewrite and storage-side rewrite respectively.

## 8. Cross-format consideration

Spans address the **canonical** post-normalize body (`messages.<i>.content.<j>`, `inputs.<i>`, `http.bodyView`), not the wire-shape body. This is what makes redaction wire-agnostic:

- A request that arrives as OpenAI Chat Completions, Anthropic Messages, Gemini GenerateContent, or the OpenAI Responses API all canonicalize via `IngressChatToCanonical` before the hook pipeline runs. The pii-detector sees the same canonical text and emits the same `messages.0.content.0` span set.
- The adapter's `RewriteRequestBody` translates the canonical span set back into the wire-specific schema — `RewriteRequestBody` is the inverse of `ExtractRequest` for that adapter, slot-for-slot.
- The audit writer's `applyStorageAction` applies the same spans against the canonical payload that lives in `traffic_event_normalized`.

The wire formats can differ in how many slots an adapter exposes — `ExtractRequest` for an embeddings request returns `inputs[i]` slots, whereas a chat request returns `messages[i].content[j]` slots — and the span emitter follows the same shape (`KindAIEmbedding` branch in `executeRedact` emits `inputs.<i>` addresses; chat branch emits `messages.<i>.content.<j>` / `…toolResult`).

The webhook-forward hook exists for the special case of *external* compliance webhooks that return a flat-offset redaction list against a flat joined projection. Those redactions arrive as `aiguard.Redaction`-shaped wire records and are decoded into `TransformSpan` with `ContentAddress = "webhook.flat"` — a sentinel address that `ApplySpans` does **not** resolve. The webhook spans therefore land in the audit row for forensic completeness but do *not* mutate the in-flight body; inflight redaction of AI-Guard-style suggestions requires the internal `aiguard-classify` path inside ai-gateway, which produces canonical `SourceAIGuard` spans against the addressed payload structure.

## 9. Audit annotations

The audit row stamps the following PII-related fields:

- `Record.HookDecision` / `Record.ResponseHookDecision` — terminal hook pipeline decision (`Approve` / `RejectHard` / `BlockSoft` / `Modify`).
- `Record.HookReason` / `Record.ResponseHookReason` — human-readable reason string (e.g. `"PII detected: email"` or `"PII redacted"`).
- `Record.HookReasonCode` / `Record.ResponseHookReasonCode` — closed-set machine code, set by the hook itself (`PII_DETECTED`, `PII_REDACTED`) and overridden by the proxy with one of the four `Reason*` constants when the storage axis or adapter capability diverged from the inflight intent.
- `Record.HookRewriteCount` / `Record.HookRewritten` — how many content slots the adapter actually overwrote and a boolean rewrite-applied flag.
- `Record.ComplianceTags` — union of per-hook tags (`compliance:pii`, `severity:confidential` from pii-detector; other tags from sibling hooks).
- `Record.BlockingRule` — rule-pack attribution (pack, version, rule ID) when the decision came from a rule-pack engine.
- `Record.HooksPipeline` — full ordered hook-execution trace via `HookExecRecord` (per-hook JSON fields: `stage` / `order` / `hookId` / `name` / `decision` / `reason` / `reasonCode` / `latencyMs` / `error`; `name`, `reason`, `reasonCode`, and `error` marshal omitted when empty).
- `Record.Request*` / `Record.Response*` storage triple — `StorageAction`, `TransformSpans`, `RedactRuleIDs`.

The control-plane UI's traffic-audit drawer reads `HookReasonCode` and surfaces a chip for the closed-set codes with locale-translated explanatory text (English / Spanish / Simplified Chinese bundles all carry the strings for `REDACT_STORAGE_ONLY_BY_POLICY`, `STORAGE_DROPPED_BY_POLICY`, `REDACT_INFLIGHT_UNSUPPORTED`, `AIGUARD_SUGGESTED_VS_POLICY`).

## 10. Observability — metrics

The hook pipeline exports a single counter for redaction outcomes:

- **`nexus_hook_pipeline_total{ingress_format, stage, decision}`** — `decision` is the lowercase form of `Decision` (`approve` / `modify` / `block_soft` / `reject_hard` / `error` / `skipped`); `stage` is `request` / `response`; `ingress_format` is the wire-format label. Incremented once per hook pipeline execution at both request and response boundaries. Empty labels are stamped as `unknown` so cardinality is bounded. Registered as `hook.pipeline_total` against the opsmetrics registry, which prepends the `nexus_` namespace at scrape time.

The counter intentionally does not break out *which* `Reason*` code fired — that fidelity lives on the audit row (`HookReasonCode`) where forensic queries pull it. Aggregating the closed-set reason codes at metric time would force a high-cardinality label set on a counter that observers already poll at one-minute resolution; the audit-row query is the right place for those slices.

## References

- `packages/shared/policy/decision/types.go`
- `packages/shared/policy/hooks/core/onmatch.go`
- `packages/shared/policy/hooks/core/types.go`
- `packages/shared/policy/hooks/validators/pii_detector.go`
- `packages/shared/policy/hooks/builtins/builtins.go`
- `packages/shared/policy/hooks/webhook/webhook.go`
- `packages/shared/policy/pipeline/pipeline.go`
- `packages/shared/transport/normalize/core/types.go`
- `packages/shared/transport/normalize/core/apply_spans.go`
- `packages/shared/traffic/adapter.go`
- `packages/shared/traffic/types.go`
- `packages/ai-gateway/internal/platform/audit/audit.go`
- `packages/ai-gateway/internal/ingress/proxy/proxy.go`
- `packages/ai-gateway/internal/ingress/proxy/proxy_cache.go`
- `packages/ai-gateway/internal/ingress/proxy/classify/classify.go`
- `packages/ai-gateway/internal/policy/aiguard/types.go`
- `packages/ai-gateway/internal/platform/metrics/metrics.go`
- `packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx`
- `packages/control-plane-ui/public/locales/en/pages.json`
- `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md`
