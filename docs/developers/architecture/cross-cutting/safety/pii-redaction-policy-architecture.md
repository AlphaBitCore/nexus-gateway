# PII redaction policy architecture

PII redaction in the Nexus Gateway is driven by a **single match action**: every match by a content-touching hook resolves to one of `approve` / `redact` / `block`, encoded as `onMatch.action` on the hook config. `redact` and `block` both mask the matched content; `approve` leaves it untouched. The action governs the in-flight body (the bytes Nexus forwards upstream and returns to the client) and what the audit pipeline persists in one decision — there is no separate "storage" axis. Pipeline aggregation picks the strictest action across all matched hooks, and the proxy stamps a small closed set of standard `ReasonCode` values onto the audit row when an adapter could not honour an in-flight redact.

The redacted bytes Nexus persists are the **raw wire copy** the adapter already rewrote in flight — not a separately recomputed normalized projection. The canonical normalized projection (`traffic_event_normalized.request_normalized` / `response_normalized`) is **never written at audit time**; the control plane recomputes it at view time from the stored raw body, which is itself the action-governed (already-redacted under `redact` / `block`) copy, so the recompute reads PII-safe bytes. See [audit-pipeline-architecture.md](../observability/audit-pipeline-architecture.md) for the write/view split.

Detection produces byte-addressed `TransformSpan` values against the canonical (post-normalize) payload. That span set drives the in-flight rewrite: the adapter's `RewriteRequestBody` / `RewriteResponseBody` applies it on the wire, and the rewritten wire bytes are what the audit store keeps. Spans address `messages.<i>.content.<j>` (chat), `messages.<i>.content.<j>.toolResult` (tool output), or `inputs.<i>` (embeddings), so the redaction set is wire-shape-independent.

Anchor packages:

- `packages/shared/policy/decision/` — `Decision`, `HookResult`, `CompliancePipelineResult`, `Action`, `ActionFromLegacy`, and the standard `Reason*` constants.
- `packages/shared/policy/hooks/core/onmatch.go` — `ParseOnMatch`, `DecisionForAction`, `ActionFromDecision`, `StrictestAction`, `ResolveReplacement`.
- `packages/shared/policy/hooks/validators/pii_detector.go` — the `pii-detector` built-in: regex + Luhn detection, span emission, replacement template.
- `packages/shared/policy/hooks/builtins/builtins.go` — registry that wires `pii-detector` and the related content-touching hooks (`keyword-filter`, `content-safety`, `rulepack-engine`, `webhook-forward`).
- `packages/shared/policy/pipeline/pipeline.go` — pipeline aggregator that unions per-hook spans and reduces per-hook actions to the strictest value.
- `packages/shared/transport/normalize/core/types.go` — `NormalizedPayload`, `TransformSpan`, `TransformSource`, `TransformAction`.
- `packages/shared/transport/normalize/core/apply_spans.go` — `ApplySpans` engine that walks the canonical payload and rewrites the addressed content blocks (used for the in-flight rewrite).
- `packages/shared/traffic/adapter.go` + `packages/shared/traffic/types.go` — adapter `RewriteRequestBody` / `RewriteResponseBody` contract and `ErrRewriteUnsupported` sentinel.
- `packages/shared/traffic/redact/redact.go` — `StorageRawBody` (raw-copy governance), `MarshalSpans`, `CollectRuleIDs` — the storage-policy helpers every audit producer calls.
- `packages/ai-gateway/internal/platform/audit/record.go` — the `Record` fields that ferry the per-stage action + redacted wire copy from the hook pipeline to the AI Gateway audit writer.
- `packages/shared/policy/pipeline/audit_emitter.go` — `AuditEmitter.buildEvent`, the single choke point where compliance-proxy and agent rows have the action applied to the raw captured copy before any writer sees the event.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — `Reason*` stamping, MODIFY decision dispatch, ErrRewriteUnsupported handling.
- `packages/ai-gateway/internal/policy/aiguard/types.go` — `aiguard.Redaction` (LLM-as-judge suggested span), the AI-Guard analogue of a hook-emitted span.

## 1. The single-action policy

Every content-touching hook (pii-detector, keyword-filter, content-safety, rulepack-engine, quality-checker, webhook-forward) reads the same declarative `onMatch` block from its config:

```json
{
  "onMatch": {
    "action":      "approve" | "redact" | "block",
    "replacement": "[REDACTED_<RULE_ID>]"
  }
}
```

The action means:

- **`approve`** — forward / return the body unchanged; persist the captured bytes as-is.
- **`redact`** — rewrite the payload, masking matched spans inline; the *same* masked wire body is forwarded upstream, returned to the client, and persisted.
- **`block`** — reject the transaction (403 at a proxy that owns the client response, a minimal Forbidden on the agent's on-host interceptor); persist the redacted copy so an auditor can review what tripped the policy.

`redact` and `block` share the same payload-redaction operation and differ only in disposition (rewrite-and-forward vs reject). Storage therefore has only two states — as-is (`approve`) or redacted (`redact` / `block`).

`ParseOnMatch` validates the closed string set and applies the compliance-default fallback (`action = block` + `[REDACTED_<RULE_ID>]` template). The default is deliberately conservative: a config that omits the `onMatch` block still rejects on match and persists a redacted copy, so no operator can accidentally forward or persist sensitive bytes without explicitly choosing `approve`.

`DecisionForAction` maps the action onto the hook pipeline's internal `Decision` vocabulary on a match: `approve → Approve`, `redact → Modify`, `block → RejectHard`. `ActionFromDecision` is the inverse used at the pipeline boundary, and `StrictestAction` aggregates per-hook actions (ordering: `block > redact > approve > ""`).

**Backward-compatibility window.** The prior two-key shape (`inflightAction` + `storageAction`) is also accepted when `action` is absent: `ActionFromLegacy` folds the pair to one action (`block-hard`/`block-soft → block`; `redact → redact`; `approve + keep → approve`; `approve + redact`/`drop-content → redact`) and logs a one-shot warning pointing the operator at the single `action`. A one-time data migration rewrites stored rows; the schema drops the two-key form once the window closes. `drop-content` is not an operator choice — it survives only as the internal safety fallback below (a redacting match that resolves no spans cannot mask and must not persist sensitive bytes).

## 2. Detection — the pii-detector hook

`pii-detector` is the canonical PII detection built-in. Its config is a list of `patternDefinitions` — each with `id`, `regex`, optional JavaScript-style `flags` (`g` collapsed; `i`, `m`, `s` honoured), optional `luhn` for credit-card patterns, and an optional per-pattern `replacement` that overrides the `onMatch.replacement` template for that pattern's hits.

The hook is registered in `builtins.Registry` alongside the other content-touching hooks. When `_rulePackInstalls` is present on the config, the factory delegates to the `rulepack-engine` so admin-managed rule packs flow through the same matcher.

Detection runs against the **canonical projection** of the input — text segments addressed against the `NormalizedPayload`, walked via the embedded `TextOnlyContentScanning` helper. `Execute` dispatches on the single `onMatch.action` into one of three paths:

- **Approve path** (`executeApprove`) — used when `onMatch.action = "approve"`. Short-circuits on the first match, sets `Reason = "PII detected: <id>"`, `ReasonCode = PII_DETECTED`, tags `compliance:pii` + `severity:confidential`, leaves the decision at `Approve` and the payload untouched (detect-for-tagging-only). No spans are collected.

- **Block path** (`executeBlock`) — used when `onMatch.action = "block"` (also the default). Collects the redaction spans, and on a match stamps `Decision = RejectHard`, `Action = ActionBlock`, `ReasonCode = PII_DETECTED`, and the same `compliance:pii` + `severity:confidential` tags. The request is rejected, never forwarded; the spans drive the redacted copy persisted to the audit store so an auditor can see what tripped the policy without the raw bytes.

- **Redact path** (`executeRedact`) — used when `onMatch.action = "redact"`. Walks the projection, collects per-pattern match offsets in the **original** segment text, applies replacements in descending start-offset order so successive rewrites do not shift earlier offsets, emits one `TransformSpan` per match, and stamps `Decision = Modify`, `Action = ActionRedact`, `ReasonCode = PII_REDACTED`. The same masked body is forwarded upstream, returned to the client, and persisted. Spans carry `Source = SourceHook`, `SourceID = pattern.id`, `Action = ActionRedact`, `ContentAddress` matching the projection slot (`messages.<i>.content.<j>` for chat text, `messages.<i>.content.<j>.toolResult` for tool-result output, `inputs.<i>` for `KindAIEmbedding` request payloads), and the resolved `Replacement` string.

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

The Nexus model is **admin policy is authoritative**; AI-driven detection (the AI-Guard judge) acts as a *suggestion* layer whose effect is bounded by what admin policy allows. The reconcile is wired inside the shared `webhook-forward` hook (`packages/shared/policy/hooks/webhook/webhook.go`): after parsing the webhook reply into a `Decision`, `WebhookForward.Execute` computes `core.StrictestDecision(suggested, core.DecisionForAction(onMatch.Action))` and adopts the stricter of the two. When the reconciled decision differs from the suggestion, the hook stamps `ReasonCode = ReasonAIGuardSuggestedVsPolicy` and rewrites `Reason` so both halves read in the single-action vocabulary the operator wrote in the hook config (`ActionFromDecision(suggested)` for the webhook side, `onMatch.Action` for the policy ceiling).

The strictness ordering (`RejectHard > BlockSoft > Modify > Approve > Abstain`) matches the pipeline aggregator's `mergeResults` precedence, so reconcile and aggregation agree on relative strictness. Three behaviours emerge naturally:

- A hook whose `onMatch.action = "block"` overrides any softer AI-Guard suggestion — the request rejects with 403 and the audit row carries `request_hook_reason_code = AIGUARD_SUGGESTED_VS_POLICY` with `Reason` carrying both values so operators can see the AI-Guard verdict the admin policy overrode.
- A hook whose `onMatch.action = "redact"` accepts an AI-Guard suggestion of `approve` as the redact ceiling (decision becomes `Modify`); the AI-Guard span set rides through on `HookResult.TransformSpans` and the strictest action across hook + AI-Guard still wins.
- A hook whose `onMatch.action = "approve"` passes the webhook's suggestion through verbatim when the webhook is stricter than approve, and short-circuits cleanly when the webhook returns `Abstain` (the per-hook decision stays `Abstain` so the pipeline aggregator can skip the hook without inheriting a manufactured opinion).

`webhook-forward` overrides the `ParseOnMatch` block default to `approve` when the admin did not configure an explicit `onMatch.action` — the webhook's reply IS the decision, so a missing ceiling means "advisory mode" rather than "block by default". Match-only hooks (pii-detector, keyword-filter, content-safety) keep the block default because for them the match itself is the decision.

The standard `ReasonCode` constants record the divergences operators care about:

| ReasonCode | Meaning |
|---|---|
| `REDACT_INFLIGHT_UNSUPPORTED` | A `redact` match could not be applied on the wire because the adapter returned `ErrRewriteUnsupported` — upstream received the original body. Because the persisted copy is the wire copy, this also means the redacted wire copy is absent, so the raw body is dropped (`NULL`) rather than persisted unredacted. |
| `AIGUARD_SUGGESTED_VS_POLICY` | Admin policy ceiling overrode AI-Guard's suggestion — both values carried in `Reason` for audit forensics. |

The codes are stamped onto `Record.HookReasonCode` (request stage) / `Record.ResponseHookReasonCode` (response stage) at the proxy boundary; the UI locales (`pages.json`) carry user-facing strings for the closed set. Two further codes — `REDACT_STORAGE_ONLY_BY_POLICY` and `STORAGE_DROPPED_BY_POLICY` — stay defined as constants and locale strings so historical rows that carry them still render, but no live path stamps them: a single action admits no "store-redacted-but-forward-original" divergence, and `drop-content` is not an operator choice.

## 5. In-flight rewrite — the Modify decision path

When the aggregated `CompliancePipelineResult.Decision = Modify` and `len(ModifiedContent) > 0`, the request handler calls `trafficAdapter.RewriteRequestBody(ctx, body, path, contentBlocksToNormalized(hookResult.ModifiedContent))`. Three outcomes:

1. **Success** — the adapter returns the rewritten bytes plus a count of overwritten slots. The proxy stamps `rec.HookRewriteCount` + `rec.HookRewritten = true` and forwards the rewritten body upstream.
2. **`ErrRewriteUnsupported`** — the adapter cannot reverse-encode its wire format (e.g. a passthrough wire shape Nexus does not own). The proxy emits a warn log and **forwards the original body unchanged** while stamping `rec.HookReasonCode = ReasonRedactInflightUnsupported` on the audit row. Because the persisted copy is the wire copy and no redacted wire copy was produced, the raw body is dropped (`NULL`) rather than persisted unredacted — the audit store never becomes the leak.
3. **Any other error** — internal inconsistency (the body passed `ExtractRequest` but failed to round-trip). Surfaces as `500 request rewrite failed`.

The same three-arm pattern runs on the response side via `extractor.RewriteResponseBody` for non-streaming responses, cache-hit replays, and the streaming held-back SSE prefix.

Adapters MUST walk their schema in the same order as `ExtractRequest` / `ExtractResponse` emitted segments, so position `i` in `content.Segments` pairs with the i-th extractable slot — this is the canonical adapter contract documented on `Adapter.RewriteRequestBody`.

## 6. Storage — the redacted wire copy is what persists

The bytes Nexus persists are the **raw wire body** the adapter already produced in flight — there is no second, normalized copy redacted independently at audit time. The canonical normalized projection (`traffic_event_normalized.request_normalized` / `response_normalized`) is **never written by the audit pipeline**; the control plane recomputes it at view time from the stored raw body (which is the action-governed copy), so the recompute never resurrects sensitive bytes. See [audit-pipeline-architecture.md](../observability/audit-pipeline-architecture.md) for the write/view split and the view-time recompute.

The only storage-policy helper is `StorageRawBody` in `packages/shared/traffic/redact`, which selects the raw captured bytes allowed onto the persisted payload store under the action:

```
StorageRawBody(captured, redacted, action) →        (raw captured copy)
  approve          → captured bytes as-is
  redact / block   → ONLY the adapter-rewritten redacted wire copy; absent → NULL
  otherwise        → NULL
  captured nil     → NULL always (capture-off must never be resurrected)
```

`approve` persists the captured bytes unchanged. `redact` and `block` persist **only** the adapter-rewritten redacted wire copy (`RequestBodyRedacted` / `ResponseBodyRedacted`, the output of `RewriteRequestBody` / `RewriteResponseBody`). When that redacted copy is absent — no in-flight rewrite ran, or the adapter returned `ErrRewriteUnsupported` — the raw body is dropped (`NULL`) rather than persisted unredacted. This is the single fail-safe invariant: **when a redaction cannot be applied, drop the content rather than persist it.** It is also where the internal `drop-content` semantics survive — not as an operator choice, but as the automatic outcome of a redacting match that yields no usable redacted copy. A nil captured copy always yields nil so a storage policy can never resurrect bytes the capture config chose not to store.

`redact` produces one masked body across forward, return, and storage: the same `RewriteRequestBody` / `RewriteResponseBody` output is forwarded upstream, returned to the client, and persisted, so the three are byte-identical. `block` persists the redacted copy while rejecting the transaction, so an auditor reviewing the row sees the inline `[REDACTED_<RULE_ID>]` markers plus the rule-ID / category metadata already on the row — what kind was redacted and where — without any sensitive bytes and without any persisted normalized projection.

`redact.MarshalSpans` / `redact.CollectRuleIDs` serialize the post-redact span metadata for the wire/DB columns when a producer carries spans; the `request_redaction_spans` / `response_redaction_spans` columns stay on the schema as a shipped contract but the audit pipeline leaves them unset (the projection they annotated is not persisted).

Enforcement points:

- **AI Gateway** — `recordToMessage` (`packages/ai-gateway/internal/platform/audit/message.go`) calls `StorageRawBody` per direction. The audit `Record` ferries `RequestAction` / `ResponseAction` (the single per-stage action, derived from the pipeline decision via `ActionFromDecision`) and `RequestBodyRedacted` / `ResponseBodyRedacted` (the adapter `Rewrite*Body` output) from the pipeline result to the writer; the proxy stamps them at the request boundary and every response hook boundary (response / streaming / cache-hit stages). The normalized projection is never stamped.
- **Compliance Proxy + Agent** — `AuditEmitter.buildEvent` (`packages/shared/policy/pipeline/audit_emitter.go`) is the single choke point both MITM services emit through. It reads each stage's `CompliancePipelineResult.Action` (via the `stageAction` helper; a nil stage maps to `approve`) and applies `StorageRawBody` to the captured bodies — the tlsbump forward handler stamps `AuditInfo.RequestBodyRedacted` / `ResponseBodyRedacted` when an in-flight rewrite succeeded — so every downstream persistence (proxy MQ wire, agent SQLite, agent→Hub upload) receives only the action-governed raw copy. The normalized-projection fields are left nil.

## 7. Pipeline aggregation across multiple hooks

`pipeline.Execute` accumulates spans **from every hook that ran**, regardless of terminal decision — even Approve hooks may emit informational transforms (e.g. cache-normaliser strips wrapped as a hook integration). The aggregator:

- Appends `r.TransformSpans` to the union `allSpans` for every hook.
- Reduces per-hook `r.Action` to the pipeline action via `StrictestAction` (block beats redact beats approve).
- On `RejectHard`, returns immediately with the reject reason + the spans accumulated up to that point so the audit row sees what the rejected request *would have* redacted.
- On `Modify`, retains the last hook's `ModifiedContent` alongside the union `TransformSpans` — adapters that consume the span-driven rewrite read `TransformSpans` while the `ModifiedContent` slice serves narrow `RewriteRequestBody` callers that take a `NormalizedContent` argument.
- On `Approve`, optionally clears soft-reject accumulators when `clearSoftOnApprove` is set on the pipeline.

The final `CompliancePipelineResult` carries `TransformSpans` (the union) and `Action` (the strictest) — the spans drive the in-flight rewrite and the action drives what the audit store persists.

## 8. Cross-format consideration

Spans address the **canonical** post-normalize body (`messages.<i>.content.<j>`, `inputs.<i>`, `http.bodyView`), not the wire-shape body. This is what makes redaction wire-agnostic:

- A request that arrives as OpenAI Chat Completions, Anthropic Messages, Gemini GenerateContent, or the OpenAI Responses API all canonicalize via `IngressChatToCanonical` before the hook pipeline runs. The pii-detector sees the same canonical text and emits the same `messages.0.content.0` span set.
- The adapter's `RewriteRequestBody` translates the canonical span set back into the wire-specific schema — `RewriteRequestBody` is the inverse of `ExtractRequest` for that adapter, slot-for-slot. The rewritten wire bytes are what the audit store keeps under `redact` / `block`.

The wire formats can differ in how many slots an adapter exposes — `ExtractRequest` for an embeddings request returns `inputs[i]` slots, whereas a chat request returns `messages[i].content[j]` slots — and the span emitter follows the same shape (`KindAIEmbedding` branch in `executeRedact` emits `inputs.<i>` addresses; chat branch emits `messages.<i>.content.<j>` / `…toolResult`).

Because the persisted copy is the redacted **wire** body — not a separately recomputed normalized projection — there is no cross-format address-translation gap to bridge at storage time: the redaction has already been applied on the wire shape before the bytes are captured. The control-plane view-time recompute reads those already-redacted wire bytes, so it normalizes a PII-safe body regardless of any projection skew between the hook-time and view-time normalize paths.

The webhook-forward hook exists for the special case of *external* compliance webhooks that return a flat-offset redaction list against a flat joined projection. Those redactions arrive as `aiguard.Redaction`-shaped wire records and are decoded into `TransformSpan` with `ContentAddress = "webhook.flat"` — a sentinel address that `ApplySpans` does **not** resolve. The webhook spans therefore land in the audit row for forensic completeness but do *not* mutate the in-flight body; inflight redaction of AI-Guard-style suggestions requires the internal `aiguard-classify` path inside ai-gateway, which produces canonical `SourceAIGuard` spans against the addressed payload structure.

## 9. Audit annotations

The audit row stamps the following PII-related fields:

- `Record.HookDecision` / `Record.ResponseHookDecision` — terminal hook pipeline decision (`Approve` / `RejectHard` / `BlockSoft` / `Modify`).
- `Record.HookReason` / `Record.ResponseHookReason` — human-readable reason string (e.g. `"PII detected: email"` or `"PII redacted"`).
- `Record.HookReasonCode` / `Record.ResponseHookReasonCode` — closed-set machine code, set by the hook itself (`PII_DETECTED`, `PII_REDACTED`) and overridden by the proxy with `REDACT_INFLIGHT_UNSUPPORTED` when the adapter could not honour an in-flight redact, or `AIGUARD_SUGGESTED_VS_POLICY` when the admin ceiling overrode an AI-Guard suggestion.
- `Record.HookRewriteCount` / `Record.HookRewritten` — how many content slots the adapter actually overwrote and a boolean rewrite-applied flag.
- `Record.ComplianceTags` — union of per-hook tags (`compliance:pii`, `severity:confidential` from pii-detector; other tags from sibling hooks).
- `Record.BlockingRule` — rule-pack attribution (pack, version, rule ID) when the decision came from a rule-pack engine.
- `Record.HooksPipeline` — full ordered hook-execution trace via `HookExecRecord` (per-hook JSON fields: `stage` / `order` / `hookId` / `name` / `decision` / `reason` / `reasonCode` / `latencyMs` / `error`; `name`, `reason`, `reasonCode`, and `error` marshal omitted when empty).
- `Record.RequestAction` / `Record.ResponseAction` — the single per-stage match action (`approve` / `redact` / `block`) the audit writer keys the persisted raw body off, plus `RequestBodyRedacted` / `ResponseBodyRedacted` carrying the redacted wire copy.

The control-plane UI's traffic-audit drawer reads `HookReasonCode` and surfaces a chip for the closed-set codes with locale-translated explanatory text. The English / Spanish / Simplified Chinese bundles carry the strings for the two codes still stamped (`REDACT_INFLIGHT_UNSUPPORTED`, `AIGUARD_SUGGESTED_VS_POLICY`) plus the two retained for historical rows (`REDACT_STORAGE_ONLY_BY_POLICY`, `STORAGE_DROPPED_BY_POLICY`).

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
