# Model capability architecture

How Nexus describes what a model can do, which field owns which question, and
what every consumer is allowed to read.

This is the companion to
[endpoint typology](./endpoint-typology-architecture.md): that document
classifies **requests**, this one describes **models**. They meet at exactly one
field — `Model.type` — and the boundary between them is the first rule below.

Anchor packages:

- `tools/db-migrate/schema/providers.prisma` — the `Model` row: `type`,
  `features`, `inputModalities`, `outputModalities`, `capabilityJson`.
- `packages/control-plane/internal/ai/providers/modelstore/` — the only writer.
  `modalities.go` derives the modality arrays; `model.go` holds Create/Update.
- `packages/shared/transport/typology/endpointkind.go` —
  `EndpointKindAcceptsModelType`, the endpoint↔type guard.
- `packages/ai-gateway/internal/routing/` — the readers:
  `resolver_modality.go` (type), `strategies/strategy_smart_sizing.go`
  (modalities + features), `capability/filter.go` (capabilityJson).
- `packages/ai-gateway/internal/ingress/models/models.go` — the public
  `GET /v1/models` contract, which exposes all five fields.

## 1. The four questions, and the one field that answers each

A model carries four independent facts. Each has exactly one owner. Any
consumer that reads a different field to answer a question is a defect,
regardless of whether the two agree today.

| Question | Owner | Shape | Example |
|---|---|---|---|
| **Which endpoint serves it?** | `type` | scalar | `chat`, `embedding`, `image`, `tts`, `stt`, `realtime`, `video`, `rerank` |
| **Which modalities can it take / return?** | `inputModalities` / `outputModalities` | arrays | `["text","image"]` / `["text"]` |
| **Which non-modality capabilities does it have?** | `features` | array | `streaming`, `function_calling`, `reasoning`, `json_mode`, `structured_outputs` |
| **What are its per-endpoint numeric limits?** | `capabilityJson` | object | embeddings `min_dimension`/`max_dimension`, `supported_dimensions`, `max_batch_size` |

The load-bearing sentence: **`type` answers which endpoint, never which
modality.** A scalar cannot answer both, and the attempt to make it do so is
what produced the retired `audio` type — minted by a discovery heuristic for
any id containing "audio", applied to `gpt-audio-*` models that OpenAI serves
on chat completions, which then made every one of their requests unroutable.

## 2. Why this document exists

Three of the four fields overlapped, and the overlap was not theoretical.
Every item below was measured on production, not inferred.

**`features: ["vision"]` and `inputModalities ∋ "image"` described the same
property.** On 2026-08-05, 34 chat models on production advertised `vision`
while declaring text-only input, and **zero** chat models declared `image`
input at all. The smart router — the most-used routing path in this
deployment — read `features`, so it answered "can this model see?" from the
field that disagreed with the catalog's own modality arrays.

**Nothing kept them consistent on write.** `CreateModel` defaulted every model
to `inputModalities ["text"]` regardless of its type or features, so a model
created with `features: ["vision"]` landed self-contradicting. That is how
`command-a-vision-07-2025` reached production advertising vision with
text-only input, minted during this very session.

**The admin UI can edit only `features`.** `grep -rn "inputModalities"
packages/control-plane-ui/src` returns zero hits. The modality arrays are in
the database, are served on `GET /v1/models` to every SDK caller, and are
invisible and uneditable in the only interface an administrator has. The
Features checkbox list offers Vision — so "can see" is expressible and "can
hear" is not.

**"Required" is inexpressible.** `gpt-audio-1.5` and `gpt-audio-mini` are chat
models that *cannot serve an ordinary text-only chat request*: OpenAI answers
`400 invalid_value`, "This model requires that either input content or output
modality contain audio." Nothing in the vocabulary can say that, so a
cross-ingress regression produced 16 such rows — every ingress, stream and
non-stream — and no filter could have prevented them.

## 3. The rules

**R1. `type` answers which endpoint, never which modality.** The endpoint↔type
guard is `EndpointKindAcceptsModelType`. It runs in two places, and both are
required: `filterByModality` for rule-selected targets, and the explicit-model
passthrough in `stage_routing.go`. A guard that covers only the routing pool
lets the explicit path through unchecked — see R5.

**R2. Modality questions read the modality arrays. Only.** `features` must
never be consulted to decide whether a model handles a modality. As of
`be187c46d` the smart router's capability filter reads `InputModalities` for
image input and `Features` only for `function_calling`.

**R3. `features` carries only non-modality capabilities.** `streaming`,
`function_calling`, `json_mode`, `thinking`. `vision` stays in the array
because it is on a shipped API contract, but it stops being a second source of
truth: **it decides nothing** (true today — R2 landed in `be187c46d`) and
**becomes a derived projection of `inputModalities ∋ image` rather than an
independently stored value** (M5, not yet done). Until M5 the two are separate
columns held equal by the R4 invariant; the equality is a migration state, not
the design.

**R4. The write path maintains the invariant, in every direction.** A model
whose `features` claim vision accepts image input, and a model that accepts
image input reports vision. This holds on create AND update; the UI keeps
sending `features` and the backend translates. That translation is not a
second shape — it is a vocabulary boundary, the same way an adapter translates
canonical to wire.

**R5. Every guard covers both selection paths.** Rule-selected targets and the
explicit-model passthrough. The modality guard does; the embeddings capability
guard does not, which is why `dimensions=512` on a Cohere embedding model
returns 200 with 1024-dimension vectors instead of a refusal.

**R6. Required is a distinct fact from supported, and `required ⊆ supported`.**
See §4.

**R7. A limit is described in the shape the model actually has — a range when
the model accepts a range, a set only when it accepts a set.** Embedding
`dimensions` carries both forms: `min_dimension`/`max_dimension` for a model
that truncates to any width (Matryoshka — OpenAI `text-embedding-3-*`, Gemini
`gemini-embedding-001`), and `supported_dimensions` for one that emits a fixed
set (Cohere v3 emits 1024 and nothing else). When a range is declared it wins;
the enumeration applies only in its absence.

The rule exists because the wrong shape is not a cosmetic mismatch — it
manufactures refusals. A catalog listing `[256,512,1024,3072]` for
`text-embedding-3-large` turned every `dimensions: 1536` caller into a local
`400 MODEL_CAPABILITY_MISMATCH` for a request OpenAI serves without complaint,
and no amount of adding values fixes the class, because the next unlisted width
fails the same way. An enumeration over a continuous capability is a claim the
model never made, and R7 is what stops us restating it.

Both readers of the field enforce this identically —
`routing/capability/filter.go` for the request path and
`storage/configstore/semantic_cache.go` for the admin config path. They answer
the same question, so they may not disagree: an operator must not be blocked
from configuring a dimension the gateway would forward, nor allowed to
configure one it would reject.

## 4. Required modalities

`gpt-audio-*` needs audio in the input **or** the output. That is a
disjunction across the two arrays, so neither `requiredInputModalities` nor
`requiredOutputModalities` can express it, and a pair of them would invite the
reader to assume conjunction.

One field: `requiredModalities: string[]`, meaning **the request must carry at
least one of these modalities, counting both what it sends and what it asks
for**. For `gpt-audio-*` that is `["audio"]`. For every other model today it is
absent, and absent means no constraint.

The invariant is `requiredModalities ⊆ (inputModalities ∪ outputModalities)`,
enforced on write. A model cannot require what it cannot handle.

**Known limit, recorded deliberately.** The semantics are disjunctive — *at
least one of*. A model that required audio **and** image simultaneously could
not be expressed. No such model exists in the catalog today, and inventing a
conjunction/disjunction selector for a case with zero instances is the kind of
speculative knob this codebase deletes on sight. But the choice is not free:
changing the meaning of an already-shipped `requiredModalities` later would be
a breaking contract change, not an extension. **The trigger for revisiting is
the first model that needs a conjunction**, and the resolution then is a new
field with its own name, never a redefinition of this one.

Both sides of the check are already reachable at routing time — measured, not
assumed:

- **Input.** `RoutingContext.Request` is a `*normcore.NormalizedPayload`, and
  the codecs set `core.MediaRef.Modality` structurally from the part type
  (`input_audio` → audio, `image_url` → image, file → file). No semantic
  guessing from prompt text.
- **Output.** `openai_chat.go` lists `modalities` and `audio` in the request
  `FieldSpec.Optional`, so the caller's output intent survives into canonical.
- **Precedent.** `EmbeddingRequestParams` is already threaded through
  `RoutingContext` for exactly this purpose — "so the capability pre-filter can
  apply compatibility rules." Same idiom, no new architecture.

## 5. Migration

`features` including `vision`, and both modality arrays, are all on the shipped
`GET /v1/models` contract. Under the 1.0 GA rule none may silently change
shape.

| Step | Change | Compatibility |
|---|---|---|
| **M1** | Write-path invariant on create and update | Additive. Fixes rows going in; no reader changes. |
| **M2** | Backfill: every row satisfies the invariant | Data only. Already true on production as of 2026-08-05 (0 disagreeing rows). |
| **M3** | Readers switch to modalities | Behaviour change, guarded by M1+M2. Done for the smart router in `be187c46d`. |
| **M4** | UI edits modalities; Vision checkbox becomes a derived display | The Features surface is **extended**, not replaced — no new page, no new tab. |
| **M5a** | Modalities move to one home in the catalog; the generator emits them to wizard templates | Structural. `Model.json` is byte-identical — the values did not change, only where they are stated. |
| **M5** | `features.vision` becomes derived on read | Contract-preserving: the field still appears with the same value. |
| **M5-fix** | The admin API accepts the modality arrays on create and update | Additive. They were editable in the UI and unreadable by the handler, so an admin's edit returned 200 and changed nothing. |
| **M6a** | `requiredModalities` column, carried end to end | Additive; absent = no constraint, so every existing row keeps its behaviour. |
| **M6b** | Populate it: `gpt-audio-*` → `["audio"]`, in the catalog AND on production | Data. prod-deploy applies schema but never seeds, so production needs the same targeted repair the `type` fix needed. |
| **M6c** | Routing reads it; the smoke probe reads it | Behaviour. Turns 16 measured upstream 400s into correct selection. |

M1–M6 are complete.

**M6b filled two rows, not a table.** The plan said "populate it by endpoint
kind"; measuring the catalog said otherwise. A floor only does work where a
model competes for requests it cannot serve, which means chat — every other
type has its own endpoint and never sees a plain text chat request. Of 160
chat models, the ones accepting a non-text modality split into image (vision
models, which serve text perfectly well and need no floor) and audio:
`gpt-audio-1.5` and `gpt-audio-mini`. Filling in "requires audio" on the four
`stt` rows would have written a value nothing reads, and the next reader would
reasonably assume it did something. The standing check enforces both halves:
no floor outside a modality the model accepts, and no floor on a non-chat row.

**M6c cost 319ns and three rewrites to get there.** The first version ran the
filter unconditionally and copied the whole candidate pool on every request to
change nothing 99% of the time: +98% and double the allocations on the
image-need benchmark. Scanning for the first failure instead of building a
fresh slice returned the allocations to baseline. The remaining gap was
`for _, r := range rows` copying a wide struct per iteration — indexed access
took it from 2096ns to 1722ns against a 1403ns baseline. Final measured
position: image-need +22.7% (+319ns), both-needs +0.3% (noise). Against an
auto-routed request that makes an LLM call, 319ns buys the elimination of a
class of upstream 400s.

**M6a's fourth fact.** `inputModalities` is a model's ceiling — what it can
accept. `requiredModalities` is its floor: what a request must carry for the
model to serve it at all. Neither derives the other, and neither derives from
`type`: `gpt-audio-mini` is `type=chat`, accepts `["text","audio"]`, and
refuses a plain text request upstream. Sixteen such 400s were measured before
the column existed and nothing on the row could have predicted them. Empty is
the normal case and means no constraint, so every existing row keeps its
behaviour. No admin editor yet — the data has to exist and be readable before
it is worth asking anyone to diverge from it.

Adding it surfaced that a model was created down **two** paths with **two**
normalizers. `providerstore.CreateProviderWithChildren`, which is what the
admin wizard uses, had its own: it defaulted every type to
`["text"]/["text"]`, so a wizard-created `stt` model landed declaring it
accepts text rather than audio — the exact defect `defaultModalities` was
written to fix, still live on the path most admins actually use — and it never
folded `vision` at all, so M5's translation did not apply there. Both paths now
share `modelstore.NormalizeCreateParams`. Two normalizers for one write is the
same shape of bug as two vocabularies for one fact.

**How M5 landed.** `vision` is no longer stored. On the way in it is
translated, not dropped and not rejected: the admin API is a shipped 1.0
contract, so a client that has always sent `features: ["vision"]` keeps
working and keeps meaning what it meant — the string is removed from
`features` and `image` is added to `inputModalities`
(`modelstore.FoldVision`). On the way out `GET /v1/models` puts it back
(`ingress/models.withDerivedFeatures`), so every SDK reading `features`
sees exactly what it saw before. Chat only: `gemini-embedding-2` accepts
images and is not a chat model, and calling it a vision model would be a new
claim rather than a preserved one.

The catalog was stripped in the same change — 95 models, with a refusal built
into the strip for any row whose `inputModalities` lacked `image`, so the
claim could not be lost silently. Verified by comparing the set of models
claiming image input either way, before and after: 96 both times, none lost,
none gained.

The admin UI's Vision checkbox is gone. It said what the Accepts row of the
modalities field says, and offering both is what let an admin tick one without
the other. `mergeModelFeatureOptions` still renders the value for a row that
still carries it, so an un-migrated row stays editable instead of losing the
value on the next unrelated save.

Fixing M5 surfaced that M4 had shipped a UI writing into a hole: neither
`POST /providers/{id}/models`, `PUT /models/{id}`, nor the two nested
provider-create paths read the modality arrays from the request body, so an
admin edited them, got 200, and the row was untouched. The OpenAPI carried
them on responses only. Both are closed here — the fields are on all four
request paths and in the specs.

**M5a exists because M5 cannot be done without it.** The modality arrays lived
in two places in `model-catalog.json`: under `seed` for the 75 rows that reach
the database, at the top level for the 125 template-only rows the provider
wizard creates models from. One fact, two homes — and that shape had already
produced one silent failure, a sweep that wrote the top level while the
generator read `seed`, so the repair was a no-op and the check certifying it
was green because nothing changed. The generator also never emitted modalities
into the wizard templates at all, so a template model posted none and the
create API derived a default over the catalog's own answer. With `vision` still
carrying the image fact that derivation happened to recover it; the moment M5
removes the `vision ⇒ image` coupling it stops recovering anything, and every
wizard-created vision model comes out text-only. The fields now live at the top
level for all 200 models, the generator emits them to both the fixture and the
templates, and it refuses a catalog that omits them or puts them back under
`seed`.

**M4 must not make the form heavier.** Two more multi-selects on every model
would be a worse product than the problem being fixed. The modality arrays are
derivable from `type` for every model in the catalogue except the handful that
genuinely diverge, and `defaultModalities` already computes exactly that
derivation. So: the form shows the derived value, collapsed, and an
administrator expands it only to override — a sensible default for every knob,
configuration only where divergence is real. The Vision checkbox stays where it
is and reflects `inputModalities ∋ image`; ticking it adds the modality.

**M6c's acceptance is cross-ingress or it is nothing.** The 16 rows that
motivate `requiredModalities` appeared on all four ingresses, stream and
non-stream. A single-ingress run would have shown four and implied the other
twelve.

## 6. Rejected alternatives

**Mint a new endpoint kind (`audio_chat`) for models that mandate audio.**
Rejected. Their wire *is* `/v1/chat/completions`, so a new `EndpointKind` would
misstate the endpoint while requiring coordinated changes to `traffic_event`
labels, Prometheus labels, the cost-formula registry and the routing matcher.
Worse, it multiplies — `vision_chat`? `thinking_chat`? — and that is precisely
how the retired `audio` type was minted. A per-model request-shape constraint
is not a new endpoint.

**Retype `gpt-audio-*` to `tts` or `stt`.** Rejected on measurement. They are
served on `/v1/chat/completions` with a `messages` array, carry
`function_calling` and `streaming`, and return text plus audio with a
transcript in one turn — verified live, HTTP 200 with a 93 KB WAV. Typing them
`tts` breaks chat again *and* offers them on `/v1/audio/speech`, whose request
shape they do not accept: two dead endpoints instead of one.

**Delete `features.vision` outright.** Rejected. It is on a shipped contract;
deriving it costs nothing and breaks no client.

**Keep both fields and add a consistency check.** Rejected. Two fields that
must be kept equal are still two shapes; the check is a permanent tax that
exists only because the duplication was not removed.

## 7. What holds this in place

Rules that only exist in a document drift back. Each rule above needs something
that fails when it is broken.

| Rule | Gate | State |
|---|---|---|
| R2 — modality questions read modality arrays | `TestSmart_CapabilityFilter_VisionFeatureCannotOverrideTextOnlyModalities` builds a candidate with `features:["vision"]` and `inputModalities:["text"]` and requires it to lose. Reverting the filter to the feature list fails two tests. | **in place** |
| R4 — write-path invariant | `TestDefaultModalities_VisionAlwaysImpliesImageInput` plus the create-path tests; both failure modes mutation-tested. | **create only.** Update has no equivalent — the gap M1 closes. |
| R1 / R5 — guards cover both paths | Nothing asserts that a guard applied to the routing pool is also applied to the explicit-model path. The embeddings guard is missing exactly that half, and no test noticed. | **missing** |
| Catalog data satisfies the invariants | `catalog-modality-consistency.test.ts` reads the field the generator actually consumes (`seed.*`). | **in place** |
| R6 — `required ⊆ supported` | With M6. | **not started** |

The R1/R5 gap is the one worth naming: a guard's *coverage* is not something
any existing test can see, because each half passes its own tests in isolation.
A check that enumerates the selection paths and asserts every guard runs on all
of them is the only shape that catches it.

### Performance

`filterByCapability` runs on the routing path, so the change from reading
`Features` to reading `InputModalities` was measured rather than reasoned
about. `BenchmarkFilterByCapability_ImageNeed_{Legacy,Current}` runs both
implementations over shape-equivalent 50-model pools in one process — the
legacy body is kept in the bench file so the comparison is against the real
prior code, not a description of it.

| | median | min | max | allocs |
|---|---|---|---|---|
| legacy (reads `Features`) | 1218 ns/op | 1192 | 1562 | 12048 B / 6 |
| current (reads `InputModalities`) | 1225 ns/op | 1206 | 1412 | 12048 B / 6 |

n=11 × 50000 iterations. **+0.6% median, inside the noise** — the legacy
implementation's own spread is 31%. Allocations are identical.

Reasoning would have gotten this wrong. The first implementation drove the two
dimensions from a table of closures, which read better and measured **11%
slower**: the per-candidate call is indirect and does not inline. It was
rewritten as two explicit passes because this is the routing path, and the
number is the reason — not the shape of the code.

## 8. Consumer inventory

Every reader of a capability field, as of 2026-08-05. A new consumer must
appear here.

| Consumer | Reads | Question |
|---|---|---|
| `EndpointKindAcceptsModelType` | `type` | which endpoint |
| `filterByModality` (rule path) | `type` | which endpoint |
| `stage_routing.go` passthrough | `type` | which endpoint |
| `filterByCapability` (smart) | `inputModalities`, `features` | carried modalities; `function_calling`; `reasoning`; `structured_outputs` |
| `buildModelCatalog` (smart → LLM) | `features` | preference signal only — the hard filter has already run, so every candidate the LLM sees can already serve the request's modalities. Correct today because the R4 invariant keeps `vision` truthful; **self-resolving at M5**, when `vision` becomes derived and the catalog cannot disagree with the arrays even in principle. Deliberately NOT given a modality field of its own: the token budget is why the JSON keys are single letters, and a field the LLM does not need to make its choice is not worth the prompt. |
| `capability.Compatible` (embeddings) | `capabilityJson` | dimensions (range then set — R7), batch, encoding |
| `GET /v1/models` | all five | public contract |
| Admin UI model form | `type`, `features` | the only editable surface |

**`structured_outputs` is a separate tag from `json_mode`, and had to be.** Probed per MODEL
against each provider's own wire on 2026-08-19 (one request each, checking the response body and
not only the status): OpenAI serves `response_format.json_schema` on 16 of 19 probed rows,
Anthropic on 10 of 10 via `output_config.format`, Gemini on 7 of 7 via
`generationConfig.responseSchema`, Cohere on 5 of 5, Moonshot on 10 of 11, DeepSeek on 0 of 2.
`json_mode` — which describes the weaker `response_format: {type: json_object}` — disagrees with
that on exactly the rows that matter: `gpt-4-turbo` CARRIES `json_mode` and answers 400 to a
`json_schema`, while every `claude-*`, every `command-*` and the whole o-series carry no
`json_mode` and serve one correctly. Reusing it would have excluded Anthropic, Cohere and the
o-series from every structured-output request — an enumeration refusing what the provider serves.

Two of OpenAI's three refusals took a second probe to establish. `gpt-audio-1.5` and
`gpt-audio-mini` answer a plain text body with 400 "requires that either input content or output
modality contains audio" — a verdict about MODALITY that says nothing about `response_format`.
Re-asked with audio output declared they answer 400 naming `response_format`, and the same request
minus the schema answers 200. The control is what turns an absence into a measurement.

An untagged row is EXCLUDED, not deferred: the filter's row-level fail-open rescues a row
declaring no features at all, but a row declaring `function_calling, streaming` and nothing else
looks described. So "probed and it refuses" and "nobody asked" have the same effect, and only one
is a decision. `TestCatalog_EveryRoutableRowOnAProbedAdapterHasAVerdict` fails the build on the
second case for the six adapters a key exists for. Rows on the thirteen template-only adapters —
`azure-openai`, `bedrock`, `vertex`, `fireworks`, `together`, `groq`, `mistral`, `xai`, `glm`,
`minimax`, `perplexity`, `huggingface`, `replicate` — stay untagged and therefore excluded from
structured-output routing. That is an accepted limit rather than a wire's behaviour inferred from
a sibling model's, and it lifts when somebody can ask those wires.

The tag is read only when the ROUTER picks the model. A caller who names a model owns that model's
limits: `gpt-4-turbo` and the DeepSeek family refuse with a loud 400, and a 4xx is
`classNoFailoverNoRetry` so it terminates rather than sliding onto a worse target. One candidate
does not fail loudly at all — `kimi-k2.5` accepts the field and answers in prose with HTTP 200 —
and nothing downstream can turn that into an error, so declining to CHOOSE it is the only place
the mismatch can be prevented. `json_object` is deliberately not a constraint: every target either
honours it natively or is given a system instruction that does.
