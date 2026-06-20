# Nexus Gateway Benchmark — Results Summary (for James)

**One page, plain English. Local validation numbers — the publishable full-scale run happens on AWS.**

## What we measured and why it matters

We compared three AI gateways — **Nexus**, **LiteLLM**, **Bifrost** — on the same machine, same model (`gpt-4o-mini`), same prompts, with caching off, so it's a fair head-to-head of raw gateway behavior. We separately measured Nexus's governance features (semantic cache, PII enforcement), which the other two don't have, so those are labeled as **feature demos, not head-to-head**.

The old v1 benchmark PDFs are not trustworthy as a final comparison: Nexus had caching on (44% hit rate) while the others didn't, and the load ran off a laptop with uncontrolled network jitter. This v2 effort fixes the methodology.

## Fair comparison — short chat (S-01)

| | Nexus | LiteLLM | Bifrost |
|---|--:|--:|--:|
| Time-to-first-token (median) | 1327 ms | 517 ms | 418 ms |
| Reliability | 100% | 100% | 100% |

Nexus's median is higher — but that's **not** slower routing. It's the compliance pipeline running on every request (see below). All three are completely reliable at this scale.

## Why Nexus's latency is higher — and it's by design

We ran Nexus with its compliance hooks **on** and **off**:

| Nexus config | TTFT median |
|---|--:|
| Hooks ON (PII scan + keyword block on every request) | 1327 ms |
| Hooks OFF (pure routing) | 367 ms |

**The compliance pipeline adds ~960 ms per request.** With hooks off, Nexus is actually *faster* than LiteLLM and Bifrost. So the latency gap is the cost of governance, not gateway inefficiency — and it's a knob, not a fixed tax.

## Nexus-only feature: semantic cache

On repeated prompts, Nexus serves the answer from its semantic cache instead of calling the model — cutting time-to-first-token by **~1838 ms** and avoiding the provider API call (and its cost) entirely. LiteLLM and Bifrost have no equivalent, so this is a Nexus capability demo, not a race.

## Nexus-only feature: PII / compliance enforcement

We sent prompts containing fake PII (fake SSNs, card numbers, phones, emails):
- **100%** of PII-containing prompts were **blocked** before reaching OpenAI.
- **0%** false positives — clean prompts passed through normally.
- Blocks happen in **single-digit milliseconds**, consuming **zero model tokens**, and every rejection is **logged with the policy reason**.

The same PII prompts sent through LiteLLM and Bifrost go straight to the model — no block, no log. That contrast is the core selling point: Nexus is a governance layer, not just a router.

## Bottom line

- All three gateways are reliable.
- On raw speed, the thin proxies are faster — because Nexus is doing compliance work they don't do.
- Turn Nexus's hooks off and it's the fastest of the three.
- Nexus uniquely provides: per-team virtual keys, full traffic audit, semantic caching, and enforced PII/compliance policy.

## Next step

The numbers above are small-sample local runs for this review. The **publishable** comparison — every scenario at 3,000+ requests per gateway, on controlled AWS hardware — runs once the new AMI lands. Methodology, scenarios, and harness are all locked and ready; AWS day is pure execution.
