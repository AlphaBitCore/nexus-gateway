# Canonical waist architecture

Every consumer that needs to know *what a request means* — routing, cache
identity, compliance, metering, audit, and the operator's view of stored traffic
— reads one representation, produced by one decode. This document states that
contract, the invariants that make it hold, the measured ways the current
implementation departs from it, and the constraints that bind any change to it.

It is written for the session that implements the change. Where a conclusion was
reached and later refuted, both are recorded: an implementation that rebuilds a
refuted argument is worse off than one that never saw it.

## 1. What the gateway owes a request

Eight obligations, stated without reference to implementation. Seven of the eight
need the meaning of the request, not its bytes.

| # | Obligation | Needs |
|---|---|---|
| F1 | Admission: may this caller use this model, within quota | identity + model id |
| F2 | Routing: resolve to a provider, model, credential, failover order | **meaning** (size, modality, tool needs) |
| F3 | Cache: reuse an answer for a request that means the same thing | **meaning** (cache identity is semantic identity) |
| F4 | Compliance: inspect before egress, before delivery, and mid-stream, **with the power to edit** | **meaning** + an address the edit can be written back to |
| F5 | Translation: the caller's wire and the provider's wire may differ | **meaning** + the target wire's encoding capability |
| F6 | Metering: tokens and cost | **meaning** (volume, modality) |
| F7 | Audit: reviewable and defensible after the fact | **meaning** + the original bytes |
| F8 | Observability: an operator can see the real content and the verdict on it | **meaning** |

One requirement cuts across all eight: the ingress surface is multimodal. Of the
sixteen mounted ingresses, ten carry something other than text as their primary
content. Images, audio, video and documents are *content*, not attachments; a
model that treats content as string-shaped by default has already lost most of
the surface.

## 2. Architectural requirements

**A1 — one representation of meaning.** Seven consumers need the same thing. If
each reads the raw wire, "what does this request mean" has seven possibly
different answers and no mechanism makes them agree. Decode once; everyone reads
that. Only codecs touch wire bytes.

**A2 — classify meaning by endpoint semantics, not by vendor.** The test is
whether a semantic question exists that one endpoint can answer and another
structurally cannot. "What did the assistant say in turn three" separates chat
from embeddings — different specs. Nothing separates `/v1/chat/completions` from
`/v1/messages` or `/v1/responses`: `input` versus `messages`, `output` versus
`content`, are spellings. One spec, several codecs. N endpoint semantics × M
wires costs N + M, not N × M.

**A3 — reading and writing are separate capabilities.** "Can this body be decoded
into the canonical spec" and "can an edit reach the caller's wire without loss"
are independent questions. Every chat wire answers the first yes. Only some
answer the second yes. Collapsing them into one predicate is what kept three
wires on a weaker scan model for no benefit.

**A4 — capability is declared at registration, never inferred at the use site.**
A predicate that asks whose wire this is cannot answer what the wire can do, and
the correlation between the two breaks in practice — see §5.

## 3. The refuted argument, and what replaced it

A design iteration argued that the canonical spec must be a **superset** of every
wire rather than one member of the set, on the grounds that a lossy canonical
forces a chain: lossy → cannot re-encode → must edit in place → the scanned
object and the forwarded bytes become two different things → a family of defects
follows.

**The chain does not hold, and the implementation session should not rebuild
it.** The property that matters — the forwarded bytes are produced by the object
that was scanned — has two possible implementations:

1. **Re-encode** canonical → bytes. This *does* require a lossless canonical,
   otherwise the encode drops whatever the canonical does not model.
2. **Edit in place**: keep the caller's bytes and apply edits at the positions the
   decode recorded. The bytes *are* the object they were decoded from. Nothing is
   rebuilt, so nothing can be lost — **regardless of what the canonical models**.

The second satisfies the property with no requirement on the canonical at all,
and it is what this repository already does on the chat leg
(`ApplySpans` → `RewriteCanonicalRequestContent`, which additionally proves
provenance by refusing when the body's message count disagrees with the
payload's). "Cannot re-encode" was treated as the defect; it is the correct
answer.

Of the five defects the superset argument was built on, four are unrelated to the
canonical's expressiveness: duplicate JSON keys and case-variant field names are
byte-layer parsing ambiguities, duplicate multipart parts likewise, and
block-order/slot-order mismatch is an addressing bug fixed by address-based spans.
Only cross-wire translation fidelity genuinely needs a superset — and that is a
different claim from defect closure.

A superset canonical may still be worth building for extensibility and
conceptual integrity. If it is built, it must be justified on those grounds and
sequenced against the constraints in §7, not presented as the fix for the defects
in §6.

## 4. Invariants

**I1 — meaning exists in one place.** Routing, cache identity, compliance,
metering, audit and view-time rendering read the canonical payload. Only codecs
read wire bytes. There is no second understanding layer below the waist.

**I2 — decode precedes the first stage that needs meaning.** Since nearly every
stage does, decode is the first step, not something a later stage performs on the
side. Any ordering where canonical appears mid-pipeline forces the stages before
it to read raw wire.

**I3 — the bytes forwarded are produced by the object that was scanned.** An
in-place edit gives this by construction. It breaks the moment two
representations are involved: decoding with one model and writing back with
another writes one channel's edit into another channel's slot. See §6, G1.

**I4 — an address carries as many indices as the wire has nesting.** A single
"which segment" coordinate cannot name a text block inside a tool result inside a
message. Address types match the nesting depth of what they address.

**I5 — media is content.** Images, audio, video and documents are content blocks
beside text, on the same addressing scheme and under the same scanning
obligation. "We do not inspect text inside images" is an acceptable product
decision, but it must be a declaration in the spec, not a missing branch in a
projection function: a declaration can be seen, questioned and changed; a missing
branch is only ever forgotten.

**I6 — the set of channels to scan is derived from the spec, never hand-written.**
Adding a block type extends the obligation automatically and fails any scanner
that cannot handle it. A hand-written list cannot notice the entry it omits —
reasoning, tool arguments, tool results and media were each omitted in turn.

**I7 — lossless is the default; a discard is a recorded event, not a category.**
On the same wire there is no reason to drop anything: providers reject fields
they do not know, which means they accept their own. Carrying a field back costs
nothing. A field is dropped only in cross-wire translation, and the reason is a
checkable property of the target — it has no slot for this — not a judgement that
the field was unimportant. Fields that appear inert routinely are not: response
padding defends against length-based inference, a frame's sequence coordinate
reassembles interleaved output, a vendor request id is what support asks for on a
failure.

**I8 — the scan verdict is recorded, and it is not the same thing as the
disposition.** Four outcomes: fully decoded; partially decoded (something
unrecognised, or content the gateway provably cannot see, such as server-side
conversation state or a stored prompt template); best-effort text only (no codec
for this traffic); nothing readable. Only the last obliges a refusal. The other
three proceed and are recorded. **There must be no outcome where "approved" and
"never inspected" are indistinguishable in the record.**

**I9 — provider evolution is a tag, never a refusal.** Vendors add fields to their
own wires continuously, normally with a compatibility window. Treating an
unrecognised field as a reason to fail closed converts another company's release
schedule into this system's outage, on a timetable it does not control — and a
mechanism with that property gets switched off, after which there is no tag
either. Record provider, wire and key path; let the request proceed; count the
records. An unrecognised member at a *content* position additionally degrades the
verdict, because that position is known to carry user text. Refusing remains an
operator opt-in.

**I10 — the gateway's own state never enters the payload.** Routing decisions,
caller identity, tenant, correlation ids, coverage verdicts and cost attribution
travel in the request context and the audit record. The canonical payload carries
provider-wire data only. Providers reject unknown fields, so a leaked internal
carrier is a failed call, not a tolerated one.

**I11 — what the gateway changed is recorded, and the record is total.** Codecs
fill fields the caller omitted — a required output ceiling, a reasoning contract a
model generation demands, a schema keyword a target rejects. Each fill makes the
forwarded body differ from the received one, so each is recorded and surfaced
(`X-Nexus-Coerced`, routing audit trace). A ledger covering *some* fills is worse
than none: the entries it carries read as exhaustive.

**I12 — acceptance is field-level, not signature-level.** A → canonical → B →
canonical → A must return A field for field, across {request, response} ×
{streaming, non-streaming} for every (wire, endpoint kind) pair. The only
permitted losses are those the spec declares it does not carry, and that
declaration is derived from the spec rather than hand-listed. Comparing canonical
*signatures* — roles, text, media, tool declarations — passes while
`cache_control`, `metadata.user_id`, `mcp_servers` and `container` are silently
dropped, because none of them is in the signature.

## 5. Capability is per model generation, not per vendor

`IsOpenAIFamily` is consulted at several call sites that each ask a different
capability question: does this wire terminate its SSE with `[DONE]`, is this wire
the canonical chat body, does egress need reshaping. One predicate, several
questions, and its name identifies which one at none of them. It also cannot
express "this OpenAI endpoint does not speak the OpenAI chat wire", which is
exactly `/v1/responses`.

The correlation breaks inside a single vendor. Measured: stripping the reasoning
attestation from a Gemini function call is accepted by one model generation and
rejected with `400 … missing a thought_signature` by the next. Anthropic moved
`thinking.type` from `enabled` to `adaptive` with the level relocated to
`output_config.effort`, and the older shape now returns 400 on the newer family.
Capability therefore cannot be derived from the vendor, nor even from the wire.
It is declared where the ingress is registered and read by name where it is used,
and the declaration set is enumerable so that adding a wire fails until every
question is answered.

### 5.1 A routed conversation outruns the credential its last turn minted

The capability question above has a harder sibling that only appears once routing
is free to move between vendors mid-conversation, and production shows it does.

Observed on prod, one `auto` conversation inside ninety seconds:

    10:42:25  auto -> openai         gpt-4o-mini        200
    10:42:27  auto -> google-gemini  gemini-3.6-flash   400
    10:42:56  auto -> google-gemini  gemini-3.6-flash   200
    10:42:59  auto -> openai         gpt-5.6-terra      200
    10:43:01  auto -> google-gemini  gemini-3.6-flash   400
    10:43:34  auto -> openai         gpt-5.6-luna       200

Every 400 is a Gemini leg immediately following an OpenAI leg, and every one of
them reads `Function call is missing a thought_signature in functionCall parts`.

**This is not a field the waist dropped.** The Gemini codec handles the
attestation correctly in all four places it appears — the streaming decoder reads
it off the Part, the native ingress reads it, the request encoder writes it back
onto the Part it belongs to, and the buffered accumulator carries it. There was
nothing to carry: the turn that produced that tool call was answered by OpenAI,
so a Gemini attestation for it was never minted. Gemini is asking for its own
signature on a function call Gemini never made.

The general statement: **a provider-native opaque credential is bound to the
model generation that minted it, and a conversation that is re-routed leaves that
credential behind.** Anthropic's thinking signature is the same shape of thing.
No codec can fix this, because a codec can drop a credential or reshape it but
cannot mint a valid one.

Two levers exist, and one of them is not a fix at all.

**Route stickiness — pinning a tool conversation to the provider that opened it —
is not an option to be weighed. It is the failure state.** Sending each request
to the model that suits it, across providers, turn by turn, is the capability
`auto` exists to deliver and the strongest thing the gateway offers. A rule that
freezes a conversation onto whichever provider happened to answer its first turn
does not solve the problem; it removes the feature and reports success. If tool
conversations can only be served by staying put, then `auto` does not work for
tool use — which is most of what agents do.

So the requirement is stated positively: **a conversation carrying tool calls
must remain portable across provider families, so that routing stays free on
every turn.** Portability is the deliverable; stickiness is what it looks like
when the deliverable is missing.

**The right answer is the one the codecs already practise.** They routinely
degrade a provider-native construct the target cannot take — dropping a field the
route does not support, refusing a part kind rather than silently losing it. The
same technique applies here: when a conversation is encoded for a target and it
carries an assistant tool-call turn minted by a *different* provider, that turn
must not be emitted as the target's native tool-call construct, because the
native construct is what carries the credential requirement. It is rendered
instead in a form the target accepts without one.

Two things must exist for a codec to make that decision:

1. **Canonical carries tool-call provenance** — which provider and which model
   generation minted each assistant tool call — not merely the credential. A
   codec that only sees "no signature present" cannot tell a foreign-minted turn
   from a same-provider turn whose signature the waist lost, and those two need
   opposite handling: degrade the first, and fail loudly on the second.
2. **The degraded rendering is declared per target**, next to the capability
   declarations above, so a wire that has no signature-free way to express a
   prior tool call fails at registration rather than at 400.

Acceptance: an `auto` conversation containing tool calls survives being routed
across provider families for N consecutive turns, with no upstream 4xx and no
turn silently dropped from the history. A target that genuinely cannot represent
a foreign-minted tool turn is refused before dispatch, with a reason, rather than
sent and rejected upstream.

## 6. Where the implementation departs from this today

Sixteen ingresses were audited individually. The findings reduce to five root
causes; the per-ingress evidence lives in the task ledger.

**G1 — the scanned object and the forwarded bytes are not always the same.** The
Compliance Proxy decodes with the registry (canonical blocks) and hands the
result to a traffic adapter's `RewriteRequestBody`, which walks its own wire's
slots in wire order and writes block *i* into slot *i*. Measured on an Anthropic
request: nine blocks in, three slots written, a `document` block's SSN left on the
wire, a `tool_result` slot overwritten with a different block's text — and the
audit row said `action=redact`. This is corruption plus a false record, not a
coverage gap. `positionalRewriteAligned` is the provenance test that closes it:
the positional contract holds only for payloads built by
`PayloadFromTextSegments`, the adapter-extraction fallback and the only producer
whose ordering is the adapter's own.

**G2 — the request stage is the only stage not at the waist.** Response,
streaming, audit write and view-time rendering all read canonical. The request
stage gates on vendor lineage, so Anthropic, Gemini and `/v1/responses` fall to a
flat text-segment extraction that cannot represent a tool result or a reasoning
turn at all.

**G3 — absence reads as clean.** An extractor that recognises no shape returns no
segments and no error; every content hook abstains; the request is approved. The
audit row for a request that was never inspected is identical to one that was
inspected and found clean.

**G4 — one downstream filter discards non-OpenAI tool calls.** Tool-call entries
whose discriminator is not `function` are dropped after extraction. Anthropic
spells it `tool_use`, Gemini has no discriminator, `/v1/responses` spells it
`function_call`. Three wires lose tool-call arguments, and no fail-closed fires
because the masking guard sees an adapter that implements the interface and an
empty set to mask.

**G5 — three components disagree about the same wire.** For one request the MITM
leg scans at the waist, the gateway's native ingress does not, and the Traffic
drawer renders at the waist. The operator can see the tool argument holding a
secret next to the audit row that says `approve`.

## 7. Constraints on any implementation

These bind regardless of scope, and none of them is a cost argument.

**C1 — `compliance_coverage` is a shipped, persisted contract.** It carries the
value set `prompt-only` / `none`, is written from about ten call sites, and
travels to the Hub over a stable binary field id with **no version
discriminator**. Widening its vocabulary is not additive: during a rolling deploy
a new gateway emits a new word, an older Hub stores it under the old contract, and
nothing afterwards tells them apart. A richer verdict goes in a new field; the
existing value set does not change.

**C2 — `guardrail.coverage` is a required enum in a shipped OpenAPI spec**, and
the API reference instructs callers to read it before trusting the action. Folding
channel completeness into `full` re-means a value customers gate on. Either keep
`full` meaning policy completeness and add a separate channel-completeness field,
or version the spec with a deprecation window.

**C3 — cache identity has no per-consumer rollback.** Cache identity is derived
from canonical; if canonical changes, keys change. Switching invalidates the
existing key space in one step, and rolling back writes under the new identity
while reading under the old. There is no double-run comparison that detects this,
because the difference only appears across requests. Any change here needs a
dual-write or shadow-key period planned in advance.

**C4 — cache identity must be composed with context identity.** Tenant and
credential domain do not enter the canonical payload (I10), and cache identity is
computed from the canonical payload. Composed naively, the key function
structurally cannot see the tenant. The key is semantic identity ⊕ context
identity; "cross-wire cache hits work naturally" is otherwise a description of a
cross-tenant leak.

**C5 — cache lookup must not precede the request-side compliance verdict.** A
lookup before redaction keys on unredacted content, and a hit returns directly,
bypassing a verdict that would have denied.

**C6 — audit rows change meaning permanently, with no generation marker.** Rows
written before and after differ in what `approve` means, and no column records
which generation produced them. Retrospective review must otherwise be segmented
by timestamp.

**C7 — hooks can only be scoped by service.** `applicableIngress` selects
`ALL` / `AI_GATEWAY` / `COMPLIANCE_PROXY` / `AGENT` — never an endpoint or a wire.
There is consequently no dial with which to enable newly-scanned channels
gradually. A shadow-enforcement stage (scan, record what would have changed, do
not apply) and an endpoint-kind scoping dimension are prerequisites, not
follow-ups.

**C8 — an encode's output is a triple, not a body.** The existing adapter contract
already returns body plus rewrites plus a URL override, and at least one codec
selects between two upstream endpoints through it. Beta headers additionally
change which body shapes are legal. Any interface written as
`Encode(canonical) → bytes` is narrower than the contract already in production.

**C9 — compliance has never run on real production traffic.** All hook
configuration rows are disabled in production; no customer request has passed
through the pipeline there. Any plan whose first phase is "measure a baseline"
must first make the pipeline runnable and observable, or the baseline is empty by
construction.

## 8. The change, in dependency order

**N1 — retire positional write-back.** Consumers use address-based spans; the
positional projection is used only where the adapter's own extraction produced
the content. Closes G1, the only corrupting defect. *(Guard landed; the wider
retirement is tracked separately.)*

**N2 — separate the read decision from the write decision.** Decode every chat
wire for scanning; hand back a canonical body — and therefore a canonical
write-back — only where a rewriter can address those bytes. A caller holding a
payload without a body has full scanning and must fail closed on a redaction
rather than degrade to a positional rewriter. Closes G2.

**N3 — remove the discriminator filter that drops non-OpenAI tool calls.** Closes
G4.

**N4 — one strict parse at the ingress boundary.** Reject duplicate top-level
keys, reject duplicate multipart parts, normalise case in field lookup. Closes
the "spelling equals bypass" class in one place at the byte layer. Note that
providers themselves reject some of these — OpenAI returns 400 on duplicate JSON
keys — so the exposure is per-provider and should be measured before the class is
prioritised.

**N5 — model tiered cache-write accounting.** Cache writes are billed per TTL
tier and the canonical `Usage` carries one undifferentiated field, so the tier
premium is not attributed. Separately, at least one vendor reports billed units
distinctly from token counts, and the two are not derivable from each other.

**N6 — make the compliance pipeline runnable in production** so that any
subsequent measurement has a baseline (C9).

A superset canonical, if pursued, sequences after these and is justified by §3's
surviving rationale rather than by §6.

## 9. Known unknowns

- Six of the seven endpoint kinds have **no captured real traffic** at all;
  only chat does. A spec for the others can currently be derived only from codec
  source, which carries the same blind spots as the codecs themselves.
- One Anthropic response field is present on every response and null across
  every stop reason and model generation probed. Its populated shape is unknown
  and must be observed before it can be classified.
- Whether the "spelling equals bypass" vectors reach each provider is measured
  for one provider only.

## References

- [hook-architecture.md](../../services/ai-gateway/hook-architecture.md) — the policy layer that consumes the canonical payload
- [normalization-architecture.md](../../services/ai-gateway/normalization-architecture.md) — the payload contract, the tiered dispatch, and the in-place rewriters
- [endpoint-typology-architecture.md](endpoint-typology-architecture.md) — wire shapes and endpoint kinds
- [audit-pipeline-architecture.md](../observability/audit-pipeline-architecture.md) — where the record is written and what it must distinguish
