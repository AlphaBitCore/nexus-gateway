---
name: wire-capability-probe
description: Establish what a provider wire can and cannot carry, and fix the gateway when the answer is "we send the wrong form". Use when an upstream refuses a request (4xx/422 naming a content type, a field, a modality, a URL form), when a catalog capability is about to be written from a refusal, or when a codec translation is being added or changed. Turns a refusal into one of three verdicts — the wire cannot, we send the wrong form, or the model cannot — each with the evidence that distinguishes it. Trigger keywords: upstream refused, 422, unrecognized content type, does not support, capability probe, wire capability, codec translation, /wire-capability-probe.
---

# wire-capability-probe

An upstream refusal is not an answer. It is one of three different answers wearing the same clothes:

| Verdict | What it means | What fixes it |
|---|---|---|
| **The wire cannot** | This API has no shape for what the caller sent | Refuse in our words, naming what and why; record the capability |
| **We send the wrong form** | The wire carries it, under a different name or in a different place | A codec mapping |
| **The model cannot** | The wire carries it; this model does not | Routing / catalog, not the codec |

Reading the refusal message tells you which one it is **only when you are lucky**. This skill is the procedure for when you are not, and its rules were each bought by a wrong verdict.

---

## The core procedure: three probes, never one

For every refusal, run all three. The third is the one that changes verdicts.

```
1. THROUGH THE GATEWAY   — reproduce it. This is the symptom.
2. DIRECT, OUR SHAPE     — same bytes, straight to the vendor, no gateway.
                           Same refusal?  → not a mistranslation.
                           Different?     → the gateway is the problem. Stop here.
3. DIRECT, VENDOR SHAPE  — the form the vendor's own docs describe.
                           200?           → "we send the wrong form". Codec mapping.
                           Refused?       → the wire genuinely cannot.
```

**Worked example, and the reason this skill exists.** Cohere answered
`422 unrecognized content type 'file'` for a document. Probe 2 reproduced it exactly, which
looked like confirmation. Probe 3 sent the same document in Cohere's documented top-level
`documents` array: **200, and the model answered from the document's contents**. The verdict
flipped from "Cohere cannot take documents" — which was about to be written into the model
catalog — to "our codec has an unused mapping".

Three refusals probed the same day gave three different verdicts:

| Vendor | Our shape, direct | Vendor's documented shape | Verdict |
|---|---|---|---|
| Cohere | 422 unrecognized content type | top-level `documents` → 200, answered | **our gap** |
| Moonshot | 400, identical to via-gateway | `GET /v1/files` → 200; upload-then-reference, no inline form | **no inline form** — an upload capability, not a mapping |
| DeepSeek | 400 unknown variant | nothing documented | **genuinely unsupported** |

---

## Rule 1 — a negative control, or the answer proves nothing

A 200 with the right answer in it does not prove the model READ what you sent. Models guess
plausibly.

Run the same request **without** the thing under test. If the model still answers, your probe was
never measuring what you thought.

```
with documents    → "The reference number is 52903."
without documents → "I'm unable to provide the reference number as there is no document…"
```

That second line is what makes the first one evidence.

---

## Rule 2 — the anchor rides in the bytes

Put a value in the media that cannot be guessed and does not appear in the prompt: a reference
number inside the PDF, a phrase spoken in the audio. Then the assertion is "did the answer contain
the anchor", and a model that never received the media cannot pass.

Fixtures with anchors live in `tests/fixtures/media/`:

| File | Anchor |
|---|---|
| `doc.md` | reference number **52903** |
| `doc.pdf` | reference number **38617** |
| `image.png` / `image.jpg` | a red / blue square |
| `speech.wav` | the spoken phrase "regression fixture" |
| `clip.mp4`, `image.gif/heic/svg`, `generated.png` | shape and format coverage |

---

## Rule 3 — suspect the fixture before the vendor

Three times in one session a broken fixture impersonated a capability limit:

- `https://example.invalid/x.png` — **unresolvable by definition**. The 400 said nothing about
  whether the vendor accepts external URLs; a real reachable URL was needed to learn that it does
  not.
- A hand-built 1×1 PNG the vendor answered `failed to decode image` for. That was the fixture,
  not the wire. The real fixture answered 200.
- `"xxx"` as a base64 payload in a test asserting a media-type default — it LOOKS malformed and is
  valid unpadded base64, so the row was asserting a belief the measurements contradicted.

Before concluding from a refusal: is the payload valid, is the URL reachable, is the model the one
you meant, does the escape survive the shell and the JSON encoder. A fixture that cannot succeed
proves nothing about a wire.

---

## Rule 4 — measure the disagreement, do not reason about it

When two code paths disagree about a wire, one probe settles it and no amount of argument does.

Two copies of a data-URL parser disagreed on whether to validate base64. The probe: the same PNG,
padded and unpadded, to two vendors.

```
                  anthropic          gemini
padded            200                200, answered "Red"
unpadded          400 invalid        200, answered "Red"
base64url         —                  200, answered "Red"
newline-wrapped   —                  400 Invalid value
```

That table did three things no reasoning could: it showed the strict copy was right for one
vendor, wrong for another, that "which encodings are acceptable" is a per-vendor question, and
that the answer was neither copy — normalize to the form all of them accept.

---

## Rule 5 — send the codec's ACTUAL OUTPUT to the vendor

An offline assertion about a wire is a belief. Dump the bytes the codec emits and POST them.

```go
// A test that writes the encoded body out, skipped unless asked for.
func TestX_DumpEncodedBodyForLiveProbe(t *testing.T) {
    path := os.Getenv("NEXUS_<VENDOR>_DUMP")
    if path == "" {
        t.Skip("set NEXUS_<VENDOR>_DUMP=<file> to write the encoded body for a live probe")
    }
    body, err := encode(...)
    ...
    os.WriteFile(path, body, 0o600)
}
```

Then `curl --data @<file>` at the vendor. This is what closes the loop: shape assertions passed
while the emitted body was still wrong more than once.

---

## Rule 6 — refuse in our own words

When the verdict is "the wire cannot", forwarding to the vendor's refusal is not neutral — it
hands the caller a message that describes the vendor's parser, not their problem.

```go
return &provcore.ProviderError{
    Status: http.StatusBadRequest,
    Code:   provcore.CodeInvalidRequest,
    Type:   "nexus_field_unsupported",
    Message: "nexus: <wire> carries documents as text, and " + mediaType + " is not text. " +
        "Extracting it would change what the model is given, so send the document's text instead",
}
```

Name **what was sent**, **why this wire cannot carry it**, and **what to send instead**. Check the
branch order too: a field that is PRESENT but unusable must not be reported as absent — testing
for a `data:` prefix instead of presence told callers they had sent no `file_data` when they had.

---

## Rule 7 — the destination is not always the structural counterpart

A content part does not have to map to a content part. Cohere puts documents in a top-level
`documents` array; the mapping lifts the part out of the message and appends it there.

So when probe 3 fails, ask where else the vendor puts the concept before concluding the wire
cannot. The vendor's own reference, read for the CONCEPT rather than for the field name, is the
input to this.

Bound the mapping by what the destination actually is. `documents` carries TEXT, so a markdown
file maps and a PDF does not — extracting it silently would change what the model was given.

---

## Rule 8 — the copy of this parser you are about to write is the third one

Before adding a helper to a codec, grep for it. Two copies of `ParseDataURL` existed and
**disagreed**, so one canonical body got two verdicts depending on which model the router picked.

Shared helpers live in `packages/ai-gateway/internal/providers/specutil/`. Per-wire QUIRKS stay in
the adapter that talks to that wire (§3a Rule 3) — the split is: the grammar is shared, the policy
is per-wire, and strictness is policy. Verify with a probe which side of that line a rule falls on
rather than assuming.

---

## Rule 9 — an OpenAI-compatible adapter has TWO doors, and production uses the other one

A codec that speaks an OpenAI-compatible wire is reached through two entry points, and which one
a request takes is decided by the CALLER'S INGRESS — something the adapter cannot see:

| Entry point | Taken when |
|---|---|
| `EncodeRequest` | **cross-format** — the body was bridged from `/v1/messages`, Gemini, or any non-OpenAI ingress |
| `RewriteNative` | **same-spec** — an OpenAI-shaped request to an OpenAI-compatible provider. **This is the common path.** |

A rule placed on one of them is silently absent from the other, and unit tests that drive only the
first pass while production is unchanged.

That is not hypothetical: a content check added to Moonshot's `EncodeRequest` left every test
green and the live path untouched, and the only thing that caught it was replaying the class
against production after the deploy — the vendor's own refusal came back, unchanged. The same
asymmetry in the opposite direction is why that adapter's field rules are assembled into a
Contract both doors consume.

So: **put the rule on both doors, and write a test that drives each.** The stronger form is to
drive `NewSpec(...)`'s codec rather than constructing the wrapper directly — a test that builds
the wrapped codec by hand proves the wrapper works and says nothing about whether the adapter
uses it. Unwiring it in `spec.go` must redden something.

---

## Rule 10 — a translation runs on every request

Measure before and after. The reference point is what makes a number mean something: a real
upstream call is **200–2000 ms**, so a 200 ns function is noise and should be said to be noise
rather than filed as a finding.

- cheap byte scan before any parse; assert the no-op path returns the body **byte-identical**
- `go test -bench=. -benchmem -run=^$ -count=5`, report the median
- every benchmark must **assert the branch it took** before timing — three benchmarks in one
  session measured the cheap path under an alarming name
- state the absolute cost and the fraction of a request, not just ns/op

---

## Rule 11 — mutation-prove the guard, aimed at the intended violation

A test that cannot go red is not a weak test, it is not a test.

Restore the behaviour the fix removed and require the test to redden **with a message naming the
real problem**. "It goes red" is not enough — it must go red for the violation it was written to
catch. In one session five mutation proofs were attempted and three initially stayed green.

The strongest form: the mutation reproduces the production numbers. Restoring the streaming usage
bug reddened with `input_tokens: non-streaming 2717, streaming 0` — the same figures measured
live.

---

## Provider keys and safety

Keys live in `~/.nexus/provider-keys.json`. They never enter chat, a commit, a log, or a file in
the repo. Read them in-process:

```python
import json, os
K = json.load(open(os.path.expanduser('~/.nexus/provider-keys.json')))['<VENDOR>_API_KEY']
```

Prod probing uses a virtual key created through the admin API (`projectId` + `expiresAt`, lands
`pending`, then `POST .../approve`). Probe traffic lands in `traffic_event` indistinguishable from
customer traffic — name probe VKs so a later 4xx sweep can tell them apart.

**Do not run the full ai-gateway smoke to answer a capability question.** It is expensive and
billed. Probe the one wire in question.

---

## Checklist

- [ ] All three probes run; verdict stated with the evidence for it
- [ ] Negative control run — the answer is impossible without the thing under test
- [ ] Anchor rides in the bytes, not in the prompt
- [ ] Fixture validated: payload decodes, URL resolves, model is the intended one
- [ ] Disagreements between code paths settled by measurement, not argument
- [ ] The codec's actual emitted body sent to the vendor
- [ ] Refusals name what was sent, why, and what to send instead; branch order checked
- [ ] Grepped for an existing helper before writing one
- [ ] On an OpenAI-compatible adapter: the rule is on BOTH `EncodeRequest` and `RewriteNative`, and a test drives the codec `NewSpec` actually ships
- [ ] Replayed the class against production AFTER deploying — a green test suite is not evidence the live path changed
- [ ] Hot path measured with a stated reference point; no-op path asserted byte-identical
- [ ] Every new guard mutation-proved against the violation it exists to catch
- [ ] `provider-adapter-architecture.md` updated in the same commit (code/doc lockstep)

## References

- `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` — §3a, the binding adapter rules
- `packages/ai-gateway/internal/providers/specs/anthropic/codec/` — the reference shape for a spec codec
- `packages/ai-gateway/internal/providers/specutil/` — shared codec helpers
- `tests/fixtures/media/` — anchored media fixtures
- `.claude/skills/adapter-conformance-check/SKILL.md` — the §3a audit this pairs with
