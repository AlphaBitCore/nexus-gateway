# Nexus Gateway — Demo Guide (for James)

A walkthrough of the four things you asked to see: **model setup**, **virtual key creation**, **traffic/log visibility**, and **PII/compliance rejection**. Each section says exactly where to click in the Control Plane UI (http://localhost:3000 locally; the AMI's CP URL on AWS) and what you'll see.

> Screenshot placeholders are marked `[screenshot: …]` — capture these live during the demo or pre-capture them for the deck.

---

## A. Model setup

**Where:** Control Plane UI → **AI Gateway → Providers & Models**

[screenshot: provider/model list]

What you'll see:
- Provider: **OpenAI**, status **active**, bound to credential **`openai-prod`**.
- Model: **`gpt-4o-mini`**, status **active**, routable.
- The provider credential (the OpenAI API key) is stored **encrypted** in the Nexus database — it never appears in plaintext in the UI, config, or logs. Admins update it via the credential rotation flow, not by editing a file.

Talking point: *"The org's OpenAI key lives inside Nexus, encrypted. Application teams never touch it — they get a virtual key instead."*

---

## B. Virtual key creation

**Where:** AI Gateway → **Virtual Keys → New Key**

[screenshot: virtual key creation form]
[screenshot: one-time plaintext key reveal]

What you'll do:
- Name: e.g. `benchmark-dev`.
- Model access: leave unrestricted (all models) or select `gpt-4o-mini`.
- Expiry: optional.
- On create, the plaintext key (`nvk_…`) is shown **once** — copy it then; only a hash is stored.

Talking point: *"Each app/team gets its own virtual key. You can scope it to specific models, set rate limits, expire it, or revoke it instantly — without ever exposing the real provider key. That's the access-control story the thin proxies don't have."*

---

## C. Traffic & log visibility

**Where:** Dashboard → **Traffic**

[screenshot: traffic table]

Each row shows: timestamp, model, prompt/response token counts, latency (TTFT + total), HTTP status, the **virtual key name** that made the call, and the **hook decisions** applied (e.g. `pii-scanner: passed`, or `pii-scanner: rejected`).

**Where:** **Analytics & Metrics**

[screenshot: TTFT / throughput charts]

Talking point: *"Every request is attributed to a virtual key and shows which compliance hooks ran. When a request is blocked, you see exactly why and which policy fired — full audit trail. This is the governance layer."*

---

## D. PII / compliance rejection (the headline)

**Run it live:** `bash JAMES_LIVE_DEMO.sh` (from `benchmark/v2/`). Three commands:

1. **Clean prompt → Nexus → 200.** "What is the capital of France?" passes through, model answers.
2. **PII prompt → Nexus → 403.** A prompt with a fake SSN (`123-45-6789`) and fake card (`4111-1111-1111-1111`) is **blocked in single-digit milliseconds** with header `X-Nexus-Hook: rejected:pii-scanner:pii-detected`. The request never reaches OpenAI — **zero tokens consumed**, decision logged.
3. **Same PII prompt → LiteLLM → 200.** The thin proxy forwards the PII straight to the model. No block, no log, no governance.

[screenshot: terminal showing the 403 with X-Nexus-Hook header]
[screenshot: Traffic table row showing the rejection + reason]

**Audit trail:** the rejection appears in Dashboard → Traffic (and the admin audit log) with the policy name and reason, so compliance can prove enforcement after the fact.

Talking point: *"Same model, same prompt. Nexus catches the PII and refuses before it leaves your perimeter. LiteLLM and Bifrost send it to OpenAI. That difference is the entire value proposition — governance, not just routing."*

---

## Supporting numbers (from local validation; AWS re-run pending)

- **Compliance overhead:** ~960 ms TTFT p50 — that's the price of running PII scanning + keyword blocking on every request (Nexus 1327 ms with hooks on vs 367 ms with hooks off). It's a deliberate trade: governance for latency. See `results/hooks-ab-comparison.md`.
- **Semantic cache:** on repeated prompts Nexus serves from cache, cutting TTFT by ~1838 ms (a Nexus-only feature, measured separately). See `results/` S-08 output.
- **PII enforcement:** the PII demo blocks 100% of fake-PII prompts with 0% false positives on clean prompts. See `demo/pii_demo_evidence.json`.

> These are small-sample local numbers for the demo. The publishable, full-scale comparison runs on AWS (n≥3000). See `STATUS-2026-06-15.md` and `AWS_RUNBOOK.md`.
