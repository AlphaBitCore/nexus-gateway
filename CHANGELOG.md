# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.6.0] — 2026-08-23

### Added

- **`Model.features` gains `structured_outputs`, and GET /v1/models publishes
  it.** The array is a shipped contract, so this is an additive contract change:
  existing values are untouched and a consumer reading only the older tags is
  unaffected. It answers a question no existing tag answered — will this model
  hold its answer to a caller-supplied JSON Schema — which is NOT what
  `json_mode` says. `json_mode` describes the weaker
  `response_format: {type: json_object}`, and the two disagree on exactly the
  rows that matter: probed per model against each provider's own wire on
  2026-08-19, `gpt-4-turbo` carries `json_mode` and answers 400 to a schema,
  while every `claude-*`, every `command-*` and the whole o-series carry no
  `json_mode` and serve one correctly.

  The tag is an ELIGIBILITY constraint, not a preference: `auto` will not choose
  a model whose row lacks it for a request carrying a `json_schema`. One
  catalogued model is the reason — `kimi-k2.5` accepts the field and answers
  with HTTP 200, `finish_reason: stop`, and prose, so nothing downstream can
  turn it into an error and the caller's own parse is the first thing that
  notices. A model the caller NAMES is unaffected; their model's limits are
  theirs.

  **A row that declares other features and not this one is excluded from
  structured-output routing.** 44 rows on the six providers a key exists for are
  tagged from measurement; rows on the thirteen template-only adapters
  (`azure-openai`, `bedrock`, `vertex`, `fireworks`, `together`, `groq`,
  `mistral`, `xai`, `glm`, `minimax`, `perplexity`, `huggingface`, `replicate`)
  are untagged and therefore not selected for such requests. Operators running
  those providers who want them eligible should probe the wire and tag the rows;
  inferring the answer from a direct-provider twin is what this tag exists to
  avoid.

- **`capability_matrix` on GET /v1/models gains `reasoning` and
  `structured_outputs`.** Both are hard routing filters, so a client could not
  observe from the public catalogue the two facts `auto` actually acts on.
  Additive: the five existing keys are unchanged.

- **Media is one shape, with one grammar and one resolver.** Every binary a
  request or response carries — image, audio, video, document — is now a
  single `MediaRef` naming what the bytes are, proving which bytes they were
  (`sha256`), and, when they are recoverable, saying where to find them. It
  replaces two `BinaryRef` families, six divergent per-codec behaviours and a
  parallel `image_ref` summary.

  The locator grammar has five containers, each resolvable from the stored
  bytes alone: `body`, `json:<path>`, `datauri:<path>`, `sse:<frame>:<path>`,
  `multipart:<part>`. Grammar, predicates and resolver live in one package
  (`transport/normalize/locator`), because a locator is a promise, and a
  promise made in one place and kept in another drifts — the codecs and the
  admin artifact endpoint previously disagreed about what counted as
  decodable.

- **`GET /api/admin/traffic/{id}/artifact` resolves any locator**, on either
  direction, through the same package that built it:
  `?locator=<locator>&direction=request|response`. Request-side media — an
  uploaded image, transcribed audio, a submitted document — is reachable for
  the first time. Omitting `locator` keeps the endpoint's original behaviour
  for image generations and TTS; those derive the locator the caller did not
  send and take the same path rather than being a second implementation.

  The served `Content-Type` is sniffed from the bytes, never taken from what
  the wire declared. Types outside a frozen inline-renderable set still serve;
  they download instead of rendering. Every response carries `nosniff`, a
  `default-src 'none'; sandbox` CSP, `Cross-Origin-Resource-Policy:
  same-origin`, `Cache-Control: private, no-store`, an `ETag` over the bytes
  (`If-None-Match` yields 304), and a filename built from the event id and the
  sniffed type. New statuses: `400` (unreadable locator), `422` (the bytes are
  there and are not decodable).

- **Large binary request inputs are captured.** STT audio and video input
  references were fingerprinted and released; the transcription was auditable
  and the thing transcribed was not. They are now captured by handing over the
  buffer the request already holds rather than copying it — measured at the
  26 MiB STT ceiling, 3.3 ns and zero allocations against 1.36 ms and 27 MB
  across four allocations for the copy path. The size lands off-heap on the
  async audit side through the existing spill. Capture never changes what is
  forwarded upstream, and both paths pin that with a test that re-emits the
  forward after capture has taken its reference.

  With payload capture off, the fingerprint alone is surfaced as a
  `fingerprint` media element, so the file is still identifiable by digest.

- **Published multi-architecture container images and a `docker compose`
  quickstart.** `nexus-hub`, `control-plane`, `ai-gateway`,
  `compliance-proxy`, `control-plane-ui`, and `db-migrator` are now built and
  published to `ghcr.io/alphabitcore` and `docker.io/alphabitcore` for
  `linux/amd64` and `linux/arm64`, plus an amd64-only `-avx2` tag variant for
  the four Go services. `deploy/docker-compose.yml` brings up a working
  instance from those images in two commands (`./init-secrets.sh` then
  `docker compose up -d`); see
  `docs/developers/architecture/cross-cutting/deployment/container-image-architecture.md`
  and `docs/operators/ops/container-deployment.md`.

- **Self-contained Linux tarballs.** `scripts/release/build-tarball.sh`
  produces `nexus-gateway-<version>-linux-<arch>.tar.gz` with statically
  linked service binaries, the built UI, and systemd units, for operators
  who deploy without containers.

- **Vendor-spend reconciliation: `traffic_event` now records the smart-router
  LLM call's own cost and the provider that served it, closing the gap that
  under-reported OpenAI spend by 28.2% ($39.46 over eleven days of production
  traffic) while Anthropic reconciled to -0.07%.** Three additive
  `traffic_event` columns — `router_cost_usd`, `router_provider_id`,
  `embedding_provider_id` — carried over new binwire field ids `109`-`111`,
  plus two new rollup series (`vendor_spend_usd`, `vendor_spend_internal_usd`)
  that attribute each cost component (customer traffic, router call, L2
  embedding, AI-Guard classifier) to the provider that was **actually
  charged** for it — which is frequently not the provider that served the
  request. One `traffic_event` row can therefore contribute to several
  providers' vendor-spend totals at once. The vendor-bill reconciliation job's
  diff basis moves to this series via two additive
  `vendor_bill_reconciliation` columns, `our_vendor_spend_usd` and
  `our_internal_ops_usd`; the existing `our_billed_usd` column and customer
  quota/billing are untouched. The reconciliation report's read and
  review-ack endpoints gain two additive response fields, `ourVendorSpendUsd`
  and `ourInternalOpsUsd`, and a `no_basis` coverage value for days the
  vendor billed but the gateway recorded no comparable vendor spend for.
  Rows written before this change are not comparable — router cost was never
  recorded in any form historically, so no backfill can reconstruct it — and
  the report marks them as such. See
  `docs/superpowers/specs/2026-08-04-vendor-spend-reconciliation-design.md`
  for the full analysis and
  `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md`
  for the cost-attribution detail. One side effect: the AI-Guard classifier's
  own `traffic_event` row now carries a `routed_provider` dimension (it
  previously carried none), so an operator who enables AI-Guard will see that
  provider's per-provider request count and token totals step up in
  Analytics → By Provider with no other config change — `billed_cost_usd` is
  unaffected. Dormant today: production has zero ai-guard rows.

  **Deploy order is mandatory: schema → Hub → producers.** The new binwire
  field ids are FORWARD-INCOMPATIBLE — a producer (ai-gateway,
  compliance-proxy, agent) emitting field id 109, 110, or 111 to a Hub that
  does not yet know them has that field silently dropped, not queued or
  errored. Apply the schema migration first, deploy the Hub second, and only
  then deploy/restart the producers.

  **Operator impact — expect one `vendor.bill_sync_failed` alert per
  reconciled provider on first deploy, and it is not a regression.** The
  reconcile job's trailing window is `[today-5, today-2]` (UTC); on its first
  run after this deploy, days in that window predate the vendor-spend series
  and get `coverage='no_basis'` placeholder rows — an honest "we have no
  comparable basis for this day" signal, not a computed 100% drift. Once a
  `no_basis` (or `fetch_failed`) row stays unhealed past the ~25-hour
  staleness threshold it raises `vendor.bill_sync_failed`, so expect exactly
  one such alert per covered provider shortly after deploy. It auto-resolves
  as those pre-cutover days age out of the bounded trailing window — no
  action needed. See
  `docs/operators/ops/runbooks/vendor-bill-reconciliation.md` (the `no_basis`
  row in the coverage table) and `docs/operators/ops/runbooks/alerts.md`
  (the `vendor.bill_sync_failed` section) for the full alert semantics.

- **The `audio` model type is retired.** It was minted by model discovery for
  any id containing "audio", and the models that received it — `gpt-audio-*` —
  are served by the provider on chat completions, so the routing guard
  rejected every one of their requests with `MODEL_MODALITY_MISMATCH`. `type`
  answers *which endpoint serves this model*; which modalities it handles is
  what `inputModalities` / `outputModalities` are for, and one scalar cannot
  answer both. Discovery no longer mints it, the two affected rows are typed
  `chat`, and `EndpointKindAcceptsModelType` no longer accepts it for tts,
  stt or realtime.

  **Migration:** none for callers. Any catalogue row still carrying
  `type: "audio"` should be retyped to the endpoint that actually serves it;
  a standing test fails if one appears.

- **Catalogue modality data corrected.** 94 models advertised `vision` while
  declaring text-only input, 33 contradicted their own type (embeddings
  emitting text, transcribe models taking no audio), and 188 carried no
  modality arrays at all. Nothing broke only because the routing guard reads
  `type` and ignores those fields — which made the drift latent rather than
  harmless. A standing check now asserts the three descriptions agree, so the
  next model sync cannot reintroduce it.

- **Media custody has six states, each with a producer.** `aged-out` gained
  one: when the Control Plane serves a stored normalized sidecar after
  establishing that no body is recoverable, captured references in it are
  rewritten — locator cleared so no control is offered, digest kept so the
  file is still identifiable. Previously such a row offered a Download that
  resolved to 404. `redacted` was removed; nothing produced it, and a custody
  value nothing can reach is a distinction the system claims to make and does
  not.

- **Cohere catalogue completed.** `command-a-vision-07-2025` (verified against
  the live API) and the `rerank-v3.5` / `rerank-english-v3.0` /
  `rerank-multilingual-v3.0` family are catalogued. Rerank rows record no
  per-token price — Cohere bills per search unit, and a zero would render as
  "free" where absent renders as "not priced".

- **`traffic_event.end_user_id` is stamped from `X-Nexus-End-User-Id` only.**
  The tag correlates traffic to the same end user across the Nexus product
  family, so it is something a caller declares to Nexus. The gateway no longer
  falls back to a provider's own end-user field — the OpenAI shape's top-level
  `user` / `safety_identifier` and the Anthropic shape's `metadata.user_id`
  identify the caller's end user *to that provider* and answer a different
  question, so filing traffic under them attributed rows to an identifier
  nobody chose for the purpose.

  **Migration:** callers relying on the fallback must send the header;
  otherwise `end_user_id` is NULL for their rows from this release on and
  existing rows are untouched. Attribution now behaves identically on every
  ingress shape rather than depending on which protocol a caller speaks, and
  the request path no longer scans request bodies for the field — measured at
  27 microseconds for a 64 KiB body and 105 microseconds at 256 KiB, against
  32 nanoseconds when the header is present.

### Changed

- **`error.code` is a string on every error the gateway itself produces.** A
  whole class of them sent the numeric HTTP status in that field — `"code": 429`
  next to `"type": "rate_limit_error"` — so a client branching on `error.code`
  saw a number from one route and an UPPER_SNAKE string from the next. Every
  gateway-originated error now goes through one builder: `code` is an
  UPPER_SNAKE string or is absent, never the status restated, and `type` is
  derived from the status rather than fixed at `api_error`. Migration: branch on
  `error.code` as a string, and on `response.status` for the numeric status.
  Provider errors are untouched — they keep the upstream's own words inside your
  dialect's envelope.

- **One error envelope across the gateway's own routes.** Five shapes were in
  service — the proxy's, the estimate endpoint's, the not-supported handler's,
  and two more — differing in whether `code` was present, whether a hint was
  inline, and which `type` they claimed. They are one shape now. The estimate
  endpoint no longer has an envelope of its own.

- **One 429, with the headers to act on it.** Rate-limit refusals differed by
  route family and some carried nothing to back off with. Every 429 now carries
  `Retry-After` and `X-RateLimit-Limit`, the compare endpoint (`/v1/estimate`)
  included — it has its own per-key ceiling and used to answer with neither.

- **A path this gateway does not serve answers in JSON, and a wrong method
  answers 405.** A miss outside `/v1` returned a text/plain 404 from the mux; a
  wrong method on a served path returned 404 with no `Allow`. Both now return
  the dialect's JSON envelope, and a wrong method returns 405 with `Allow`
  listing the methods that path does serve.

- **The response carries one `x-request-id`, not two.** When a caller sent their
  own, the gateway appended its own value beside it and clients read whichever
  their HTTP library happened to surface first. The caller's value is echoed
  back unchanged when they send one, and the gateway's own is used when they do
  not.

- **`traffic_event.id` is minted by the gateway and is no longer the same value
  as `trace_id`.** They were equal for every row, so anything that had the
  `X-Nexus-Request-Id` response header could pass it to a row-keyed endpoint and
  it worked by coincidence. `trace_id` is the enclosing unit of work and is
  non-unique by design — retries, a realtime session's exchanges, an agent flow
  across services all share one; `id` is the per-row primary key and the
  idempotency key on the audit path. The header carries the TRACE id, which is
  the only handle a caller ever holds. Migration: to reach the rows behind a
  header value, filter the admin traffic list by `requestId` (it matches
  `trace_id`, and is what the drawer's trace pivot uses); `GET
  /api/admin/traffic/{id}` continues to take the row's own id.

- **A Gemini-dialect wrong-method answers `status: INVALID_ARGUMENT`.** 405 is
  absent from Google's documented HTTP-to-gRPC table so it fell through to
  `UNKNOWN`, which is the one thing that does not tell a Gemini client the
  method was wrong.

- **An image `n` above the resolved provider's ceiling is refused before
  dispatch**, with `SPEND_LIMIT_EXCEEDED` naming the field, the bound and the
  provider. Routing can send one request to providers with different ceilings,
  and the request used to reach the upstream and bill for whatever it produced.

- **A quota-exhausted provider is the provider's fault, not the caller's.** Nine
  provider adapters classified an exhausted account budget as an invalid
  request, so `model: auto` returned the caller a 400 instead of failing over to
  a provider that could serve them.

- **`/v1/estimate` honours the parameters it accepts.** Three of them were
  parsed and then ignored.

- **The rerank document ceiling applies on the native Cohere target too**, where
  the validation had been skipped, leaving the 1000-document guard inert on the
  one path that reaches Cohere directly. And the TTS `instructions` field is
  scanned by the content policy — it is caller-supplied text and was not.

- **`external_request_id` survives the agent's local audit queue**, and an id
  the agent does not have reaches the column as SQL NULL rather than the empty
  string, matching what gateway-written rows have always stored.

- **`platform.BuildInfo` takes two fields instead of three (Go API, internal).**
  `ServiceVersion` / `BuildSHA` / `BuildTime` are replaced by `Service` and
  `BuildVersion`; `CaptureStaticInfo` resolves all three itself. No migration
  window is offered because the type has no consumer outside this repository —
  it is not part of any published Go module surface, and every in-repo call site
  moved in the same commit. Callers passing the old fields will not compile,
  which is the intent: when callers computed the three fields themselves, three
  of the five services reported a hardcoded `"<service>/0.1.0"` and all five
  reported an empty `buildSha`, so no node could be tied to a build at all.

- **`Model.features` no longer carries two names for one capability.** GET
  /v1/models publishes this array verbatim, so this is a contract change.
  `thinking` is now `reasoning`: the two described the same capability and the
  vendor sets were disjoint — Anthropic and Gemini rows said the first, thirteen
  other providers said the second, and no row said both — so a rule, a picker,
  or a query keyed on either one saw a partial answer with nothing to say so.
  The canonical layer had already chosen `reasoning` for the response content
  type and the token counter. `tool_use` is removed: it sat on four rows, every
  one of which also carried `function_calling`, and distinguished nothing.
  **Consumers keying on `thinking` or `tool_use` must switch to `reasoning` and
  `function_calling`.** Stored rows are migrated by
  `dedupe_model_feature_vocabulary` in `tools/db-migrate/manual-scripts/`; the
  admin picker keeps rendering an unmigrated value so such a row stays editable
  rather than losing it on the next save.

- **`requestedModelLiterals` matches as globs, which is what the form has always
  said it does.** The admin form offers `gpt-4-*` as its example for this field
  while the comparison was exact equality, so a rule written from that example
  matched nothing and left nothing to read. The raw `model` string is where
  version suffixes live (`gpt-4o-2024-11-20`), so a pattern is the only way to
  write a rule that survives the next model release. A value with no `*` still
  compares exactly, so `auto` — which every smart rule is required to pin — is
  unaffected. **A stored value containing a `*` starts matching after this
  ships**; review any rule that carries one.

- **A rule scoped to a provider no longer fires according to catalogue row
  order.** `matchConditions.providers` compared the first candidate the
  catalogue returned for the named model code, and the query resolving a code
  states no order — so for a code two providers both serve, the same request
  could match on one day and not the next. It now compares against every
  provider serving that code, the same shape `matchConditions.models` already
  used. The requested-side provider fields stay empty for an ambiguous code
  rather than naming one arbitrarily.

- **A request a routing rule redirected is refused rather than served by the
  model the caller named.** When every rule that matched resolved no target, the
  gateway fell through to the requested-model passthrough — so a compliance rule
  redirecting `gpt-4o` elsewhere, whose targets were all unavailable, answered
  with `gpt-4o` and a 200. Such a request now returns 503
  `ROUTING_RULES_RESOLVED_NOTHING`, and the routing trace records why each rule
  yielded. A request no rule matched still passes through unchanged.

- **Smart routing carries two more of its own candidates.** `model: auto`
  returned a single target and discarded the pool the router had just chosen
  from, so one transient failure sent the request out of the rule entirely. Two
  other members of that pool now follow the pick — a provider not already in the
  plan first, then the cheaper model. This raises the maximum upstream calls one
  auto-routed request may make, since the call budget is derived from the plan's
  length.

- **`stickyOn` / `stickyTtlMs` are removed from the loadbalance strategy.** No
  gateway code ever read them, while the rule form described Redis-backed
  session affinity across replicas that did not exist. A payload still carrying
  the keys keeps validating; they are ignored, as they always were.

- **The traffic row records the class of each failed attempt.** The dispatch
  walk stamped the `retryOn` bucket, which spells an unrecognised provider error
  and a genuine network fault identically, and the audit projection dropped even
  that. Attempts now carry `errorClass` in `routing_trace`, in the same
  vocabulary the neighbouring `code` field uses — three classes have names of
  their own (`permission_denied`, `network`, `unknown_provider_code`) because
  they draw distinctions a canonical code cannot.

- **Routing rules now match on the endpoint and on the request, not on whatever
  model the caller happened to name.** Two match conditions read facts that do
  not exist for a request that names no model, which made the rules carrying
  them silently inert for exactly the traffic they were written for. Both
  changes alter which rule serves existing traffic; neither is silent, and
  neither needs a data migration.

  `modelTypes` is compared against the endpoint the request arrived on. Stored
  values are unchanged — an existing `embedding` keeps meaning the embeddings
  endpoint, and the audio sub-types keep their coarse-`audio` compatibility,
  because the comparison goes through the same catalogue-type-to-endpoint
  translation the rest of the gateway uses. What changes: a rule with a
  `modelTypes` condition now matches `model: "auto"` requests on that endpoint.
  Previously it could not match them at all, because `auto` names no model and
  the condition was reading the named model's catalogue row.

  `providers` is now INAPPLICABLE when the caller named no model, instead of
  failing to match. An admin who scopes a rule to one provider means "route
  within this provider", and `auto` is the case where the gateway does the
  routing. A request that DID name a model is still compared, so a rule scoped
  to one provider still leaves another provider's model alone.

  **Review any rule that carries a `modelTypes` or `providers` condition and
  sits above a catch-all.** Such a rule may now capture `auto` traffic that
  reaches a lower-priority rule today.

- **A virtual key is refused a model it names but is not allowed**, rather than
  being served whatever a routing rule would have redirected it to. A key
  restricted to one model, with a rule redirecting everything to that model,
  previously answered a client pinned to a different model with a 200 — the
  client's configuration silently overridden and every response attributed to a
  model the key cannot use. The refusal is a `403 MODEL_NOT_ALLOWED` naming the
  model the caller sent.

  This applies only when the caller named exactly one catalogue model. `auto`
  is not a catalogue model and can never appear on an allow list, and a code
  that fans out to several providers has no single reference to match; requiring
  either would refuse every routed request from every restricted key.

- **A routing rule that matches but produces nothing now says so** on
  `traffic_event.routing_trace`, naming the rule and which of the four reasons
  applied: its configuration does not parse, its strategy could not be
  evaluated, it resolved no target, or every target it resolved is outside the
  calling key's allowed models. Every trace entry also carries the rule that
  produced it, so a losing rule's reasoning is no longer read as the winner's.

- **A routing rule's entries name a provider and a model; a nested strategy is
  refused.** The gateway resolves an entry inside a strategy as a leaf and does
  not evaluate one strategy inside another. The admin API previously accepted
  nesting ten levels deep, and such a rule was persisted, broadcast fleet-wide,
  and then routed nothing while showing as enabled. It is now refused at the
  write boundary, where the admin can be told what a child must be.

  The simulate walker agrees rather than descending. A stored nested entry is
  reported as unreachable with the reason, instead of contributing a
  probability to a distribution no live request can produce.

  **Migration, unconditional:**
  `tools/db-migrate/manual-scripts/disable_nested_routing_configs_2026_08_08.sql`
  disables such rules and appends the reason to the description. Configurations
  are preserved, not flattened — which branch an admin meant to keep is not
  something a migration can know.

- **A rule that backs up other rules is an ordinary rule at lower priority.**
  `strategyType: "fallback"` no longer marks a separate species of rule that was
  collected apart from the others and appended to every plan as recovery,
  whatever it matched. The walk now advances between rules on its own, so a rule
  an admin ranked below another IS the alternative for when that one cannot
  serve the request — and a `fallback` rule no longer backs up rules it has
  nothing to do with while its own match conditions go half-read.

  The `fallback` STRATEGY is unchanged: a chain of provider+model entries inside
  one rule is a different thing from a rule that backs up other rules, and only
  the second is gone. A rule's own chain is still tried before any lower rule.

  **Review every rule whose strategy type is `fallback`.** Such a rule
  previously could not win the primary slot whatever its priority, and backed up
  every other rule regardless of its own match conditions. It is now an ordinary
  rule: a high-priority one with broad match conditions will START serving
  traffic as the primary, and a narrowly-matching one will STOP backing up the
  rules it used to cover. Set its priority below the rules it is meant to back
  up, and check that its match conditions cover the traffic it should catch.

- **The structured embeddings 400 is for requests the gateway routed.** It lists
  what each candidate would have accepted — dimensions, batch size, encoding
  formats — which answers "you asked us to pick and none of ours fit". A caller
  who NAMED a model asked a different question: that model's limits are its own,
  and the upstream states them in its own terms. They now receive the provider's
  refusal instead of a catalogue of models they did not choose, which also stops
  publishing the shape of a deployment's model list to a caller who wanted one
  model.

- **Vendor-bill reconciliation's `diff_usd` / `diff_pct` are now computed from
  the vendor amount and our recorded vendor spend rounded to cents**, since
  the vendor's own cost APIs (e.g. Anthropic's `cost_report`) are only
  cent-denominated — comparing them against our unrounded 6-to-10-decimal-place
  estimate reported the vendor's own rounding as drift. A day where the two
  figures agree to the cent now reconciles to exactly `diff_usd = 0` /
  `diff_pct = 0` instead of a spurious nonzero percentage. **Stored amounts are
  unchanged**: `our_billed_usd`, `our_vendor_spend_usd`, `our_internal_ops_usd`
  and `vendor_reported_usd` keep their full unrounded precision in their
  existing `Decimal(20,10)` columns; only the two diff figures are quantised.
  Rows written before this change keep their old, unrounded-basis diff until
  the trailing `[today-5, today-2]` re-reconcile window recomputes them. See
  `docs/operators/ops/runbooks/vendor-bill-reconciliation.md` ("Diff basis is
  rounded to the cent").

### Fixed

- **`/v1/messages` usage counters now compose the way Anthropic's own do.** The
  two conventions differ: canonical follows OpenAI, where `prompt_tokens` is the
  total and `cached_tokens` is a subset inside it, while Anthropic's wire is
  additive — `input_tokens` counts only what was neither read from nor written
  to the cache, and the two cache counters stand beside it. The egress wrote the
  canonical total into `input_tokens` while also emitting both cache counters,
  so the three overlapped and a client summing them as Anthropic documents
  over-counted by the whole cached prefix. Observed on a real cached request:
  `input_tokens 12936` beside `cache_creation_input_tokens 12924`, where the
  real uncached input was 12.

  **This changes numbers an Anthropic-wire client may be reconciling against.**
  A client that summed the three counters was over-counting and will now see the
  correct total; a client that read `input_tokens` alone as "everything" will now
  see only the uncached part and should add the cache counters, which is what
  Anthropic's own API requires. Cache-creation tokens bill at a premium and
  cache-read at a discount, so the correction matters most on requests that used
  prompt caching. The Gemini egress is unaffected and unchanged — its
  `promptTokenCount` is a total with `cachedContentTokenCount` as a subset,
  already the canonical convention.

- **A caller's `cache_control` reaches the wire from every ingress**, not only
  from `/v1/messages`. The gateway already promised to respect a caller-set
  marker — the marker injector stands down when it finds one — but on any other
  ingress the canonical→Anthropic projection rebuilt each content part and the
  marker was gone before the injector could look. Measured: the same marker
  cached on `/v1/messages` and did nothing on `/v1/chat/completions`. Anthropic
  prompt caching is roughly a tenth of the input price, so the silent version
  cost the caller money on every repeated prefix.

- **The gateway's internal namespace no longer reaches the caller.** A response
  from an Anthropic-backed model arrived at an OpenAI-wire client carrying
  `"nexus":{"ext":{"anthropic":{"cache_creation_input_tokens":N}}}`. The
  Anthropic, Gemini and Responses egress converters are projections and drop
  unknown keys as a side effect of rebuilding the body from named fields; the
  OpenAI-family egress is the identity, so whatever a codec left in the
  namespace was delivered verbatim.

  Four of the five response-side writers are deleted rather than filtered.
  Three had no reader anywhere (`nexus.ext.bedrock.model`,
  `nexus.ext.voyage.model`, `nexus.ext.cohere.returned_embedding_type`), and
  the fourth wrote a number the canonical body already carried two lines above
  it — Anthropic's cache-creation count now has one home,
  `usage.prompt_tokens_details.cache_creation_tokens`, beside the
  `cached_tokens` OpenAI defines. An Anthropic-wire caller still receives
  `cache_creation_input_tokens` unchanged. Only `nexus.ext.openai.responses.*`
  remains as a genuine carrier between a decode and the Responses egress
  encoder, so only that one is removed at egress.

- **Audio sent to `/v1/responses` is served instead of refused.** The Responses
  wire has no audio content part — its own rejection enumerates what it takes:
  text, image and file — while the same model accepts audio on
  `/v1/chat/completions`. A request carrying content that wire cannot express
  now takes the wire that can and comes back in the Responses shape the caller
  asked for. A request that ALSO uses a Responses-only feature (a built-in
  tool, `previous_response_id`, `store`, a non-disabled `truncation`) still
  gets the existing refusal naming that feature, because it cannot be served
  either way.

- **Smart routing no longer picks a model that cannot take the request's
  content.** Its capability filter asked about images only, so a request
  carrying audio, video or a document was routed without anyone checking
  whether the selected model accepts it — and the router produced the upstream
  400 itself. It now asks once per modality the request actually carries. A
  refusal the caller's own model choice caused is the model's; a refusal our
  pick caused is ours.

- **Every sampling rule now fails safe inside its vendor namespace.** Each
  vendor's rule is an ACCEPTS list — a family the list does not name has its
  `temperature` / `top_p` stripped rather than forwarded. Anthropic already
  read this way; OpenAI and Moonshot were denylists, and a denylist over a
  catalog that gains models without a code change goes stale in the direction
  that hurts.

  The reason it must be strip is routing. A rule or the smart router sends the
  request to a model the caller never named, so the parameters that arrive were
  chosen for a different one. If the unknown direction forwards, routing itself
  becomes the cause of a 400 — the caller asked for something that worked and
  got a failure the redirect created. Stripping costs sampling control on that
  request, reported on `X-Nexus-Coerced`; forwarding costs the request.

  The namespaces are version-aware rather than string prefixes, so a generation
  nobody has probed yet lands inside the rule instead of outside it. Models
  below the rejecting generation — `gpt-4o` and earlier, `moonshot-v1-*` — stay
  outside deliberately: they accept sampling, and stripping there would forfeit
  caller intent without averting anything. The DeepSeek thinking-mode structural
  fixes carry forward the same way.

- **The catalog sync compared nine of the sixteen fields the template carries.**
  The three modality lists and the three audio prices were never offered as
  corrections, so a provider row could drift on exactly the fields that decide
  whether a request is routable and what it costs. Fields are now classified at
  compile time, so one added to the template cannot reach the diff without a
  decision about it.

- **Adding a model with no required modality answered 500.** The create path
  left `requiredModalities` nil, which reaches Postgres as SQL NULL; an explicit
  NULL beats the column DEFAULT and the column is NOT NULL. Almost no catalog
  entry declares a modality floor, so this was nearly every manual model
  addition.

- **Output ceilings take the minimum of a live probe and the vendor's
  documentation.** A ceiling published on `/v1/models` is one callers echo back,
  so a number the wire rejects turns the gateway's own advertisement into a 400.
  The o-series carried the probe's 100003 against a documented 100,000.

- **A media card could offer a control that failed on click.** Three separate
  causes, each closed: the frame walker behind `sse:` locators split on a
  literal `"\n\n"` while the codec's own fold and the browser resolver both
  handled CRLF, so a captured reference pointed at a frame the resolver could
  not see; the aged-out sidecar case above; and a `datauri:` payload with
  leading whitespace or a format character that one side trimmed and the other
  did not.

- **HEIC, HEIF and AVIF were served as `video/mp4`.** The admin endpoint and
  the browser held two sniffers that disagreed about the same bytes — an
  iPhone photo submitted as a video input reference downloaded as `.mp4` from
  one and read as `image/heic` in the other. They now share a vector table
  that both assert against.

- **A whole `data:` URI could be stored in `MediaRef.URL`.** A prefix test
  against a client-controlled string is not a guard: a leading space, a
  zero-width character, or an uppercase `DATA:` scheme walked a 20 KB payload
  into a reference field. Normalisation now covers whitespace and the Unicode
  format category, the scheme test folds case per RFC 3986 §3.1, and a length
  bound catches whatever a future classifier misses.

- **A base64 payload could reach the compliance scanner as prose.** Media
  elements contribute nothing to the text projection hooks scan, and the text
  beside them still does; both halves are pinned.

- **Replicate array outputs.** Captions kept their position relative to
  artifacts, each artifact addresses its own index, and a non-string element
  is named rather than silently dropped — the drop fused the text either side
  into one utterance that was never sent.

||||||| 8d850e575
- **Vendor-bill reconciliation no longer reports a day the rollup correction
  pass has not rebuilt yet.** The reconciliation basis (`metric_rollup_1d`
  `vendor_spend_usd`) is produced for historical days by `rollup-correction`,
  and both jobs run on the same 24-hour tick: the reconcile pass finished in
  ~3 seconds while the correction pass took ~100 seconds to write the very
  rows it reads. Every run therefore read the tier before it was rebuilt and
  stamped `coverage = 'no_basis'` on days whose figures landed a minute later,
  which `vendor.bill_sync_failed` then escalated 25 hours after the fact. The
  correction job now publishes a `rollup-correction` watermark naming the
  newest UTC day it rebuilt, and the reconcile job defers any day past it —
  writing nothing rather than a placeholder, and reconciling the day on a
  later run. The same gate stops a *partially* aggregated day (the live
  rollup covered only the hours after a deploy) from being compared at all,
  which would have fired `vendor.bill_drift` with a fabricated under-record.
  Days already reconciled are untouched.

- **A single rate-limited vendor cost-API response no longer costs a provider
  its whole reconcile window.** OpenAI meters its admin API at 30 requests per
  minute across the entire organization, so the costs endpoint answers 429 for
  traffic the reconcile job never issued; any non-200 was terminal, so all four
  days of the window became `fetch_failed` placeholders. Rate-limited and 5xx
  responses (and transport failures) are now retried up to four attempts with
  exponential backoff — 0.5s, 1s, 2s — honouring `Retry-After` when the vendor
  sends it. Standing failures (400/401/403/404) are still reported on the first
  response, so a revoked or unscoped admin key surfaces as promptly as before.

- **An expired virtual key can be brought back.** Extending a key's
  `expiresAt` left `vkStatus` reading `expired`, and the renew endpoint
  matched only `active` keys, so it returned 404 on exactly the keys it
  exists to rescue. Because the gateway refuses admission on any non-`active`
  status as well as on a past date, the stale status alone kept a renewed key
  rejected. `expiresAt` is now the source of truth and `vkStatus = 'expired'`
  is derived from it: writing the column re-derives the status (a future or
  cleared date returns the key to `active`, a past date marks it `expired`
  immediately rather than at the next hourly job run), and renew accepts an
  expired key. `revoked`, `rejected` and `pending` keys are unaffected by an
  expiry edit — those are administrative decisions, not clock positions.

### Removed

- **`POST /v1/audio/translations` is withdrawn.** No catalogue model served it,
  so every call reached a routing failure; the route now answers as an endpoint
  this gateway does not serve. Migration: transcribe with
  `POST /v1/audio/transcriptions` and translate the text with a chat model.

- **The `policy` strategy type is gone from the admin API and the published
  schema.** It never had an implementation. A rule carrying it was accepted,
  broadcast fleet-wide, and then held the primary slot at stage 1 while
  returning no targets and no error — locking out every lower-priority rule
  with nothing surfaced. `POST`/`PATCH` now reject the value, at the top level
  and as a nested node.

  **Migration, unconditional:**
  `tools/db-migrate/manual-scripts/disable_policy_routing_rules_2026_08_08.sql`
  disables stored `policy` rules and appends the reason. The row and its config
  are kept and NOT rewritten to another strategy — a `policy` node names no
  provider or model to convert, and silently reinterpreting an admin's
  configuration is what this release stops doing.

  The admin UI previously mapped an unrecognised stored strategy onto `single`
  when opening the edit form, so saving any field — even the name — persisted a
  single-shaped rule over the original configuration, while the detail page
  still displayed `policy`. The form now refuses to load such a rule and says
  why.

- **`traffic_event_normalized` is dropped.** It held a second stored copy of a
  request's captured text; the normalized projection is recomputed at view time
  from the captured body instead. The erasure path is the reason — a second copy
  is a second thing a subject-erasure request can miss, and one surface is the
  stronger position. `normalizedScrubbed` stays in the DSAR response pinned at
  `0`, deprecated in the OpenAPI spec with a removal window rather than
  vanishing from a shipped contract. The deploy destroys the table and its rows;
  `docs/operators/ops/runbooks/prod-deploy-data-changes.md` carries the pre-drop
  dump, the ordering (binaries first, schema second) and what rollback can and
  cannot restore.

- The four per-package Dockerfiles
  (`packages/{nexus-hub,control-plane,ai-gateway,compliance-proxy}/Dockerfile`)
  are deleted. They were dev-grade (no Vectorscan build tag, no version
  stamp) and unreferenced; `docker/services/Dockerfile` supersedes them.

## [1.5.0] — 2026-07-28

### Added

- **A `storage.spill` runtime-introspection source on the compliance proxy and the AI Gateway**,
  reporting whether a spill backend exists, which one, where it stores, and whether that location is
  readable only from the one host — plus `residency`: object
  count, total bytes and the oldest/newest object timestamps of the spill backend, measured when the
  introspection source is read rather than at boot. Bounded by the backend's own scan limit
  (`localfs` 50 000 objects, `s3` 10 list pages) and by a 2 s deadline; `truncated` + `scanLimit` say
  so when a bound bites, and a failed measurement omits `residency` entirely rather than reporting
  zeros — "we could not look" and "the store is empty" must not render identically.

### Fixed

- **The audit trail no longer reports a compliance verdict for responses no hook examined.** Six
  bumped-flow relay paths handed the audit emitter a fabricated `Approve` when there was nothing to
  report — no response pipeline bound, an unreadable body, the non-AI fast path, a pipeline that
  failed to build and was fail-open relayed, an SSE stream with no response stage, and an upstream
  that failed before a response existed. `response_hook_decision` therefore read `APPROVE` for
  traffic nothing had looked at, indistinguishable from traffic a hook really approved. Confirmed
  live: with every hook disabled the column read `APPROVE` with `response_hooks_pipeline` NULL; with
  the same build and hooks enabled it read `APPROVE` with a populated pipeline. The column is now
  NULL when no response hook ran — matching what the request stage already did, and what
  `sse_audit.go` documented in a comment directly above the line that fabricated the value. The
  storage gate still sees an explicit approve action, so body capture is unchanged. **Consumers must
  not read a null decision as approval**; the published `responseHookDecision` schema already
  allowed null and now says what null means.

- **The smoke's tool-coercion probes stopped false-reding on their own cache entry.** Two arms of the
  same model differ only in a request parameter while sending identical message content, and the L1
  cache-bypass nonce was scoped to the run — so the second arm was served the first arm's cached 200,
  which carries no `x-nexus-coerced` header because a cache hit never re-runs the coercion. The probe
  then reported "rule stale or report lost" against a gateway that had coerced correctly. Verified
  directly against the vendor: function tools with an absent `reasoning_effort` still 400 on
  `gpt-5.6-luna`, so the rule is live, and a uniquely-nonced request returns 200 carrying the label.
  The nonce is now per-arm.

- **An aborted ai-gateway smoke reports FAIL instead of PASS.** The run's verdict came only from
  recorded results, and a fatal abort merely printed its failure — so a prod run that died in P0
  preflight, having exercised no model at all, reported `Result: PASS — 0 failed` and exited 0. The
  smoke is a mandatory pre-"done" gate; one that cannot fail certifies rather than checks. Guarded by
  `check:smoke-harness` in CI.

- **Access-token revocation actually persists now.** `RevokedToken.targetJti` was typed `@db.Uuid`,
  but an access token's `jti` is 16 random bytes in base64url — never a UUID — so every insert was
  rejected by Postgres with SQLSTATE 22P02. The revoke endpoint follows RFC 7009 and returns 200
  regardless, logging the error, so **a revoked access token stayed valid until it expired** on every
  released version that had this column. The column is now `String?` (TEXT) with an index on it.
  **Migration:** `prisma db push` performs `ALTER COLUMN "targetJti" TYPE text`, which Postgres
  accepts without a `USING` clause and which cannot fail on existing rows (the column is empty
  wherever the bug applied). **Operators should assume any access token they revoked before this
  release was NOT revoked** and, if that matters, rotate the affected credentials or wait out the
  access-token TTL.
- **SSE traffic was being relayed uninspected, and now is not.** The compliance proxy's Type-B
  receiver for `streaming_compliance` applied the pushed (empty) payload instead of re-reading
  `system_metadata`, so boot installed the admin's configured mode and the first invalidation replaced
  it with the built-in `passthrough` about 70 ms later — after which no streamed response could be
  accumulated, redacted or blocked. The receiver now re-reads on an empty payload, and a DB error
  keeps the current policy rather than degrading it. **Operators should treat streamed-response
  compliance evidence from before this release as unreliable for any node that received a
  `streaming_compliance` invalidation.**
- **A `traffic_event` row is now written for a body-carrying request that PASSES THROUGH
  uninspected** (`request_hook_decision = "PATH_PASSTHROUGH"`). Previously such a request left no
  trace at all, which is the gap: an auditor could not distinguish "nothing was sent" from "something
  was sent and we chose not to look". Expect a step-change in row volume on hosts where passthrough
  rules cover chatty endpoints — the row is emitted only when the request could have carried content
  (a body-bearing method with a non-zero or chunked length), not for every GET.

- **The compliance proxy's audit-overflow logs are throttled.** Both the drop ERROR and the
  spool WARN fired once per event; with NATS unreachable at 1 000 rps that put ~1 000 ERROR lines per
  second onto the same disk the NDJSON spool needs, and the WARN flooded to report the healthy case
  where nothing is lost. Both now sample 1-in-2000 — matching the AI Gateway, so two services' logs
  sample alike — and each carries its running total; the Prometheus counters are unchanged and remain
  the authority on the rate.
- **The exemption store's shadow rebuild now takes the writer lock.** `Rebuild` published its
  snapshot without it while every other writer held it, so a revocation push landing inside
  `purgeExpired`'s read-modify-write window could be overwritten — resurrecting a revoked exemption
  and leaving the compliance pipeline bypassed for that source/host pair. `-race` cannot detect this
  class: the pointer swap is atomic, so it is a lost update rather than a data race.

- **The `policy.matcher` introspection source no longer reports a `scanBounded` field.** It claimed
  Vectorscan caps how much text a scan examines; it does not — `hs_scan` reads the whole segment, and
  the detection cap bounds a pattern's repeat. Since production runs the Vectorscan build, the field
  was a false claim exactly where it mattered, and it was false for the RE2 build too. What differs
  between the engines is passes, not coverage, which `singlePass` already reports.
- **The pprof boot line reports the endpoint's actual exposure.** It said "(loopback profiling)" for
  whatever address was configured, while `.env.example` recommends the wildcard `:6060` — an
  affirmative assurance that `/debug/pprof` was unreachable off-box while advertising the opposite. It
  now prints `exposure="loopback only"` or `exposure="REACHABLE OFF-HOST — bind 127.0.0.1 to restrict"`.
- **The compliance proxy's undecryptable-cached-cert warning names the right key.** Its remedy pointed
  at `CREDENTIAL_ENCRYPTION_KEY`, a different subsystem; that path's key is derived from the CA private
  key or from the cert-cache DEK in Redis. It now names both, and the case where two proxies sharing a
  Redis hold different DEKs under the same CA.

- **An agent upgraded in place no longer switches to a blocking audit-overflow mode.** `auditLossMode`
  had no entry in the agent's `applyDefaults`, so a config file that predates the key resolved through
  the shared `lossmode.Resolve("")` to the no-loss default (`spillblock`) and overrode the queue
  writer's deliberate `spill`. A no-loss mode blocks the emitting goroutine until the record is
  durable, and on the agent that goroutine is on the host's own outbound packet path — the one thing
  the macOS network-extension rule forbids. All three shipped templates already said `spill`, so only
  in-place upgrades were affected. **No operator action required**; an explicitly configured mode is
  still honoured.
- **A request refused because its compliance pipeline could not be BUILT no longer persists its raw
  body.** The empty-action rule (see the redact-gate change below) read `{Decision: RejectHard}` with
  no action as "no redaction demand", so the one request class the product knows it could not scan was
  the one whose unredacted body reached `traffic_event` — while an ordinary scanned block persisted
  nothing. `stageAction` now derives the action from the decision, which fixes every hand-built result
  literal at once rather than stamping each producer.

- **The agent now cross-compiles for Windows, and the gate enforces it.** `profiling`'s
  on-demand capture signal referenced `syscall.SIGUSR1` unconditionally, which does not exist on
  Windows, so `GOOS=windows` was warn-only in `check:agent-cross-build`. The signal moved behind a
  build tag: Unix keeps SIGUSR1, Windows reports no capture signal and says so once at startup
  rather than pretending dumps are armed. Live profiling on Windows goes through
  `NEXUS_PPROF_ADDR`, which is platform-neutral. The gate now fails on a broken Windows build
  instead of warning — a warn-only platform is one nobody notices breaking twice.

- **The compliance proxy's Redis certificate cache is now scoped to the CA that minted each
  entry.** The key was `nexus:proxy:cert:<hostname>`, so a CA rotation left every hostname's entry
  in place holding a leaf signed by the previous CA and a key encrypted under the previous DEK:
  one wasted round-trip, one decrypt failure and one alarming WARN per hostname before the miss
  path re-minted. The key is now `nexus:proxy:cert:<ca-fingerprint>:<hostname>`, which makes a
  rotation an ordinary cache miss; orphaned entries expire on their own TTL. **No migration is
  required** — certificates are re-mintable, so the worst case is one extra mint per hostname on
  first use after upgrade.
- **A cached certificate that cannot be decrypted no longer reports Redis as unavailable.** Redis
  had answered correctly; the entry simply was not this process's. The handler logged
  "redis get failed" and set `redis_available = 0`, so a routine CA rotation raised a false
  availability alarm pointing at the wrong system. It is now logged as a key-material condition
  with a remedy, and the gauge stays at 1.

- **`localfs.Stat` now honours its context and bounds its walk.** It previously ignored `ctx` and
  walked the whole spill root, so it could not safely be called from an operator-facing surface —
  which is why `SpillStore.Stat()` had no production callers at all. The context is checked per
  ENTRY rather than per directory, so a single flat day-directory cannot outrun cancellation, and a
  cancelled scan returns both its partial numbers and `ctx.Err()`.
- **`s3.Stat` no longer reports a silent lower bound.** It already stopped after 10 list pages and
  returned the partial counts unlabelled, so a bucket with more objects reported a total that was
  not one. It now sets `Truncated`.

### Removed

- **`PATCH /api/admin/rule-pack-installs/{installId}/overrides` now rejects an out-of-enum
  `severityOverride` with 400 instead of accepting it**, and rule-pack `Import` returns 400 rather than
  500 for a malformed pack. The previously shipped OpenAPI example used `severityOverride: high`, which
  is not one of `hard|soft|warn` — a client that copied it, or automation that has been sending an
  out-of-enum value, will now get `validation_failed` where it got 200. Existing `rule_override` rows
  holding out-of-enum values are left in place and remain non-enforcing; they are not migrated.
- **`shared/transport/streaming/policy` takes an exported-signature break**: `OverrideFromColumns`
  loses its eighth parameter and `Policy`/`Override` lose `RawSpillEnabled`. The package is compiled
  into the released agent binary, so an out-of-tree importer must drop the argument and the field —
  the same shape of change as the `WithPreSpillNormalize` removal noted below.

- **The `raw_body_spill_enabled` admin switch is gone** — from the streaming-compliance settings API
  (request and response), the admin UI, the per-host / per-provider override plumbing in
  `shared/transport/streaming/policy`, the Hub shadow projection, the agent shadow DTO and both
  services' SQL reads. It never did anything: no production code read the resolved field, and whether
  a body spills is decided solely by whether the node has a spill backend configured and by the
  inline-vs-spill threshold (`spillstore/emit.go`). Every value of the two per-row columns — in seed
  and in the live database — was NULL. (An earlier draft of this note said "every value in seed",
  which was wrong: the seeded global `streaming_compliance.config` blob shipped
  `raw_body_spill_enabled: true`, i.e. it explicitly enabled a switch nothing read. That key is
  removed from the fixture in this release.)
  **Migration — this is a BREAKING change to the admin API and to the database schema.** An old
  client that still *sends* `raw_body_spill_enabled` on
  `PUT /api/admin/settings/streaming-compliance` is unaffected: unknown fields are ignored, as the
  field's value already was. But the **`GET` response no longer returns it**, and it was declared
  `required` on that response in 1.4 — a client generated from the 1.4 spec that validates required
  fields will fail to deserialize the response and must be regenerated. A node's spill posture is
  reported by the `storage.spill` runtime-introspection source instead. The two database columns
  (`interception_domain.raw_body_spill_enabled`, `Provider.raw_body_spill_enabled`) are **dropped**;
  `prisma db push` issues the `ALTER TABLE … DROP COLUMN`. The drop is verified lossless — every row
  in seed and in the live database held NULL — but it is not reversible, so **take a backup before
  applying** and roll the schema change and the binaries together: a previous-release binary still
  running against the migrated database would fail its `SELECT`, which lists the dropped column. A stored
  streaming-policy blob that still carries the key decodes normally (the key is ignored, not
  rejected; a test pins this), so no config rewrite is required.

### Performance

- **Rule-pack content scanning on the pure-Go (RE2) build now fans its per-pattern scans out across
  cores.** With this deployment's 423 seeded rules, a 400 KB request body cost 4.31 s in the matcher
  and added ~3 s of latency to a live request through the compliance proxy; the scans are
  independent, so parallelising them takes that to 0.73 s and ~0.4 s respectively with identical
  results. Gated on measured thresholds (≥4 patterns and ≥2048 byte×pattern units) so small inputs
  keep the sequential path, and neutral at CPU saturation by construction. A union-alternation
  prefilter was benchmarked first and rejected — on Go's `regexp` it is 13–44% slower than the
  per-pattern loop and 2.4× slower once anything matches. No configuration, no behaviour change:
  sequential and parallel scans produce the same hits in the same order, pinned by a differential
  test and by a `scan-scale` regression arm that sends the same sensitive value in a tiny body and
  in a 200 KB one.

### Added

- **`policy.matcher` runtime-introspection source on the compliance proxy and the AI Gateway**, plus
  a boot log line, reporting which content-scanning engine the binary compiled in (`vectorscan` via
  build tag, or the pure-Go RE2 fallback) with its `singlePass` / `scanBounded` properties and the
  operational consequence. The engine is a build-tag choice with an order-of-magnitude cost
  difference on large bodies and was previously answerable only by inspecting the build — a
  cross-compiled binary that loses its cgo engine keeps producing correct verdicts, slowly, with no
  runtime signal.

### Changed

- **Prometheus counter `nexus_ai_gateway_generative_cap_shed_total{kind}` renamed to
  `nexus_admission_generative_cap_shed_total{kind}`.** The original carried the *service* in the
  metric name, which `prometheus-naming-architecture.md` §1 forbids: the service belongs in the
  scrape config's `job` label, and one subsystem metric emitted by two services must be a single
  series name. The counter now builds its name from `Namespace: nexus` + `Subsystem: admission`,
  the same subsystem as `nexus_admission_shed_total` (the pre-auth shed it mirrors). Semantics,
  labels and increment sites are unchanged.
  **Migration:** dashboards, alert rules or recording rules referencing the old series must be
  updated to the new name; there is no dual-emission window. Nothing in this repository referenced
  it outside documentation. To keep continuity across the rename in a Prometheus query, use
  `nexus_admission_generative_cap_shed_total or nexus_ai_gateway_generative_cap_shed_total` for one
  retention period.

- **A cache lookup skipped because routing produced no target now reports
  `no_targets`, not `disabled`** (`traffic_event.gateway_cache_skip_reason`, and the `result` label
  on `nexus_cache_lookups_total`). Both conditions previously stamped `disabled`, so a config
  posture ("no cache tier is on") and a routing outcome ("the tiers are on, but there was nothing to
  key an entry against") were indistinguishable, and pointed an operator at the wrong remedy. When
  both hold, `disabled` still wins.
  **Migration:** additive — a new value in an existing text column and an existing metric label.
  Queries or dashboards that treated `gateway_cache_skip_reason = 'disabled'` as "caching is off"
  become *more* accurate; any that counted it as "cache not consulted for any reason" should now
  match `('disabled','no_targets')`.

- **An oversize audit body is now bounded when no spill backend is configured
  (`packages/shared/storage/spillstore`, all three data-plane services).** Previously, with
  `spill.enabled: false` — which is what every shipped `*.config.yaml` sets — a body at or above
  `payloadCapture.maxInlineBodyBytes` was stored **whole** inline on `traffic_event_payload` and
  published whole on the MQ message. A 10 MiB body was kept intact under a setting named
  `MaxInlineBodyBytes: 262144`. It is now truncated to that threshold with `truncated = true`, while
  `sizeBytes` continues to report the **real** pre-truncation size and the node logs a WARN naming
  the absent backend and the remedy.
  **Migration:** deployments that rely on whole oversize bodies must configure a spill backend
  (`spill.enabled: true`) — the setting whose absence this path is. **On a multi-node deployment that
  means `s3`, not `localfs`:** a per-node localfs root is readable only by the process that wrote it,
  so following this advice with `localfs` on each node leaves every spilled body permanently
  unreadable from the Control Plane (the read path reports `not_found_host_local`) — worse than the
  truncation it was meant to fix. Use `localfs` only on a single node, or where every node mounts the
  same root. Rows written
  before this change are unaffected; only newly captured oversize bodies are truncated, and they say
  so via `truncated`. No schema, column or wire-format change.

### Removed

- **`AuditEmitter.WithPreSpillNormalize` (Go API, `packages/shared/policy/pipeline`).** Removed
  with maintainer approval. The method opted an emitter into re-attaching up to 2 MiB of a
  *spilled* body in memory so a writer's flush-time normalize pass could read the content without
  a spill-store fetch. Nothing in the repository ever called it, and the `applyNormalize` its own
  doc named as the sole consumer does not exist — so the retention was memory cost with no reader.
  Recorded here because it removes an exported symbol from `packages/shared`, which ships inside
  the released Agent binary: an out-of-tree importer that called it must simply drop the call.
  **No behaviour, wire format, database column or persisted shape changes.** `InlineBytes` was
  already excluded from a spill container's wire form (`Body.MarshalJSON` switches on `Kind`), so
  audit rows are byte-identical before and after. A spilled body is now unconditionally ref-only,
  which is what every caller already got. Reinstating the optimization requires the consuming
  normalizer to exist first.

## [1.4.0] — 2026-07-18

Hardening release on top of the 1.3.0 multimodal launch: the jsonb
shape-contract bug class is closed at every admin write boundary (a wrong
shape now 400s instead of persisting and failing cryptically downstream),
captured-traffic audit records survive mistyped scalars, the STT prompt field
joins the request-stage compliance pipeline with redact-re-emit, and the
Traffic UI gains the modality column/filter plus an inline artifact viewer.

### Added

- **STT prompt field is now compliance-scanned at request time.** The
  `prompt` form field of `/v1/audio/transcriptions` and `/v1/audio/translations`
  — the one request-side text leaf of the multipart STT request — now runs
  the same request-stage hook pipeline as chat: a hard-blocking match returns
  403, a redacting match rewrites the prompt in place so the sanitized value
  is what reaches the provider, and a clean scan stamps
  `compliance_coverage = prompt-only` (previously always `none`). Requests
  without a prompt are unchanged.

### Fixed

- **Captured-traffic audit records survive one mistyped scalar.** The
  view-time normalize codecs used to discard the ENTIRE request/response on
  any whole-struct decode failure — captured third-party traffic carrying a
  single mistyped optional scalar (e.g. `"temperature":"0.7"` as a string)
  produced a "partial" normalized record with zero message content, silently
  erasing the prompt text from the audit record. All codecs now decode
  leniently: a mistyped field is dropped, everything decodable (especially
  the messages) is preserved.
- **Routing rule `config` validated against the gateway's full strategy
  shape.** The write-time check previously validated only the top level of
  the strategy tree, so a NESTED element with a wrong-typed field (e.g. a
  weighted target with `"weight":"5"`) passed the admin API, was broadcast
  fleet-wide, and then failed the resolver's parse on every request routed
  by that rule. The validator now mirrors the resolver's recursive node
  shape (including weighted/conditional/ab/latency sub-structures), rejects
  unknown nested node types, and bounds the tree to the depth the gateway
  actually evaluates.
- **Hook config blob shape validated at write time.** A hook whose `config`
  was not a JSON object froze hook-config propagation fleet-wide (every
  reload kept the last-good snapshot) and, on the next AI Gateway /
  compliance-proxy restart, silently started the compliance pipeline with an
  EMPTY hook config — a fail-open bypass visible only as one warn log. The
  admin API now rejects a non-object `config` with `400 validation_error` on
  create and update.
- **Routing rule `fallbackChain` shape validated at write time.** A chain
  written as bare model strings decoded to zero recovery targets at the
  gateway (best-effort decode), silently losing all failover coverage for
  that rule. The admin API now requires an array of `{providerId, modelId}`
  objects and rejects other shapes with `400 fallback_chain_invalid`.
- **Interception domain `adapterConfig` shape validated at write time.** A
  non-object value made the traffic snapshot skip the whole domain with only
  a warn log — traffic for its host pattern was silently no longer
  intercepted. The admin API now rejects a non-object `adapterConfig` with
  `400 validation_error` on create and update.
- **Malformed IAM policy documents are no longer dropped silently.** A policy
  row whose document fails to parse is still skipped from the effective set
  (keeping authz alive for the principal's other policies), but the drop is
  now logged at ERROR with the policy id/name/source so operators see the
  distortion instead of debugging phantom authz decisions.
- **Virtual key `allowedModels` shape validated at write time.** A virtual key
  whose `allowedModels` was set to anything other than an array of
  `{providerId, modelId}` objects (for example an array of bare model-code
  strings) was accepted by the admin/user API and then rejected by the AI
  Gateway on every request with an opaque decoder error — a 401 on a key that
  looked valid. The Control Plane now validates the shape on create and update
  and returns a clear `400 validation_error`, so a malformed allowlist can no
  longer be persisted. The OpenAPI spec and examples now document the
  `{providerId, modelId}` object shape (previously they showed bare strings).

### Multimodal follow-ups

- **Modality-scoped routing hardening.** Speech models are now typed precisely
  in the model catalog (`tts` / `stt` / `realtime` instead of the coarse
  `audio`; Sora → `video`), closing a routing footgun: `model: auto` on a TTS
  endpoint could previously pick a non-TTS audio model. The modality guard
  already dual-accepts the coarse and precise types, so this is a smooth
  migration. The provider create/edit UI and model-discovery heuristic gain the
  precise sub-types; the `Model.type` API enum widens (additive).
- **Multimodal normalized text in the Traffic drawer.** New view-time codecs
  render the image prompt + revised prompt, the TTS input, and the STT
  transcript as messages the same way chat is shown. Image responses now
  summarize the artifact by size/mime instead of inlining multi-MB base64, and
  a TTS binary audio response is no longer misdetected as an OpenAI-chat
  `partial` parse error. **Behavior change for interception deployments:** the
  same codecs feed compliance-proxy / agent hook scanning, so intercepted
  provider-direct image/TTS prompts that were previously unscanned can now match
  admin-configured content hooks — an intercepted image/TTS prompt matching a
  redact rule on a wire with no in-place span mapping hard-blocks (fail-closed),
  consistent with the existing multimodal redaction posture. STT and video
  submit **response** bodies are captured under the existing payload-capture
  toggle (the multipart audio/video request bytes remain fingerprint-only).

## [1.3.0] — 2026-07-17

Multimodal release: the gateway extends beyond chat and embeddings to image
generation, text-to-speech, speech-to-text, video (async), a standalone
compliance-guardrail verdict endpoint, and its first WebSocket surface —
realtime voice relay. Provider adapters move to the request-contract v3 model
(the codec is always in the request path, absorbing per-model wire quirks on
every ingress).

### Realtime voice — `GET /v1/realtime` WebSocket relay (P1 dark launch)

The gateway now relays the **OpenAI Realtime API** — its first WebSocket
surface. A server-side client opens a WebSocket to
`GET /v1/realtime?model=<model>` with a virtual-key bearer token; the gateway
runs its admission chain on the plain-HTTP upgrade, dials the resolved
provider (`wss://…/v1/realtime`, provider key injected, client credentials
never forwarded upstream), and relays both directions verbatim.

**Dark launch.** The realtime model is reachable only by a virtual key whose
`allowedModels` explicitly names it — an empty (unrestricted) list is NOT
entitled, because an unbounded voice session is the most expensive billable
surface. Entitle a **dedicated** realtime virtual key. Built-in bounds (not
admin knobs): a per-VK concurrent-session cap (default 2, env-overridable), a
per-WS-frame ceiling, a 65-minute session guard, and a 60-second by-hash VK
recheck that severs a revoked key mid-session. Metering emits one
`traffic_event` row per in-band `response.done` (priced across the six
text/audio/cached components) plus a $0 session row; per response the gateway
reconciles quota and severs on a crossed reject/downgrade cost cap. **P1 does
no content scanning** (`compliance_coverage = none`); transcript-level
compliance is a later phase. Per-minute-billed models, browser/ephemeral-token
clients, and Azure/Gemini realtime are out of P1 scope. See
`docs/users/api/openapi/e88-s7-realtime.yaml`.

### Realtime pricing widening — audio-rate Model columns + model-type vocabulary

The `Model` catalog gains three additive nullable pricing columns —
`audioInputPricePerMillion`, `audioOutputPricePerMillion`, and
`cachedAudioInputReadPricePerMillion` — so **realtime models can be priced
per component**: one realtime response bills text and audio tokens
simultaneously at different rates, and the existing single input/output pair
cannot express that. The base columns carry the text rates; the cached-audio
column follows the shipped cached-read contract (NULL = no discount, falls
back to `audioInputPricePerMillion`). The admin API (model create/update,
provider create with inline models), the Control Plane pricing drawer (a
six-field per-component layout for `type=realtime`), and the
`sync-provider-pricing` skill all carry the new rates end-to-end.

The model `type` vocabulary widens to
`{chat, embedding, image, audio, rerank, video, realtime}` across the admin
validation, OpenAPI specs, and CP-UI type options — this also fixes a live
drift where the UI offered `rerank` but the admin API rejected it with a 400.

The model-`type` validation, previously enforced only on model UPDATE, is now
also enforced on the CREATE paths (`POST /api/admin/providers` inline models,
`POST /api/admin/providers/{id}/models`): an out-of-vocabulary type is
rejected 400 at create instead of persisting silently. The retired
`completion` option is removed from the provider-creation wizard (it is not a
catalog model type; no seed model used it). Operators whose automation created
models with a non-standard `type` string must use one of the seven valid
values.

Additive contract: new nullable columns and new enum values only — no
migration needed beyond `prisma db push`; existing rows and API clients are
unaffected.

### Video ingress — `POST /v1/videos` + poll / download / delete (async)

The gateway now serves **async video generation** — its first async endpoint
kind. `POST /v1/videos` submits a `multipart/form-data` job and returns a video
**job object** (not a completion); the client polls `GET /v1/videos/{id}`,
downloads with `GET /v1/videos/{id}/content`, and cancels with
`DELETE /v1/videos/{id}`. These are **parallel handlers** (`ServeVideo*`), not
the small-JSON `ServeProxy` pipeline: the submit is a large multipart upload
and the follow-ups are governed passthroughs keyed by a new **gateway-owned
correlation store** (`gateway_async_job` — the gateway's first runtime-writable
table). The row binds the provider job id → virtual key → submit-time
credential, so every follow-up is authz'd on the row (unknown / foreign id →
404 non-disclosure, never forwarded upstream) and reaches the same provider
account that owns the job.

The submit is governed like image generation: VK auth, per-VK rate limit, a
per-VK **non-terminal-jobs render cap** (bounds concurrent paid renders, not
just in-flight HTTP requests), the request-side compliance pipeline over the
`prompt` (a content match hard-blocks `403 GENERATIVE_PROMPT_BLOCKED` even
observe-only — the video output is uninspectable, so the prompt is the only
control point), and an advisory cost check. **Cost is one row per job**: the
submit row stamps the requested-seconds × per-second-price estimate
(estimate-as-floor); the poll that first observes completion reconciles live
quota with the same seconds × price value (never a provider-reported figure);
poll / content / delete rows stamp $0. Under an enforced cost quota an unpriced
routed model fails closed (`503 QUOTA_MODEL_UNPRICED`).

The artifact download streams through a **sha256/size fingerprint tee** with a
1 GiB ceiling (declared-oversize → 502; mid-stream overflow → connection abort,
never a silent short file), a Content-Type allowlist
(`video/mp4`/`image/jpeg`/`image/png`/`image/webp`), and `nosniff` +
`attachment`. No artifact bytes are stored (provider custody). The generated
video is not content-scanned (`compliance_coverage = none`) — the tee is the
named remediation mount point.

**Cross-shape (Veo):** when routing resolves a Google Gemini provider the
codec translates OpenAI `/v1/videos` ↔ Veo `:predictLongRunning` +
long-running operations — allow-list-only, lossy `size` → `aspectRatio` +
`resolution` (`X-Nexus-Coerced`), provider errors normalized to the OpenAI
envelope, the canonical job id `veo_`+base64url(operation name), and the
download dereferences the provider artifact URI under an SSRF + host-allow-list
guard (the one provider-URL-deref in the product). Per-leg differences (Veo:
`video`-variant only, best-effort local delete that does not stop the
still-billed render) are documented. A **retention sweep** (gateway-side,
hourly) marks stale rows expired (terminal > 30 d, non-terminal > 7 d), served
as `410 Gone`. `GET /v1/videos` (list) and `remix` / `edits` / `extensions` /
`characters` are deliberately unserved with an explicit OpenAI-shaped 404
envelope.

Additive: new routes, new `gateway_async_job` table (`db push`),
`BillableUnits.VideoSeconds` + `videoCostFormula`, `CostEstimate.EstimatedUsd`
and `ResolveHints.CredentialID` additive fields, the video generative-caps row
raised to 16 MiB. No shipped contract changes. Veo catalog price rows are a
deploy dependency (an unpriced Veo model fail-closes under a cost quota).

### Guardrail ingress — `POST /v1/guardrail` (standalone compliance verdict)

The gateway now exposes its compliance pipeline as a **standalone verdict API**:
a caller submits text and receives an `allow` / `block` / `redact` verdict from
the SAME hook pipeline the inline path runs (rule-pack + PII redaction + the
AI-Guard judge) — WITHOUT relaying an LLM completion. This is the ApplyGuardrail
/ Content-Safety category, but backed by the deployment's already-configured
policy (same policy, two entry points, one audit trail), reached with a virtual
key like any other `/v1/*` endpoint. It is an in-deployment capability, NOT a
SaaS. Like STT it is a **parallel handler** (`ServeGuardrail`), not `ServeProxy`.

The endpoint always returns HTTP 200 with the verdict — a `block`/`redact`
disposition is data in `action`, not an HTTP error. The verdict carries a
`coverage` honesty signal (`full`/`degraded`/`none` — a judge that fails open
never masquerades as a clean scan), a per-policy `assessments[]` breakdown,
rule-pack/PII `redactions[]` (AI-Guard judge spans stay audit-only), and a
`blocking` block that exposes category/severity/labels but never pack/rule IDs.
The raw evaluated text is never persisted. v1 bounds judge-budget abuse with
per-VK concurrency + RPM + a 1 MiB body cap; a hard per-VK spend ceiling and
per-call cost in the response are a documented fast-follow.

**Added**

- `POST /v1/guardrail` VK-authed endpoint (`e90-s1`); `EndpointKindGuardrail`
  typology + `endpoint_type=guardrail` audit vocabulary; a `guardrail`
  generative-caps concurrency row. Fully additive — no existing contract
  changes, no migration. OpenAPI: `docs/users/api/openapi/e90-s1-guardrail.yaml`.

### Speech-to-text (STT) ingress — `/v1/audio/transcriptions` + `/v1/audio/translations` (v1a)

The gateway now serves the OpenAI-shape speech-to-text routes through a
**parallel streaming-proxy handler** (`ServeSTT`), NOT the small-JSON
`ServeProxy` pipeline: an STT request is a large binary multipart stream —
one-shot, un-re-readable — that `ServeProxy`'s byte-slice executor, response
cache, text-scanning hook pipeline, and canonical/codec bridge cannot serve
without polluting the hot core (e88-s5). v1a is competitor-parity passthrough:
the transcript forwards unredacted (`compliance_coverage = none`); transcript
redaction is the v1b differentiator.

**Added**

- `internal/ingress/proxy/stt_handler.go`: the `ServeSTT` handler — VK auth →
  per-VK RPM → per-VK generative-caps concurrency (shared `Handler.genConcurrency`
  instance) → bounded multipart parse → single-target resolve → native multipart
  forward → meter → panic-safe audit tail. Reuses the shared cross-cutting subset
  (`authenticate` / `checkRateLimit` / router + resolver / cost estimator / audit
  writer) and touches none of `ServeProxy`'s internals; `provcore.Request`, the
  executor, and `spec_adapter` are unchanged.
- Two routes registered: `POST /v1/audio/transcriptions` and
  `/v1/audio/translations` (both STT-kind, one wire shape; the ingress path is
  forwarded verbatim to the upstream so the two are distinguished).
- Additive reuse seams: `forwardheader.Apply` (request-side allowlist as a free
  function) and an exported `specAdapter.ApplyAuth` (optional interface, off the
  `Adapter` interface so no test double grows it) so the STT forward single-sources
  provider auth + header filtering from the chat path. `proxy.Deps.Resolver`
  exposes the executor's `provtarget.Resolver` to the STT path.

**Bounds & metering**

- `http.MaxBytesReader` caps the upload mid-stream at the STT generative-caps
  ceiling (~26 MiB → **413** before full drain, defending chunked / lying-
  Content-Length uploads); part-count / single-file-part / per-field-size bounds
  reject multipart bombs; a duplicated governance field (`model` /
  `response_format`) is rejected **400** (R-5).
- Only `json` / `verbose_json` / `text` response formats are served;
  `srt` / `vtt` return an explicit **400** (deferred), and a streamed
  transcription (`stream=true`, the transcribe models' separate SSE trigger)
  is likewise rejected **400** (v1a buffers the response).
- Metering: provider usage tokens win, else `AudioSeconds` from the response
  `duration` (verbose_json); neither present prices $0 with a deduped WARN — the
  audio byte-count is never priced as seconds. Input audio is fingerprinted
  `{sha256, sizeBytes, mime}` (reference only — the bytes never enter the audit
  body pool, R-7).
- Single resolved target, **no failover in v1a** (a deliberate simplification —
  the bounded-buffer body is re-readable, but a wedged-credential retry also
  wants the executor's circuit-breaker feedback; deferred, signed residual).

### Built-in generative endpoint caps (per-VK concurrency + request size)

Expensive generative endpoints now carry built-in per-VK caps (e88 NFR-4) —
no admin configuration — closing the billing-DoS surface where a single leaked
or abusive virtual key could open unbounded concurrent per-call-priced requests.

**Added**

- `internal/policy/generativecaps`: a registry of built-in per-endpoint-kind
  caps (`image_generation` 4 concurrent / 256 KiB, `tts` 8 / 256 KiB,
  `video_generation` 2 / 256 KiB), env-overridable via
  `AI_GATEWAY_GENERATIVE_CAP_<KIND>_CONCURRENCY` / `_MAX_BYTES`; and a lock-free
  per-(kind, VK) concurrency counter (the admission-gate atomic pattern).
- Admission-stage enforcement: an over-cap generative request returns
  **429 `GENERATIVE_CONCURRENCY_LIMIT`** with `Retry-After` in the caller's
  ingress error shape and an attributable `traffic_event` row (post-auth, VK
  known); the per-kind body ceiling returns 413 (tighter than the global cap).
  New Prometheus counter `nexus_ai_gateway_generative_cap_shed_total{kind}`.
  The slot release is defer-covered (finalizeAudit), so it returns on success,
  error, and panic alike. Non-generative traffic is never counted.
- The realtime spike's P1 "built-in per-VK concurrent-session cap" is this same
  registry with a future `realtime` row.

### Cross-shape image codec: OpenAI images → Gemini `:generateContent`

`POST /v1/images/generations` can now route to Gemini image models
(Nano Banana — `gemini-2.5-flash-image` etc., which have no dedicated
image endpoint): the gateway translates the OpenAI images canonical to
`:generateContent` + `responseModalities:["IMAGE"]` and reshapes the
response back (`data[].b64_json`). The literal OpenAI target keeps its
native passthrough (byte-unchanged); every other leg — including
wire-adjacent OpenAI-family siblings such as Azure — is independently
demand-gated and not opened in this slice.

**Added**

- Gemini image leg: target-side wire shape, Gemini codec image
  encode/decode branches, canonical-bridge image methods
  (`IngressImagesToCanonical` / `ImagesWireShapeForTarget` /
  `IngressImagesToWire`), routing gate, prepare-stage + executor
  dispatch arms.
- Per-parameter caller contract (documented in `ingress-api.md`):
  closed allow-list on the Gemini leg — `size` maps to `aspectRatio`
  over the documented OpenAI sizes (lossy, marker recorded on
  `X-Nexus-Coerced`), `quality`/`style`/`user` drop with value-free
  markers, `response_format:url` and absent both coerce to `b64_json`
  with markers, `n` bounded 1–4 riding `candidateCount`; out-of-schema
  fields (`tools`, `systemInstruction`, `safetySettings`, `nexus.*`,
  gpt-image-1-only params) are rejected 400 and never reach the wire.
- Provider-safety blocks surface as OpenAI-shaped content-policy 400s
  that never retry or fail over; an image-less upstream reply is a 502 —
  never a 200 with empty `data[]`.

**Changed**

- The adapter dispatcher now propagates a structured `*ProviderError`
  returned by a codec's `DecodeResponse` verbatim (previously every
  decode error was flattened to a failover-eligible 502). No shipped
  codec returned one before this change — behavior-neutral for
  existing legs.

**Operational note (pricing)**

- Token-usage image models (Gemini Nano Banana, `gpt-image-1`) must be
  priced **per 1M tokens** (usage tokens always win in the image cost
  formula); per-image rates are only for usage-less models (`dall-e-*`).
  A token-usage model configured per-image silently misprices — see
  `cost-estimation-architecture.md`.

### Multimodal ingress: image generation + TTS routes (native passthrough)

The AI Gateway now serves two multimodal data-plane routes:
`POST /v1/images/generations` (OpenAI Images) and `POST /v1/audio/speech`
(OpenAI TTS), as OpenAI-shape native passthrough through the standard
ServeProxy pipeline (VK auth, per-VK rate limit, quota, kill-switch,
routing, alias → provider-model rewrite).

**Added**

- Route registrations + OpenAI-compat transport paths for the images /
  audio-speech / audio-transcriptions wire shapes. The multipart siblings
  (`/v1/images/edits|variations`, `/v1/audio/transcriptions|translations`)
  are NOT yet registered — they need multipart model extraction +
  ingress-path preservation and ship with that work.
- New `traffic_event.gateway_cache_skip_reason` value `modality_endpoint`
  (additive enum): image / TTS / STT requests skip the response cache at
  pre-lookup, endpoint-driven like `embeddings_endpoint` — generative
  variety is the product; no per-modality cache knob is added.
- Multimodal prompts are now scanned by the hook pipeline: gateway-local
  extraction feeds the image `prompt` / TTS `input` text (string **or
  array-of-strings** — no bare-string-check bypass) to the rule-pack engine
  as ordinary text blocks (the shared traffic adapters are untouched — they
  also run on interception paths, whose extension is gated on the NE
  fail-open review). Interim redaction posture is fail-closed: a redact hook
  firing on a multimodal prompt rejects the request (403) rather than
  forwarding it unredacted, because the adapter cannot yet reverse-encode a
  redacted prompt onto the images/speech wire; hooks configured to block
  behave exactly as on chat.
- Multimodal routes are forced non-stream in this slice (a client
  `stream: true` is ignored, body still forwarded verbatim) so cost metering
  and the artifact fingerprint — both on the non-stream response path —
  always run instead of being silently skipped.
- `compliance_coverage` is honest: `prompt-only` is stamped ONLY when a
  content-scanning hook actually evaluated the prompt (a metadata-only
  pipeline of rate-limit / IP / size hooks, an unscannable prompt slot, or
  emergency hook-bypass all stamp `none`) — the badge never claims a scan
  that did not happen.
- TTS character count for cost is read from the forwarded request body, not
  the audit-capture copy, so TTS is priced correctly even when request-body
  storage is disabled (the privacy-conscious default). A multimodal 2xx that
  yields no billable units logs a deduped `underivable-units` WARN instead of
  silently pricing at $0. Image artifact MIME is sniffed from the decoded
  bytes (png/jpeg/webp/gif), not hardcoded.

### Multimodal audit stamps: artifact fingerprint + compliance coverage

Two additive, non-PII `traffic_event` columns (versioned contract change;
`prisma db push` applies them):

**Added**

- `traffic_event.artifact_refs` — JSON-encoded array of artifact
  references for multimodal responses: `[{"sha256","sizeBytes","mime"}]`
  for byte-bearing artifacts (inline `b64_json` images are fingerprinted
  over the DECODED artifact bytes; TTS audio over the response body),
  `[{"url"}]` for URL-return images (reference only — the gateway never
  dereferences the URL and no content hash exists in that mode). NULL for
  non-multimodal traffic.
- `traffic_event.compliance_coverage` — request-time record of what
  compliance scanning actually ran on a multimodal request
  (`prompt-only` / `none`); empty for chat/embeddings (no claim). Stamped
  at request time because a view-time recompute from current config would
  misreport history. Feeds the per-modality coverage badge.
- Binwire field-ids 105 (`artifactRefs`) / 106 (`complianceCoverage`) —
  append-only registry; same deploy-order note as 103/104 (schema → Hub →
  producers).
- The multimodal cost formulas now receive real units at the cost site:
  image count from the response `data[]` length, TTS characters from the
  forwarded `input` (rune count). Without this stamp the per-kind formulas
  would have priced every multimodal request at $0.

### Multimodal cost metering: image / TTS / STT priced by their own units

The cost estimator's formula registry now prices the three REST multimodal
endpoint kinds instead of silently falling back to the chat token formula.

**Added**

- `estimator.BillableUnits` gains `Images`, `AudioSeconds`, `InputChars` —
  each consumed by a newly registered per-kind cost formula
  (`image_generation`, `tts`, `stt`). Pricing semantic: a model's
  `InputUsdPerM` is *USD per million billable input units*, where the unit is
  the modality's own — tokens for token-usage models, images / characters /
  audio-seconds for per-unit-priced models (e.g. dall-e-3 standard at
  $0.04/image → `InputUsdPerM = 40000`). Per-size / per-quality image tiers
  are represented as separate catalog model entries, not a pricing-schema
  extension.
- Dispatch rule inside each modality formula: provider-reported usage
  tokens win when present (authoritative for token-priced models such as
  gpt-image-1); the modality unit is the fallback. Zero units → zero cost;
  the stamping site owns the underivable-units WARN.

**Migration note** — internal `estimator` registry/struct change; no DB or
wire contract is affected. Deployments that previously saw
`image_generation` / `tts` / `stt` traffic priced through the chat formula
(with the one-time WARN) will now see correct per-unit pricing once those
routes land; no operator action is required beyond configuring model prices
in the catalog with the per-unit semantic above.

### Fixed: provider request-contract v3 — per-model wire quirks are absorbed on every ingress

The codec is now always in the request path (two entry points: cross-format
`EncodeRequest` and native-leg `RewriteNative`); passthrough skips only the
canonical round-trip, never the codec. Per-model wire quirks live in the codec
that talks to that wire, so a request coerces identically whether it arrives on
`/v1/chat/completions`, `/v1/messages`, `/v1beta`, or `/v1/responses` — the
transitional dispatch-level rewrite callback is deleted. Caller-visible via the
`x-nexus-coerced` response header.

Upstream 400s turned into a gateway coerce (all verified live on prod):

- Fixed-temperature Moonshot models (`kimi-k2.7-code`, `-highspeed`) strip
  `temperature`/`top_p` on BOTH the native chat leg and a `/v1/messages`
  cross-format leg (the latter previously 400'd `invalid temperature`).
- DeepSeek thinking models (`deepseek-reasoner*`, `deepseek-v4-pro*`) strip a
  forced `tool_choice` and back-fill a missing `reasoning_content` on replayed
  tool-call histories (previously 400'd `reasoning_content … must be passed
  back`).
- Newest-generation Claude models (Opus 4.7+, `claude-fable-5`, `claude-sonnet-5`)
  strip the now-rejected `temperature`/`top_p`/`top_k` and clamp an over-ceiling
  `max_tokens` on the native `/v1/messages` leg too (owner-approved coerce-over-400;
  older families that still accept the params are untouched).
- Assistant chain-of-thought survives the ingress→canonical→wire round-trip
  (`reasoning_content` as the L2 universal field plus a per-block Anthropic
  signature carrier that is stripped before any non-Anthropic upstream).

Also fixed: a `/v1/responses` mixed-target-list failover posting the verbatim
Responses body to the chat URL; a codec's typed error surviving the cache-prep,
adapter, and failover stations instead of flattening to a generic 400; and the
dead `EncodeResult.Headers` channel removed.

## [1.2.0] — 2026-07-17

### Added: end-user and session attribution on gateway traffic (`traffic_event.end_user_id`, `.session_id`)

Callers can now tag each request with THEIR user's identifier and THEIR
session/conversation identifier, and every gateway traffic row carries both,
so an external system can join Nexus traffic (cost, tokens, latency,
outcomes) to its own user table per end user — and group it per
conversation: cost per thread, replay of a misbehaving dialogue. Together
with `X-Request-Id` this completes the caller-side correlation hierarchy:
user → session → request.

The session tag is declared via the `X-Nexus-Session-Id` request header
(header-only — chat protocols carry no reliable native session field). The
end-user tag has three carriers, first match wins:

- `X-Nexus-End-User-Id` request header — works on every ingress.
- The OpenAI shape's top-level `user` field (or its successor
  `safety_identifier`) — anyone already sending it gets attribution with no
  code change.
- The Anthropic shape's `metadata.user_id` — same, no code change.

The value is an opaque correlation tag scoped to the calling virtual key:
the gateway never validates it, never resolves it against Nexus users, and
never feeds it into quota, routing, or IAM. It is trimmed and capped at 256
bytes. Rows from the compliance proxy and agents carry NULL. It is stored
verbatim and is NOT covered by body redaction — send opaque ids, not
emails.

- **Deploy order matters: schema, then Hub, then gateways.** The gateway
  starts emitting the new wire fields as soon as any caller's traffic
  carries an OpenAI `user` field — that is existing traffic, not opt-in —
  and a Hub that predates the fields treats the whole frame as a poison
  record on its DB-writer path: logged, acknowledged, **dropped
  permanently**. NATS only buffers while the Hub is down; an old Hub
  actively consuming loses those rows for good, so never restart gateways
  onto a Hub that has not been upgraded first. New nullable columns +
  indexes on `traffic_event`; on a large table, create the indexes
  `CONCURRENTLY` rather than via bare `db push`, which locks writes for
  the build.
- **Reading it back:** query `traffic_event` directly — e.g.
  `SELECT date_trunc('day', timestamp), sum(estimated_cost_usd) FROM
  traffic_event WHERE end_user_id = '<your-user>' GROUP BY 1;` — both
  columns are indexed with `timestamp`. The admin Traffic API/UI does not
  surface them yet.

### Changed: `cors.allowedHeaders` now extends the built-in allowlist instead of replacing it

The gateway composes its CORS request allowlist itself: the headers its own
read sites depend on (virtual-key carriers, correlation ids, the cache
opt-out) plus everything the forward-header allowlist relays to providers
(`anthropic-beta`, `openai-organization`, …). The yaml key now adds extra
names on top of that set — it can no longer shrink it.

Previously the yaml value replaced the built-in list wholesale, and every
shipped config had drifted below what the gateway needed: a browser client
sending `x-api-key` (the Anthropic SDK's carrier), `x-goog-api-key`,
`api-key`, or `X-Nexus-No-Cache` was rejected at preflight before it could
even authenticate.

- **No action required.** Existing lists keep working — their entries are
  merged in. Entries that duplicated the built-ins are now redundant and can
  be deleted from your yaml.
- Also fixed in the same pass: CORS responses now always carry
  `Vary: Origin` (previously only allowed origins did, letting a shared
  cache mix per-origin copies), and a preflight from a disallowed origin no
  longer receives the allow-lists readout.

### Changed — deprecation, migration window open: the admin API key header moved into the `X-Nexus-*` namespace

`x-admin-key` is now `X-Nexus-Admin-Key`. The Control Plane accepts both, with
the canonical name taking precedence when a caller sends both; the nexus CLI now
sends the canonical name. Nothing breaks on upgrade: an older CLI keeps
authenticating against a newer Control Plane.

- **Action required** for any script or integration that calls the admin API
  directly: send `X-Nexus-Admin-Key`. The old name is read for now and will be
  removed in a future release.
- The one order that does not work is a **newer CLI against an older Control
  Plane** — that server has not learned the new name and answers 401. Upgrade
  the Control Plane first, which is the normal order anyway.
- If a WAF or edge proxy in front of the Control Plane inspects or strips
  the admin-key header, update its rule to cover **both** spellings — a
  rule keyed on the old name alone no longer sees every credential.

### Changed — deprecation, migration window open: the cache-bypass header dropped its service prefix

`x-nexus-aigw-no-cache` is now `X-Nexus-No-Cache`, matching every other
`X-Nexus-*` header — none of which carry a per-service segment. The caller
reference told clients to send the old name, so the gateway still reads it and
still bypasses the cache; both spellings work today.

- **Action required** before the old name is removed in a future release: send
  `X-Nexus-No-Cache`. This deprecation cannot fail loudly — after removal, a
  caller left on the old name is served from cache while believing it opted out,
  with no error to notice — so it is worth migrating while both names work.
- Browser callers need no preflight change: both names are in the CORS request
  allowlist for the duration of the window.

### Removed: the `x-nexus-aigw-body-format` request header

The header let a caller on an OpenAI-compat route declare that its body was
actually some other provider's shape. Nothing needs it: the route path decides
the ingress format, and every format already has a native route
(`/v1/messages`, `/v1beta/…`, `/openai/deployments/…`) that says the same thing
without a header. It had no documented callers, and it was the step that
unlocked the Gemini `?key=` URL credential carrier from an OpenAI route in the
SEC-M3-02 kill chain — removing it forecloses that whole class of "flip the
ingress format to inherit another format's carrier" escalation.

- **No action required** unless you were sending it, in which case call the
  native route for the format you are actually sending.

### Changed

- **Two metrics renamed to obey the naming rule, now enforced by a lint.**
  `prometheus-naming-architecture.md` §1 requires `nexus_<subsystem>_<name>` and
  says the service belongs in the Prometheus `job` label, never in the series
  name. Nothing enforced it, so two violations had accumulated:

  | before | after |
  | --- | --- |
  | `nexus_ai_gateway_admission_shed_total` | `nexus_admission_shed_total` |
  | `nexus_hub_scheduler_leader` | `nexus_scheduler_leader` |

  `nexus_admission_shed_total` has never had a non-zero value in production (the
  in-flight gate has never shed), so nothing can have been reading it.
  `nexus_scheduler_leader` **is** live on the Hub — if you have a dashboard or
  query on it, update the name. No in-repo dashboard or alert rule referenced
  either.

  New `scripts/check-prometheus-naming.sh` (`npm run check:prometheus-naming`,
  plus pre-commit on staged Go files) blocks a third one. The service list comes
  from `packages/shared/schemas/thingtype`, so adding a service extends the check
  automatically.

### Fixed: the reference seed no longer deletes a deployment's OAuth callback URL

`OAuthClient.redirectUris` was replaced wholesale by the fixture, which ships
only the `localhost` URLs a developer needs. Any deployment that had registered
its own console domain lost it on the next `seed:prod` run — and because the
authorize endpoint rejects an unregistered `redirect_uri`, every admin was
locked out of the console until someone re-added it by hand. The failure arrived
whenever anyone re-seeded for an unrelated reason, such as a model-price
correction.

`redirectUris` is now merged rather than replaced: the seed guarantees its own
URLs are present and removes nothing it did not ship. Removing a URL is done
through the admin API.

- **Action required** if a re-seed has already removed your console URL: the
  symptom is `redirect_uri not registered` from `/oauth/authorize` and a console
  login that cannot complete. Re-add the URL (admin API, or
  `UPDATE "OAuthClient" SET "redirectUris" = array_append("redirectUris",
  '<your-console-url>/auth/callback') WHERE id = 'cp-ui';`) and it will survive
  every seed from this release on.
- No schema change, no migration. Deployments whose URLs are intact are
  unaffected; the merge is a no-op when the fixture's URLs are already the only
  ones present.

### Upstream failures now carry their cause to the client, the metric, and the traffic row

The gateway already normalised every provider failure onto one canonical cause
and then discarded it at the handler boundary, re-deriving what it needed from
the raw attempt list. That cost a rate limit its 429, left `errors_total` at
zero forever, and collapsed every upstream 4xx into one undifferentiated code.

**Fixed**

- A rate limit is now reported to the client as **429**, not 502, when the
  retry that follows it cannot find a usable credential. The gateway decided
  429-vs-502 by reading the last attempt's raw status, but a target abandoned
  before any call was made is also recorded as an attempt and carries no
  status — and the rate limit is what causes it, by opening the credential's
  circuit so the retry's re-resolve fails. The client was told the provider was
  down (false — it is throttling us) and therefore did not back off. Requires a
  provider credential pool of two or more; single-credential pools were never
  affected.
- The same failure keeps its credential attribution on the traffic row, so
  "which key got rate-limited?" is answerable.
- `X-Nexus-Attempts` counts calls that reached a provider, not targets
  abandoned before dispatch.
- A provider reporting itself overloaded on a status other than 429 is now
  treated as the rate limit it is, on the same footing as the executor, which
  already classified it that way when it decided to retry.

**Changed — `traffic_event.error_code` semantics**

A terminal upstream 4xx now records the provider's canonical cause —
`auth_failed`, `invalid_request`, `context_overflow`, `endpoint_unsupported`,
`not_implemented`, `no_compatible_provider` — where it previously recorded the
blanket literal `PROVIDER_ERROR`.

- No schema change and no migration. Existing rows are untouched and the
  `?errorCode=` filter still matches them exactly as before.
- `PROVIDER_ERROR` is retained in the code as the value for a terminal 4xx that
  carries no canonical cause, but **the AI Gateway no longer emits it**: the
  classifier only reaches that path via a branch that has already resolved a
  `ProviderError`, so every new gateway row carries a cause. It is still written
  by the Compliance Proxy's own pipeline, which this change does not touch.
- **Action required** if you have a saved query, dashboard or alert filtering
  `error_code = 'PROVIDER_ERROR'` and expecting it to mean "any upstream 4xx".
  Against gateway traffic it now matches **no new rows** — it does not merely
  thin out, it goes to zero, while historical rows keep the old value. Widen it
  to the specific causes you care about, or filter on `status_code` instead.
- Codes for failures the *gateway* decided (`PROVIDER_UNAVAILABLE`,
  `PROVIDER_RATE_LIMITED`, `QUOTA_EXCEEDED`, `CLIENT_CLOSED`, …) are unchanged,
  as are all client-facing error envelopes.

**Added**

- `errors_total{provider, error_type}` is incremented for the first time. It
  was registered, exported and documented as "incremented on every non-2xx
  path" while having no caller at all, so it always read zero. `error_type` is
  the terminal attempt's canonical code. Client disconnects (`499`) and
  gateway-internal rejections are deliberately excluded — see
  `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` §8.
- Gateway upstream failures are logged under one stable message per cause, so
  the operator errors page groups them by cause and each can be silenced
  independently. They previously shared a single message and collapsed into one
  row covering every cause.

### Peer service URLs resolved from the Hub (peer-URL config deleted)

A service never configures another Nexus service's URL any more. Each server
service reports its own base URLs to the Thing Registry, and peers resolve the
reported value from the Hub at runtime — removing the config-drift class where
a stale peer URL produced silent inter-service failures.

**Added**

- Every server service (nexus-hub, control-plane, ai-gateway, compliance-proxy)
  now reports a second base URL, `staticInfo.privateUrl` (internal
  service-to-service address), alongside the existing `publicUrl` (external
  clients + the Agent). Config: optional yaml `privateURL` / env
  `<SVC>_PRIVATE_URL` (`NEXUS_HUB_PRIVATE_URL`, `CONTROL_PLANE_PRIVATE_URL`,
  `AI_GATEWAY_PRIVATE_URL`, `COMPLIANCE_PROXY_PRIVATE_URL`); default is
  auto-derived as `http://<primary-outbound-IPv4>:<service-port>` so nothing
  needs to be set in the common case. The compliance-proxy derives its port
  from the runtime-API listen address.
- New Hub endpoint `GET /api/internal/things/service-url/:thing_type`
  (service-token only; agents get 403 — the private URL never reaches end-user
  devices). Returns `{thingType, privateUrl, publicUrl}` for the
  most-recently-seen reporting Thing of the type (one base per service type;
  scaled fleets sit behind one LB base), or 404 `SERVICE_URL_NOT_REPORTED`
  during the peer's boot window (callers retry).
- New shared resolver `packages/shared/transport/peerurl`: lazy first-use
  resolution, in-memory cache with 5-minute refresh (stale value served if a
  refresh fails), 5-second negative TTL, `ErrNotReported` — never a silent
  fallback; errors surface and the next use retries.

**Changed**

- The webhook-forward → AI-Guard trust anchor is now Hub-resolved instead of
  locally configured: the internal `X-RS-Token` is injected per request only
  when the hook endpoint path is `/v1/ai-guard/compliance-webhook` and its
  scheme+host match a trusted base (`webhook.Options.TrustedAIGuardBases`).
  The ai-gateway supplies its own public+private URLs; the compliance-proxy
  supplies the Hub-resolved ai-gateway URLs. While the peer is not yet
  resolved, the webhook posts without the token (fail-safe) and retries on
  the next request.
- compliance-proxy `onboarding.cpUIBaseURL` is now an optional override for
  the 407-page display link; when unset it defaults to the Hub-resolved
  Control Plane public URL.

**Removed**

- The four peer-URL config fields and their env vars:
  compliance-proxy `compliance.aiGatewayUrl`; control-plane
  `bff.aiGatewayUrl` (env `AI_GATEWAY_URL`), `bff.complianceProxyUrl`
  (env `COMPLIANCE_PROXY_URL`), `bff.complianceProxyRuntimeUrl`
  (env `COMPLIANCE_PROXY_RUNTIME_URL`).

**Migration notes**

- Operators who set any of the removed fields/env vars can simply delete
  them — the values are ignored. Split-horizon or non-default topologies are
  expressed on the *reporting* side instead: set the target service's own
  `privateURL` (yaml) or `<SVC>_PRIVATE_URL` (env) to the address its peers
  should dial.
- The auto-derived private URL follows the service's BIND interface: a
  service bound to a specific address (the single-box appliance binds
  `127.0.0.1` behind nginx) advertises that address; a wildcard bind
  advertises the primary-outbound IPv4.
- **Verify compliance-webhook hook endpoints.** The webhook X-RS-Token trust
  anchor now matches the hook endpoint's scheme+host against the AI Gateway's
  reported public/private URLs (plus the gateway's own loopback variants;
  explicit default ports `:443`/`:80` are normalized). If a webhook-forward
  hook posts to the AI-Guard compliance-webhook through a host that is
  neither of those (e.g. a vanity CNAME), the token silently stops riding and
  AI-Guard answers 401 — repoint the hook endpoint at the gateway's reported
  URL.

### Removed — Tier-1 (global) cache switches: `cache_master_kill_switch` and `normaliser_enabled`

The `cache` shadow blob's Tier-1 `global` object is retired. Both switches it
carried duplicated capabilities that finer-grained mechanisms already own.

**Migration notes**

- **Removed shadow-blob fields** — `global.cache_master_kill_switch` and
  `global.normaliser_enabled` are gone from the `cache` config-key blob, which is
  now `{adapters, providers}`. The `cache` config key itself is unchanged and
  Tier-2 (adapter) / Tier-3 (provider) settings are untouched. The removal is
  tolerant in both directions during a rolling restart: a new gateway ignores the
  now-unknown fields on an old blob, and an old gateway reading a new blob defaults
  both to `false` (kill switch off = cache on; normaliser off) — the safe direction.
- **Retired admin endpoint** — `GET /api/admin/cache/global` and
  `PUT /api/admin/cache/global` no longer exist (404). Their only client was the
  Control Plane UI "Global Defaults" panel, deleted in the same change. The
  `prompt-cache` IAM resource is unchanged — the remaining cache endpoints still
  use it.
- **Response-shape change to a KEPT endpoint** — `GET /api/admin/cache/effective`
  (the per-provider effective config, which stays) no longer emits the
  `normaliser_enabled` and `cache_master_kill_switch` keys. Any consumer reading
  those two keys off the effective response must stop; every other key is unchanged.
- **Orphaned table, left in place** — nothing reads or writes the
  `cache_global_config` singleton table any more. It is deliberately **not** dropped
  (no migration, no schema change, zero deploy risk); a later cleanup may drop it.
- **⚠ Upgrade check for an ARMED kill switch** — the new gateway ignores
  `cache_master_kill_switch` entirely. If your deployment currently holds the kill
  switch **ON** (cache deliberately disabled fleet-wide) while the per-tier
  `enabled` flags are still true, your response caches will silently re-enable at
  upgrade. Before upgrading, disable the tiers explicitly instead: the
  `/ai-gateway/cache` status strip's "Disable all gateway cache fleet-wide"
  (sets per-tier `enabled=false`), or a time-boxed Emergency Passthrough
  `bypassCache`. Pre-deploy check:
  `SELECT config FROM cache_global_config WHERE id='singleton'` — if
  `cache_master_kill_switch` is `true`, flip the tiers off first.

**Replacements — no capability was lost**

- **Emergency cache-off has two complementary surfaces.** The cache stage now gates
  purely on the two tiers' own flags (`cacheEnabled = l1Enabled || l2Enabled`).
  (1) The status strip at the top of `/ai-gateway/cache` has a one-click **"Disable
  all gateway cache" fleet-wide** action (confirm dialog, permission-gated) that
  sets both tiers' `enabled=false` — fast and durable, and more discoverable than
  the retired panel, which was buried two tabs deep. (2) **Emergency Passthrough
  `bypassCache`** remains the auditable, time-boxed bypass — mandatory ≥20-char
  reason, `enabledBy` recorded, ≤8 h auto-revert, scopable per adapter/provider.
  Use the first when the cache itself is the fault and must stay off; use the second
  when the bypass must be governed, self-reverting, or narrower than the fleet.
- **The upstream wire-rewrite engine is now demand-driven.** Instead of a global
  gate, the engine derives a `hasWork` flag at reload time from "any adapter has an
  enabled strip rule" OR "any provider has `cache_control` marker injection on", and
  no-ops when there is nothing to do. Enabling a strip rule, or a provider's marker
  injection, *is* the demand — this also fixes the footgun where per-provider marker
  injection was silently swallowed because the global switch was off. The L0
  cache-key normalisation (`NormalizeKey`) always ran and still always runs.
- **UI** — the cache-config panels moved from `src/pages/compliance/cache/` to
  `src/pages/ai-gateway/cache/settings/` so the source path matches the route. The
  `/ai-gateway/cache` route is unchanged; no deep links break.

### Fixed — semantic-cache embedding input is post-redaction

- When a request hook rewrites the wire body (redaction), the L2 semantic
  cache now renormalizes the rewritten bytes once and feeds that canonical
  to the embedding input, the L2 write-back, and the freshness detector —
  the embedding provider and the vector store see the redacted content the
  upstream sees, never the pre-hook original. A renormalize failure skips
  the L2 lookup/write-back and freshness detection for that request
  (L1 exact-match, keyed on the rewritten bytes, is unaffected) instead
  of falling back to the stale canonical. Requests without a rewrite are
  unaffected.

### Added — context-overflow failover and capability-aware smart routing

- New canonical provider-error code `context_overflow`: OpenAI
  (`context_length_exceeded` / "maximum context length"), Anthropic
  ("prompt is too long"), and Gemini ("exceeds the maximum number of
  tokens") 400s are classified separately from terminal invalid_request.
  The executor never retries the overflowing target and fails over to the
  next target when one exists; multi-target routes (fallback chains) now
  advance on overflow where they previously stopped. On the last target
  the provider's own error is surfaced verbatim.
- Smart routing arms a context-upgrade escape: alongside the router's
  pick, the largest-window candidate from the same filtered pool rides as
  a `ContextUpgradeOnly` target used exactly on a context-overflow
  verdict — closing the loop the coarse size estimate cannot.
- Smart candidate selection now also hard-filters by declared
  capabilities: candidates declaring a feature list but lacking `vision`
  (request carries images) or `function_calling` (request declares tools)
  are dropped before the router sees the catalog; undeclared feature
  lists pass and a dimension that would empty the pool is skipped (both
  fail-open).

### Fixed — smart (model=auto) routing is context-window aware

- The smart routing strategy now hard-filters candidate models by the estimated
  request size before the router LLM sees the catalog: candidates whose
  declared `maxContextTokens` cannot hold the estimated input (all roles, tool
  payloads, tool definitions) plus the output reserve (`max_tokens` or 1024)
  are dropped; when nothing fits, the largest-context candidates are kept and
  the routing trace records the overflow risk. Previously a large conversation
  could be routed to a 128k-context model while 1M-context candidates were
  available, producing an upstream context-overflow error.
- The router-LLM call itself is now budget-bounded: the conversation sent to
  the router is staged with recent-turns under
  `min(routerWindow − systemPrompt − 256, 4096)` (router model's declared
  window; 8192 fallback), so an oversized turn is tail-truncated instead of
  being forwarded as-is and failing the router call. The router now also sees
  recent user+assistant turns (client system messages excluded) plus a
  request-metadata line (`~tokens, images, tool definitions`). Overflows are
  counted on `nexus_smart_router_input_overflow_total`. The router LLM must be
  a provider trusted with unredacted traffic — routing runs before request
  hooks; see `smart-routing-architecture.md`.

### Changed — AI-Guard judge input defaults to full-conversation coverage

- `ai_guard_config.input_strategy` now defaults to `full_truncated` (was
  `system_plus_last_user`): the judge sees every turn that fits its context
  window, so violations assembled across turns stay visible. The judge prompt
  template's size is now counted against the input budget, and the input is
  bounded by the shared `inputstaging` budget enforcement (oldest dropped
  first). Existing deployments keep their stored `input_strategy` value; the
  new default applies to fresh installs and rows without an explicit value.

### Changed — overload now degrades into retryable 429s (in-flight admission gate)

- The AI Gateway bounds concurrent in-flight proxy requests (default
  `1024 × GOMAXPROCS`; `AI_GATEWAY_MAX_INFLIGHT` overrides, `0` disables). At
  arrival rates beyond the box's capacity, excess requests are rejected fast
  with **429 + `Retry-After: 1`** in the caller's ingress error shape (OpenAI /
  Anthropic / Gemini envelopes) instead of queueing in-heap until the Go memory
  limit collapses throughput (measured pre-fix: 15.9s p99 at 1.5× capacity;
  the pre-GOMEMLIMIT failure mode was an OOM kill). 429 was already part of the
  data-plane contract (per-key rate limits and quota denials); SDK retry logic
  engages unchanged. Health, metrics, and admin endpoints are never gated. Shed
  requests are counted on `nexus_ai_gateway_admission_shed_total`.

### Fixed — hook-config reload stampede at high load

- Hook configuration freshness is now push-driven with a background TTL-backstop
  ticker; the request path never loads configuration. Previously a TTL-stale
  check on the request path could fan out one full rule-pack database load per
  in-flight request while a slow load was running, collapsing the gateway at
  high request rates (measured: p99 120s at 16k req/s with content hooks on;
  fixed: p99 27ms at the same rate). Rule-pack install ordering also gained a
  deterministic tiebreaker so no-change config reloads can no longer churn the
  compiled matchers.

### Performance — content-hook path allocation and CPU

- Bodies-off deployments no longer allocate a fresh request-body buffer per
  request (the pooled buffer is returned at request end; previously measured at
  52% of all gateway allocation under content-scan load).
- Redact-action rule packs skip re-localization entirely on benign traffic
  (zero matches on a complete scan).
- Config snapshot loads expose `nexus_configcache_load_failures_total` and
  `nexus_configcache_last_success_timestamp_seconds` for alerting on a frozen
  config plane.

### Fixed — Request/Response hook timing

- **Streamed responses now record response-hook timing, exactly once per hook.**
  The streaming response pipeline runs the response stage at every checkpoint, so
  the live audit-only path previously recorded nothing (`response_hooks_ms` NULL)
  while the chunked_async path recorded the same hook once per checkpoint (N
  duplicate rows, an N×-inflated aggregate — observed as a "RESPONSE PIPELINE (63)"
  list of identical rows). The trace is now folded to one record per hook (summed
  latency, latest decision) across the ai-gateway live + Model A paths and the
  shared compliance-proxy/agent path. The audit drawer also collapses any residual
  duplicates (historical rows) into a single `×N` card.

### Added — microsecond-precision hook timing (additive, backward compatible)

- Per-hook latency is now measured in **microseconds** (`latencyUs`) alongside the
  existing truncated-millisecond `latencyMs`, with new aggregate columns
  `request_hooks_us` / `response_hooks_us` beside the unchanged `_ms` columns.
  Hooks run at microsecond scale, so the millisecond aggregates floored a
  sub-millisecond hook to `0`; the µs fields carry the real value, surfaced
  precisely per hook in the control-plane audit drawer. The `_ms` columns / wire
  ids / values are unchanged. The new binwire field ids are forward-incompatible,
  so the deploy order is **schema → Hub → producers**.

### Changed (BREAKING — major version bump)
- **Hook `onMatch` collapses to a single `action` (approve | redact | block).**
  The orthogonal `onMatch.inflightAction` (approve / block-hard / block-soft /
  redact) × `onMatch.storageAction` (keep / redact / drop-content) pair is
  replaced by one `action` field across the AI Gateway, Compliance Proxy, and
  Agent. `redact` rewrites the payload (the same masked body is forwarded,
  returned, and stored); `block` rejects and stores the policy attribution
  (matched rule, reason, compliance tags) — not a content body, since a blocked
  request never produces a masked wire copy; `approve` forwards and stores as-is.
  A redact whose adapter cannot reverse-encode the masked content onto the wire
  (`ErrRewriteUnsupported`) fails **closed** (the request/response is rejected,
  not forwarded unredacted). Soft-block (HTTP 246) is removed — block-soft folds
  into block (HTTP 403). The canonical normalized projection is **no longer
  persisted** for audit; the control plane recomputes it at view time from the
  (already-redacted) raw body, so `request_normalized` / `response_normalized`
  and `request_redaction_spans` / `response_redaction_spans` are no longer
  emitted.
  **Migration:** the config reader maps the legacy keys for a deprecation window
  (one-shot warning); a one-off data migration
  (`tools/db-migrate/manual-scripts/migrate_hook_onmatch_action_2026_06_22.sql`)
  rewrites stored `HookConfig.config.onMatch` rows:
  `block-hard|block-soft → block`, `redact → redact`,
  `approve + keep → approve`, `approve + redact|drop-content → redact`.
  Runtime enforcement is unchanged by the mapping: `block-soft` already **rejected**
  the request — it returned an error response (previously with the non-standard
  status 246, now 403) and never forwarded the traffic, so this is a status-code
  change, not an allow→deny change. The only data-level behavior change is
  `approve + redact|drop-content → redact`, which upgrades a storage-only redact to
  a full redact (the compliance-safe direction, never less masked than before) and
  occurs in no current row, so the live migration is lossless. Client note: any SDK
  that branched on the soft-block status 246 must now treat such a rule's response
  as a 403 reject. The Agent signals a block by dropping the
  connection (no rich error body); the proxies return an attributed 403 whose
  response-stage reason carries rule-ID labels only, never the upstream value.
### Fixed — co-firing redact + soft-block no longer drops the redaction (security)

- **A redact hook co-firing with a soft-block hook now masks-and-delivers instead of
  leaking or failing closed.** When a redact hook (`Modify` + masked content/spans) and
  a soft-block hook fired on the same request or response, the pipeline aggregator
  promoted the reported `Decision` to `BlockSoft` (the strictest) but DROPPED the redact
  hook's replacement content, leaving spans without content. Downstream this produced a
  no-op rewrite that, depending on the path, either failed closed (canonical response)
  or replayed/forwarded the ORIGINAL unredacted body — a PII leak on the shared buffer
  pipeline (compliance-proxy appliance included), the agent Model A wire, and both
  request stages. `mergeResults` now carries the redact's `ModifiedContent`
  unconditionally, and every redaction consumer gates on the new
  `decision.CompliancePipelineResult.CarriesRedaction()` predicate (Modify OR a
  BlockSoft masking a co-firing redact) rather than `Decision==Modify`, so the masked
  body is applied and delivered on all paths. The audit row stamps the disposition
  `action=redact` even when the (soft-block) `Decision` ceiling is `BlockSoft`. No
  config or schema change; behavior is compliance-safe (a hard `block`/`RejectHard`
  still rejects; a standalone soft-block still delivers-with-warning). The no-redactor
  buffer degrade is now posture-aware (appliance fail-closed, agent fail-open).

### Changed — three-end streaming-compliance parity via a shared Model A engine

- **The Model A streaming-compliance algorithm is now a single shared engine driving
  three ends.** The prescan-gated real-time streaming path (bounded tail-hold +
  union prescan + confirm + escalate-to-buffer redaction) for a redact-scope
  `chunked_async` stream is extracted into a substrate-agnostic engine
  (`shared/transport/streaming/modela`). The AI Gateway drives it with a canonical
  substrate (fail-closed) and the transparent proxy used by the Agent +
  Compliance Proxy drives it with a raw-SSE-wire substrate (fail-open, NE
  host-packet safety) — so hooks/compliance behave identically across all three
  ends while each keeps its own ingress and delivery. The transparent-proxy live
  path becomes **audit-only** (real-time write-through, observe-only checkpoints,
  never blocks/rewrites): scope-derived routing sends a `block` scope to buffer and
  a `redact` scope to Model A (or buffer), so only non-enforcing traffic reaches
  live. The adoption also closed two latent PII-leak paths in the shipped AI Gateway
  Model A (a redact masked behind a co-firing soft-block; a memory-pressure eviction
  of an incomplete content unit). No config or contract change; behavior is
  compliance-safe (a sub-window value is never delivered raw; storage never persists
  a raw prefix on an enforcing outcome).

### Changed — normalized projection is now fully view-time (no migration required)

- **The normalized traffic projection is no longer written on the hot path; it is
  recomputed at view time.** Building on 1.1.0 (where the producers stopped
  stamping it), this completes the move end-to-end: the Hub no longer
  self-derives the projection from agent uploads, and the periodic
  **normalize-backfill job is retired**. The Control Plane (and the Agent
  dashboard) recompute the normalized request/response on demand — when an
  operator opens a Traffic detail drawer — from the stored, already-redacted
  body, so the rendered projection always reflects the current decoder version
  with no scheduled job and no stored copy to drift.
  - **`traffic_event_normalized` and `traffic_event_normalize_skip` are retained,
    write-frozen.** No schema change and **no migration is required.** The
    `traffic_event_normalized` sidecar still receives a row only when an older
    shipped agent uploads its own governed normalized copy — for a block/redact
    row whose raw body was dropped, that uploaded copy is the sole forensic
    record. The `traffic_event_normalize_skip` ledger is now inert (the job that
    wrote it is gone). Dropping both tables is a planned deprecation-window
    follow-up, not part of this change.
  - **`GET /api/admin/traffic/{id}/normalized`** now returns the recompute and no
    longer includes redaction spans (the recompute reads an already-redacted
    body). It returns `404` when the projection is unavailable — no stored body
    to recompute from (payload capture was off, or a spilled body has aged out of
    retention) and no stored sidecar fallback.
  - **Operators:** the `nexus_normalize_backfill_*` counters are no longer
    emitted. A missing/NULL `traffic_event_normalized` sidecar is now the normal
    state for current traffic, not a gap to heal.

### Changed — streaming-compliance enforcement (config-compatible, no migration)

- **Streaming response compliance is scope-routed, and the real-time path is
  audit-only.** A response hook's enforcement scope decides how a streamed (SSE)
  response is handled, overriding the admin streaming-mode default wherever that
  default cannot enforce:
  - A **block** scope buffers the full response before any byte is delivered
    (zero-leak hard block).
  - A **redact** scope under `chunked_async` streams in real time behind a prescan
    gate that holds a bounded trailing window and escalates to buffered redaction on
    a confirmed match — best-effort on the wire: a complete sensitive value is never
    delivered, but a leading fragment of a value longer than the window may reach the
    client before redaction engages, while the persisted audit copy stays fully
    masked within that window. A redact scope under `passthrough` falls back to
    buffering rather than forwarding raw.
  - A **non-enforcing** pipeline streams in real time, audit-only: it scans and tags
    every checkpoint but never blocks or rewrites the wire.
  - An **unbuildable fail-closed** response hook forces buffering, which fails closed
    with an in-band error frame — never a silent fail-open on the real-time path.
- **The streamed `finish_reason` is preserved** across the canonical re-encode
  instead of collapsing to `stop`.
- The `streaming_compliance.config` mode enum (`passthrough` / `buffer_full_block` /
  `chunked_async`) is unchanged; no migration. The Control Plane UI shows an
  always-visible per-mode disclosure of exactly what each mode enforces.

## [1.1.0] — 2026-06-28

The first release after the 1.0 GA. It is a **performance and audit-storage**
release: the captured-traffic pipeline was reworked to push far higher no-loss
throughput on a single box, several shipped defaults flip toward that
throughput, the **Windows desktop agent reaches GA**, and the AWS Marketplace
AMI / single-instance appliance form factor is now a first-class deployment
target.

> **Upgrade note.** Two changes are breaking **for direct database / config
> consumers** and require a one-time migration on deployments that retain
> traffic history (see **BREAKING (migration required)**, below). Fresh
> installs — the AMI appliance, or `prisma db push` against an empty database —
> need no manual step. The supported appliance upgrade path applies the schema
> change automatically, which is why this ships as a minor rather than a major;
> the data re-encode is the only manual action, and only when old rows must
> remain readable.

### Changed — BREAKING (migration required for existing deployments)

- **Captured body storage is now raw `BYTEA`.**
  `traffic_event_payload.inline_request_body` / `inline_response_body` hold the
  captured body's **raw bytes** (text verbatim, arbitrary binary, or a raw
  `zstd` / `s2` compressed frame), discriminated by the
  `inline_request_encoding` / `inline_response_encoding` columns
  (`text` | `binary` | `zstd` | `s2`, with `base64` accepted as a read tag).
  Raw bytes let PostgreSQL store the body as-is — no per-insert parse / validate
  / tree-store, and no +33% base64 size inflation.
  - **Direct `traffic_event_payload` consumers:** read the `inline_*_body`
    column together with its `inline_*_encoding` discriminator and decompress
    accordingly, instead of parsing the old JSONB envelope.
  - **Migration:** `prisma db push` applies the `TEXT` → `BYTEA` column change.
    Rows captured before the upgrade whose encoding is `zstd` / `s2` were stored
    as base64 text; their bytes survive the type swap as base64 ASCII and must
    be decoded once to the raw frame, or they read as absent:
    `UPDATE traffic_event_payload
       SET inline_request_body = decode(convert_from(inline_request_body,'UTF8'),'base64')
     WHERE inline_request_encoding IN ('zstd','s2');`
    (and the same for `inline_response_body` / `inline_response_encoding`).
    Old `base64`-tagged rows decode transparently on the read path. The
    authoritative note lives in `tools/db-migrate/schema/traffic.prisma`
    (model `traffic_event_payload`).

- **Hook `onMatch` collapses to a single `action` (`approve` | `redact` |
  `block`).** The orthogonal `inflightAction` × `storageAction` pair is replaced
  by one field across the AI Gateway, Compliance Proxy, and Agent: `approve`
  forwards and stores as-is; `redact` rewrites the payload (the same masked body
  is forwarded, returned, and stored); `block` rejects and stores the masked
  copy. The soft-block path folds into `block`. The canonical normalized
  projection is **not persisted** for audit — the control plane recomputes
  it at view time from the (already-redacted) raw body — so
  `request_normalized` / `response_normalized` and the
  `request_redaction_spans` / `response_redaction_spans` columns are not
  emitted.
  - **Migration:** the config reader maps the legacy
    `inflightAction` / `storageAction` keys for a deprecation window (one-shot
    warning), and the one-off data migration
    `tools/db-migrate/manual-scripts/migrate_hook_onmatch_action_2026_06_22.sql`
    rewrites stored `HookConfig.config.onMatch` rows
    (`block-hard|block-soft → block`, `approve + keep → approve`,
    `approve + redact|drop-content → redact`). The proxies return an attributed
    `403` whose response-stage reason carries rule-ID labels only, never the
    upstream value; the Agent signals a block by dropping the connection.

### Changed — defaults (overridable, no migration required)

### Changed (defaults — overridable, no migration required)
These flip shipped behavior toward higher throughput; each is overridable by env
or yaml and an upgrade silently inherits the new default. Operators relying on the
prior strictness should set the opt-out shown.
- **Quota enforcement is soft by default (`NEXUS_QUOTA_WRITE_BEHIND` ON).** Per-
  request quota cost is accumulated in-process and flushed to Redis on a 250ms
  interval behind a 1s read cache, instead of a synchronous per-request Redis
  round-trip. Overshoot per instance ≤ ~1.25s of spend; across an N-instance fleet
  the blind-spend window is that × N, and a hard kill loses un-flushed increments
  (graceful shutdown drains). Opt out: `NEXUS_QUOTA_WRITE_BEHIND=0` (strict
  synchronous per-request accounting).
- **Credential-stats write-behind ON by default (`NEXUS_CREDSTATS_WRITE_BEHIND`).**
  Credential usage counters defer off the request path; circuit-breaker
  transitions stay synchronous. Opt out: `NEXUS_CREDSTATS_WRITE_BEHIND=0`.
- **Audit overflow default `AI_GATEWAY_AUDIT_LOSS_MODE=spill`.** The request path no
  longer back-pressures on a full audit pipeline; overflow spills to a durable
  on-disk spool replayed to Postgres. No loss until the spill channel + disk
  saturate; sustained overload past that drops records, counted on `dropped_total`.
  Opt out for strict no-drop back-pressure: `AI_GATEWAY_AUDIT_LOSS_MODE=block`.
- **`NEXUS_EVENTS` audit stream is in-memory by default (`NEXUS_EVENTS_STORAGE=memory`,
  `DiscardNew`, cap `NEXUS_EVENTS_MAX_BYTES=auto` = 15% RAM).** Keeps the
  delay-tolerant burst buffer off the data disk. A NATS broker restart/crash drops
  published-but-undrained events (the overflow→disk no-loss path covers only the
  stream-full case). Opt out for a durable file-backed stream:
  `NEXUS_EVENTS_STORAGE=file`.
- **`GOMEMLIMIT` auto-set from the cgroup limit when unset.** Each service, if
  `GOMEMLIMIT` is not provided, reads the cgroup memory limit at boot and sets the
  Go soft limit to ~70% of it (logging a WARN with the value), leaving it unset
  when no cgroup limit is detectable. Pin explicitly to override.
- **Cache freshness protection defaults ON (`extract_cache_config.apply_freshness_rules`
  default `false → true`).** Freshness protection is intrinsic to caching: enabling a
  cache tier should not silently replay a stale time-sensitive answer (today's date,
  "latest" prices, live status). The freshness detector only runs when a cache tier is
  active, so a cache-off gateway still pays nothing and stays a lean passthrough. The
  flip applies to fresh installs and the no-row default; an existing deployment that
  already saved an `extract_cache_config` row keeps its stored value, so **no migration
  runs and no admin choice is overwritten**. Operators who already enabled L1/L2 and
  want freshness should re-save the extract-cache config (or toggle the Freshness rules
  card) once; operators who want maximum hit-rate can leave it off explicitly.
Each default below flips shipped behavior toward higher throughput. An upgrade
silently inherits the new value; the opt-out to restore prior behavior is shown.

- **One same-target retry by default** (`maxAttemptsPerTarget` 1 → 2). A single
  transient upstream fault (network / timeout / 429 / 5xx) now retries once in
  place before failover, so flaky provider endpoints self-heal instead of
  surfacing a hard error. Bounded to one retry so a non-idempotent generation is
  re-sent at most once. Opt out: set `maxAttemptsPerTarget: 1` on the routing
  rule / retry policy.
- **Audit overflow defaults to `spillblock` (zero-loss).** The request path does
  not back-pressure on a full audit pipeline; overflow spills to a durable
  on-disk spool, and when the spool channel itself saturates the writer
  back-pressures rather than dropping. Opt out:
  `AI_GATEWAY_AUDIT_LOSS_MODE=spill` (drop on saturation) or `=block` (strict
  synchronous back-pressure on the request path).
- **Quota enforcement is soft by default** (`NEXUS_QUOTA_WRITE_BEHIND=1`).
  Per-request quota cost accumulates in-process and flushes to Redis on a ~250ms
  interval behind a 1s read cache. Overshoot per instance ≤ ~1.25s of spend; a
  hard kill loses un-flushed increments (graceful shutdown drains). Opt out:
  `NEXUS_QUOTA_WRITE_BEHIND=0`.
- **Credential-stats write-behind by default**
  (`NEXUS_CREDSTATS_WRITE_BEHIND=1`). Credential usage counters defer off the
  request path; circuit-breaker transitions stay synchronous. Opt out:
  `NEXUS_CREDSTATS_WRITE_BEHIND=0`.
- **`NEXUS_EVENTS` audit stream is in-memory by default**
  (`NEXUS_EVENTS_STORAGE=memory`, `DiscardNew`, cap `NEXUS_EVENTS_MAX_BYTES=auto`
  ≈ 15% RAM). Keeps the delay-tolerant burst buffer off the data disk; a NATS
  restart/crash drops published-but-undrained events. Opt out for a durable
  file-backed stream: `NEXUS_EVENTS_STORAGE=file`.
- **Response cache is opt-in per route, with substring freshness matching.**
  Caching is enabled per route rather than globally; turn it on for the routes
  that benefit. The Control Plane UI surfaces the staleness risk tip.
- **`GOMEMLIMIT` auto-set from the cgroup limit when unset.** Each service reads
  the cgroup memory limit at boot and sets the Go soft limit to ~70% of it
  (WARN-logged), leaving it unset when no cgroup limit is detectable. Pin
  explicitly to override.
- **Seed defaults:** content hooks ship **OFF**, and the application virtual key
  carries a default **$50k/month** quota policy.
- **Inline-body audit codec defaults to `s2`** (`AI_GATEWAY_AUDIT_CODEC`, `zstd`
  available); the CGO matcher scan limit auto-sizes (`NEXUS_CGO_SCAN_LIMIT=auto`).

### Changed — audit transport (internal, no shipped-contract break)

- **gw→Hub audit wire defaults to a binary TLV frame**
  (`NEXUS_AUDIT_WIRE=binary`). The Hub peeks the frame magic and dual-reads, so
  the legacy JSON wire still decodes; `NEXUS_AUDIT_WIRE=json` reverts. No
  persisted-contract or external API change.

### Added

- **Windows desktop agent is now GA.** Windows interception runs on a signed
  `NexusWFP` kernel driver (Windows Filtering Platform, transparent TCP
  connect-redirect, with QUIC fallback and IPv6). macOS, Linux, and Windows
  desktop agents are all GA.
- **AWS Marketplace AMI / single-instance appliance.** `nexus-ami/` bakes the
  binaries, UI, Prisma, nginx, PostgreSQL, Valkey, and NATS into one AL2023
  image via Packer, with Vectorscan compiled on-instance and the rig-validated
  audit-write defaults shipped in. See `nexus-ami/README.md` and
  `docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md`.
- **Vectorscan-backed hook pattern matching** with an edit-time pattern
  performance test in the Control Plane (governance) so admins see a rule's scan
  cost before saving.
- **Semantic vector cache tiering** — the L1 exact-match extract and L2 semantic
  lookup are now independent tiers.
- **On-demand profiling** — a `NEXUS_PPROF_ADDR` pprof endpoint on all four
  services plus SIGUSR1 file dumps that include Go `MemStats`.
- **Typed error banner** for non-200 rows in the audit drawer.

### Performance

- **COPY-based bulk insert** for `traffic_event` / `traffic_event_payload`,
  with a row-backing pool to cut per-batch allocations.
- **Adaptive memory/disk self-tuning** of the audit pipeline: lossless
  spill-recovery, backlog-aware drain, batched spill with geometric growth, and
  a lazy-canonical default.
- **Hook scan** folds each hook's raw-body prefilters into one union scan, caps
  wide repeats in the detection database, and ships an AVX-512 build flag.
- **Lower allocation on the audit/alert hot paths** — lock-free precomputed
  alert dispatch, zero-copy pooled slim decodes, and typed identity/detail
  structs replacing map reflection.
- **Dropped 7 rarely-read `traffic_event` indexes** to cut ingest
  write-amplification.

### Fixed

- View-time normalization uses the **ingress** wire format rather than the
  upstream adapter format, so the audit drawer renders the request as the client
  sent it.
- Routing-strategy filter lists all canonical strategies with labels.
- Dashboard number formatting — token B/T tiers and cost separators.
- Governance pattern-performance endpoint returns `[]` rather than `null`.

### Removed

- The in-tree load generator (`tools/loadtest`) was extracted to the standalone
  `nexus-loadtest` repository.

### Fixed (gateway response cache correctness)
- **Emergency cache master kill switch is now wired into the data plane.**
  `cache_master_kill_switch` (the Tier-1 global cache config) was parsed but never
  consulted by the AI Gateway, so flipping it did nothing. It now gates both gateway
  response cache tiers — L1 exact-match and L2 semantic — at the cache stage
  (`cacheEnabled = (l1||l2) && !cache_master_kill_switch`). It does not disable
  provider-side prompt caching (Anthropic markers / Gemini context cache), which only
  makes the upstream cache and never serves a stored gateway response.
- **L1 exact-match cache fills regardless of the `cache.broker` flag.** With
  `cache.broker=false` (the default) the broker registry was never constructed and the
  broker pump is the cache's sole writer, so an admin-enabled L1 tier silently never
  filled (0% hit rate). The registry is now always constructed; `cache.broker` controls
  only same-key in-flight dedup (coalesce concurrent same-key MISSes onto one upstream
  call vs. independent calls) — either way the cache fills.
- **L1 cache no longer serves cross-VK entries during the boot window or on
  Sentinel/Cluster Redis.** L1 folds the fleet `vary_by` isolation scope into its cache
  key, but that scope arrives on the semantic-cache config push. Before the first push
  the scope was unset (fleet-wide), so an entry written in that window could be read by
  a different virtual key; and on Sentinel/Cluster Redis the semantic config was never
  delivered to the gateway at all. L1 now fails closed (no lookup/store) until the fleet
  config has loaded, and the config snapshot (including `vary_by`) is delivered on every
  Redis topology — decoupled from the `*redis.Client`-only index lifecycle.

## [1.0.0] — 2026-06-14

First general-availability release. All three intercept planes (AI Gateway,
Compliance Proxy, Desktop Agent) and the full architecture — Hub Thing/shadow
model, control plane + UI, compliance/audit pipeline, provider-adapter
framework — are production-complete. macOS + Linux desktop agents are **GA**
(Windows experimental).

### Added

- **Desktop Agent AI-chat capture (macOS + Linux GA).** End-to-end interception
  and structured normalization of AI-chat traffic — codex (OpenAI Responses on
  chatgpt.com), Cursor (app + `cursor-agent` CLI via
  `/agent.v1.AgentService/Run`), and browser web-chat — into the audit /
  `traffic_event` pipeline without breaking the tools. macOS uses the
  `NETransparentProxyProvider` system extension as the sole intercept path.
- Cursor connect-RPC decoder: per-frame gzip-decompressed agent-service frames
  decode embedded OpenAI-compat / Lexical JSON into structured conversation +
  model + readable tool calls.
- AI vibe-coding documentation surface (`docs/developers/workflow/ai-workflow.md`,
  `docs/developers/workflow/ai-skill-catalog.md`).
- Two binding lints with HARD pre-commit + strict CI gates:
  `check-no-prod-todos.mjs` and `check-no-yaml-secrets.mjs`; reverse-grep
  detection in `check-no-redis-pubsub.mjs`.
- `.github/ISSUE_TEMPLATE/` and `.github/CODEOWNERS`.

### Changed

- `useapi-querykey` and `no-redis-pubsub` lints ratcheted from warn-only to HARD
  pre-commit + strict CI.
- Streaming-policy three-service alignment: all three data planes load the
  streaming-policy snapshot from the Hub-pushed `streaming_compliance.config`
  shadow; an unreadable snapshot at boot resolves to `passthrough`
  (`DefaultPolicy()`) rather than a hard-coded YAML value.
- `MQBatchWriter.Flush()` coordinates with the writer loop so all pending events
  are drained, including those moved into the loop's private buffer.

### Fixed

- **`traffic_event` requested-vs-routed semantics.** REQUESTED columns
  (`model_id` / `provider_id` / `provider_name`) mean what the client asked for
  and are NULL when the request did not pin a single catalog model; the
  `routed_*` columns carry what actually served, and all usage / cost /
  analytics attribute by the routed side. Direct consumers reading `provider_id`
  / `model_id` as "what served" should switch to `routed_*`.
- Connect-RPC envelope flags (`0x01` per-message gzip vs `0x02` end-of-stream)
  are decoded distinctly, fixing Cursor `/agent.v1.AgentService/Run` capture.
- Cursor host interception is chat-only — `*.cursor.sh` passes through by
  default and captures only chat-bearing paths.
- `docker-compose.yml` Postgres credentials honor `${POSTGRES_*}` overrides.

---

## How releases work

Shipped work accumulates under `Unreleased`; at each release cut the section is
renamed to `[X.Y.Z] — YYYY-MM-DD` and a fresh `Unreleased` opens above it. Each
release mirrors the structure above
(`Added` / `Changed` / `Performance` / `Fixed` / `Removed` / `Deprecated` /
`Security`).

Versioning policy:

- **Major** — a breaking change to a shipped contract (public/admin API,
  routing-rule schema, `traffic_event_*` tables, agent↔Hub wire) with **no
  in-place migration path**: a re-architecture an existing deployment cannot
  follow without rework.
- **Minor** — new features, performance work, and schema changes that ship with
  an automated migration, **even when direct database consumers must adapt** —
  those adaptations are called out per entry under "BREAKING (migration
  required)".
- **Patch** — bug fixes, docs, and lint changes.
