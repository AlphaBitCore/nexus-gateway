# Roadmap Queued

This page organizes all planned (not yet started) Nexus Gateway epics into five buckets, mirroring the structure of the canonical roadmap. The canonical source is [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md); this wiki page is a digest. Per-epic detail — story checklists, architecture decisions, critical gates, and reading-order lists — lives only in the canonical file.

The buckets reflect the nature of the work, not a priority order. All entries have `🟢 Planned` status unless noted otherwise.

---

## Enhancement of shipped systems

These epics extend capabilities that are already in production. The architecture is complete; the epic adds new coverage, a new mode, or a significant capability upgrade.

| Epic | Title | One-line scope |
|---|---|---|
| **E74** | macOS pf-intercept replacement of NETransparentProxyProvider | Close five structural NE gaps: QUIC/UDP blind spot, raw-socket bypass, per-process attribution drift, timeout-driven pass-throughs, per-hop latency — via a `pf`-based transparent intercept alongside or replacing NE |
| **E78** | Self-hosted local inference for AI Guard, AI Routing, and Semantic Embedding | One OpenAI-compatible local inference server serves all three downstream consumers; flips external-API defaults to local-first; three consumers currently call external APIs per request |
| **E79** | Traffic event storage migration (PostgreSQL → columnar store) | Move `traffic_event_*` tables from PostgreSQL to a columnar store (Clickhouse candidate) for analytics-scale reads; current PostgreSQL writes scale poorly past production volume |
| **E81** | High-availability + multi-instance clustering | Multi-instance gateway behind load balancer; HA Postgres + Valkey cluster + clustered NATS; Hub leadership election for cron jobs; rolling-restart with zero request loss; documented RTO/RPO SLOs |

---

## Verification of already-coded surfaces

These epics do not add new architecture — they verify that code already in the codebase works end-to-end against real upstream traffic or on real hardware.

| Epic | Title | One-line scope |
|---|---|---|
| **E72** | AI Gateway adapter verification | 14 spec adapter packages exist in code but have never run real production traffic; verify each end-to-end with smoke-gateway, traffic_event cross-check, and adapter-conformance-check |
| **E73** | Compliance Proxy + Agent Tier-1 adapter verification | ~40 adapters registered across `api/` / `web/` / `ide/` categories; only 9 verified end-to-end; remainder need synthetic test suites and real-traffic captures |
| **E75** | Three-platform Agent end-to-end verification | Agent code is dev-complete on macOS/Linux/Windows; no platform has a comprehensive install → intercept → hook → audit → uninstall synthetic test suite yet; all three platforms must pass for the epic to close |

---

## Quality and coverage

These epics address test coverage and quality debt on already-shipped code.

| Epic | Title | One-line scope |
|---|---|---|
| **E85** | Unit-test coverage 95% | Close pre-existing under-95% Go packages sitting in `scripts/.coverage-allowlist`; each entry must reach 95% with tests that assert observable business behavior, not just pad percentages |
| **E86** | End-to-end test coverage uplift | Define a formal gap matrix against the ~75 flows in `tests/run-all.sh`; close gaps for every shipped customer-facing capability; CI enforces matrix update on new-feature PRs |

---

## Productization

These epics make Nexus Gateway more accessible to the OSS community, evaluators, and self-hosting organizations.

| Epic | Title | One-line scope |
|---|---|---|
| **E76** | GitHub Wiki content | Public-facing wiki for OSS evaluators covering: Home, Getting Started, Architecture, AI Gateway, Compliance Proxy, Desktop Agent, Control Plane, Deployment, Operations, FAQ, Contributing, and Security (this very page is part of E76's output) |

---

## Operational maturation

These epics improve the day-2 operational experience on top of the already-shipped monitoring baseline.

| Epic | Title | One-line scope |
|---|---|---|
| **E82** | Observability stack completion | Grafana dashboard library + Alertmanager rule set + OTel trace search backend (Tempo/Jaeger) + log aggregation strategy (Loki/SaaS) on top of the existing Prometheus metrics and alert-rule baseline |
| **E87** | SAML SSO support | Runtime AuthnRequest emitter, signed-assertion verifier, and JIT-provisioning callback handler; the `IdPType.saml` enum stub and `SAMLAdminConfig` / `SAMLClaimConfig` structs are already shipped in code |

---

## Epic dependency map

Some epics have ordering constraints based on shared infrastructure or blast-radius
management. These are not strict sequential gates (all Planned epics can start in
parallel) but reflect what the canonical roadmap notes as recommended sequencing:

```
E81 (HA) → E78 (local inference)   # E81 first to avoid single-host risk concentration
E72 / E73 (adapter verification)   # Can run in parallel; tied adapters share smoke tests
E75 (agent e2e)                    # macOS arm can start now; E74 unlocks macOS content-aware stories
E74 (pf-intercept) ← E62           # E62 shipped; E74 closes the gaps E62 surfaced
```

**E85** and **E86** are quality epics that run in the background of any cycle — they do
not block feature epics but should close before a GA milestone.

**E76** (this wiki) is productization and runs in parallel with all technical epics.

**E87** (SAML) is isolated — no dependency on other open epics; the stub code is already
in place.

---

## How epic priorities are decided

There is no fixed quarterly planning cycle at this stage. Epic priority is:

1. **Customer signal** — a real customer or evaluator hitting a gap that maps to an
   open epic moves that epic up.
2. **Uptime / risk** — E81 (HA) is flagged as an uptime risk; it should land before
   E78 concentrates additional traffic on a single host.
3. **Verification completeness** — E72/E73/E75 are required to close the "production-
   validated" claim for each adapter and platform. They should close before any GA
   announcement.
4. **Contributor interest** — verification epics (E72, E73, E75) are good first epics
   for new contributors; they are intentionally scoped with no new architecture required.

---

## Retired / deferred

The following epic numbers were drafted and then retired. They are kept on record and will not be reused. If any idea is reactivated, it keeps its original number. Full reasons and reactivation criteria are in [`docs/developers/specs/_backlog.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/specs/_backlog.md).

| Epic range | Area | Reason for retirement |
|---|---|---|
| **E63–E67** | Multimodal endpoint family (audio / image / video / async-job / modality-aware hooks) | No active customer demand; endpoint typology framework from E62 stays in code and supports revival |
| **E77** | Official product website | The GitHub Wiki is the single public-facing surface |
| **E80** | SaaS multi-tenant migration | Nexus ships as single-tenant / OSS-deployable |
| **E83** | Client SDKs (Python / Go / TypeScript) | Upstream provider SDKs work transparently against `/v1/*` |
| **E84** | Compliance certifications (SOC2 / ISO27001 / GDPR / HIPAA) | Retired 2026-05-20; certification is an obligation of each deploying organization, not the upstream OSS project |

---

## Canonical docs

- [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) — per-epic detail, story checklists, critical gates, architecture decisions
- [`docs/developers/specs/_backlog.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/specs/_backlog.md) — retired / deferred epic index with reactivation criteria

**Adjacent wiki pages**: [Roadmap Active](Roadmap-Active) · [Release History](Release-History) · [Production State](Production-State) · [Contributing](Contributing) · [The Five Services](The-Five-Services)
