# Roadmap Active

This page summarizes what is in-flight during the current development cycle. The canonical source of truth is [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md), which contains per-epic detail: story checklists, architecture decisions, critical gates, reading order, and scope-outs. This wiki page is a digest snapshot; when in doubt, read the canonical file.

The roadmap is a "what to extend / verify / productize" list, not a "what's left to build" list. The 5-service architecture is shipped and serving real traffic in production. Almost every open epic is an extension of an already-shipped system, verification of already-coded surfaces, or quality/productization work.

---

## Recently shipped (2026-05-21)

The following epics merged to `main` via PR #33 (commit `05f147735`) on 2026-05-21:

| Epic | Title | What landed |
|---|---|---|
| **E61** | Smart Response Cache | Two-layer L1/L2 semantic cache with freshness; Valkey 8.x vector search; per-route budget; 12 audit skip reasons; negative-feedback thumbs-down surface |
| **E62** | Cross-adapter embeddings + endpoint typology foundation | `SchemaCodec` widening, `BillableUnits` cost abstraction, capability matrix, three-source consistency invariant, GLM + Voyage AI + Bedrock embedding adapters |
| **E68** | Negative-feedback channel for cache poisoning | Thumbs-down signal from traffic audit drawer + semantic cache eviction |
| **E69** | Pre-warm L2 from FAQ/Q&A corpus | Batch ingest of seed Q&A pairs into the semantic vector cache |
| **E70** | Sticky-token exact-match guard | Exact-match L1 guard preventing model switching mid-session |
| **E71** | Domain-specific semantic thresholds | Per-routing-rule similarity threshold overrides |

Optimization-phase fixes for E61 continue on the `develop` branch.

---

## Planned epics (ready to start)

All entries below have `🟢 Planned` status in the canonical roadmap. There is no implicit ordering — the next epic to start is a pick from this list based on priority.

### Verification of already-coded surfaces

| Epic | Title | Current state | Critical gate |
|---|---|---|---|
| **E72** | AI Gateway adapter verification | 14 spec adapters exist in code but have never run real traffic; no code work yet | `/smoke-gateway --all-ingress` passes per provider added |
| **E73** | Compliance Proxy + Agent Tier-1 adapter verification | ~40 adapters registered across `api/` / `web/` / `ide/` categories; only 9 verified end-to-end so far | `/test-compliance-proxy` per adapter + per-IDE/web synthetic tests |
| **E75** | Three-platform Agent end-to-end verification | Code dev-complete on macOS/Linux/Windows; no platform has a full install → intercept → hook → audit → uninstall synthetic test suite | All three platforms pass on clean-VM images |

### Enhancement of shipped systems

| Epic | Title | Current state | Critical gate |
|---|---|---|---|
| **E74** | macOS pf-intercept (NE replacement) | Planned; SDD pending; closes 5 NE coverage gaps (QUIC blind spot, raw-socket bypass, per-process attribution drift, timeout-driven pass-throughs, per-hop latency) | Content-aware hooks active for at least one gap class; fail-open invariants transferred |
| **E78** | Self-hosted local inference | Design locked; deployment + model choice not started; three downstream consumers (AI Guard, AI Routing, Semantic Embedding) currently use external APIs | All three consumers default to local on fresh deploy; smoke green |
| **E79** | Traffic event storage migration | `traffic_event_*` tables in PostgreSQL today; analytics-scale reads need a columnar store (Clickhouse candidate) | New store ingests with ≤10s lag; dashboards query new store; legacy Postgres tables retired |
| **E81** | High-availability + multi-instance clustering | Single-node baseline today; multi-instance gateway + HA Postgres + Valkey cluster + NATS clustering | Rolling restart of any single node causes 0 request loss |

### Quality and coverage

| Epic | Title | Current state | Critical gate |
|---|---|---|---|
| **E85** | Unit-test coverage 95% | `scripts/.coverage-allowlist` has active entries; pre-commit gate enforces 95% per Go package | Allowlist contains only category A-F structural entries |
| **E86** | End-to-end test coverage uplift | ~75 business flows in `tests/run-all.sh`; formal gap matrix not yet defined | Gap matrix at 0 ✗ for shipped capabilities; CI enforces matrix update on new-feature PRs |

### Productization and operational maturation

| Epic | Title | Current state | Critical gate |
|---|---|---|---|
| **E76** | GitHub Wiki content | Wiki being published now (this page is part of E76) | All wiki sections live; new evaluator reaches first AI request in ≤15 min |
| **E82** | Observability stack completion | Prometheus metrics + alert rules baseline exist; Grafana dashboards, Alertmanager wiring, OTel trace search, log aggregation all missing | On-call person can answer "is the system healthy?" entirely from observability surfaces |
| **E87** | SAML SSO support | IdP type enum stub shipped (`IdPType.saml`); runtime AuthnRequest emitter + signed-assertion verifier not yet implemented | SAML AuthnRequest end-to-end against Okta or Azure AD; JIT provisioning closes the loop |

---

## Reading the table

Each entry in the tables above uses the following conventions from the canonical roadmap:

- **Epic ID** — The permanent identifier. Epic numbers are never reused; retired epics
  keep their original numbers in the backlog index.
- **Current state** — Describes where the work stands today. "Planned — ready to scope"
  means requirements and SDDs have not been drafted yet but the motivation is clear.
  "Planned — SDD pending" means the approach is locked but the per-story breakdown is
  not yet written. "Code dev-complete" means the code exists but the verification suite
  is incomplete.
- **Critical gate** — The mandatory check that must pass before the epic closes. These
  are not optional smoke tests; they are the conditions the epic contract requires.
  An epic that passes its gate is a prerequisite for marking it ✅ Shipped.

The status enum used in the canonical file:

| Status | Meaning |
|---|---|
| `Shipped` | Code merged, smoke green, in production or production-equivalent |
| `In-progress` | Code work underway; some stories may already be merged |
| `Planned` | Requirements/SDDs drafted or in-progress; code work not yet started; eligible to start |
| `Draft` | Requirements/SDDs drafted but not yet approved; decisions pending |
| `Deferred` | Explicitly de-scoped from the current cycle; will be revisited |
| `Cancelled` | Explicitly killed; kept in the index for archaeology |

---

## Contribution entry points

Contributors who want to pick up a Planned epic should:

1. Read the per-epic block in [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) — it lists the reading order, story checklists, and critical gate.
2. Open or claim the first unchecked story in the story checklist.
3. Follow the SDD workflow: Plan → Todo → Architecture → Requirements → SDD → OpenAPI → Code → Tests → Verify.
4. Run the critical gate smoke test before marking a story done.

Verification-class epics (E72, E73, E75) are good first contributions for new contributors because the code already exists — the work is writing the test infrastructure and running it against real upstream services.

---

## What "active focus" means

The canonical roadmap headline reads:

> **Active focus (2026-05-21):** E61 merged to `main` via PR #33 (commit `05f147735`) on 2026-05-21. Ongoing optimization-phase fixes continue on `develop`. The next-to-start epic is up to you to pick from the 🟢 Planned rows.

There is no single designated "next epic" — contributors pick from the Planned rows based on their context. The canonical file's Headline list is updated as focus shifts. This wiki page is a snapshot; the canonical file is the live source.

---

## Canonical docs

- [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) — per-epic detail, story checklists, critical gates, reading order
- [`docs/developers/specs/_backlog.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/specs/_backlog.md) — retired / deferred epic index with reactivation criteria

**Adjacent wiki pages**: [Roadmap Queued](Roadmap-Queued) · [Release History](Release-History) · [Production State](Production-State) · [Contributing](Contributing) · [The Five Services](The-Five-Services)
