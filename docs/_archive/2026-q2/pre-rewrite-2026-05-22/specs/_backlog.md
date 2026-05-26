# Backlog — Retired & Deferred Epics

> Single index of epic numbers that were drafted (or reserved) on the roadmap and then retired or deferred without implementation. Kept here as archaeology so the numbers are NOT reused (per `roadmap.md` Maintenance checklist) and so future readers can find why a number is missing from the active roadmap.
>
> An entry leaves this file ONLY when explicit customer demand reactivates it. At that point the requirements + SDDs get drafted under `docs/developers/specs/eN-*.md` as usual, and the corresponding row reappears in `roadmap.md` with a fresh `Planned` status.

## Format

Each entry: epic number, title, retired-on date, one-line reason, reactivation criteria. No story-level detail — that lives in the original SDD if/when revived.

## Retired (will not be reused as-is)

### E63 — Audio: TTS + STT

- **Retired**: 2026-05-20
- **Reason**: No customer signal. Nexus's core positioning is enterprise AI compliance gateway; voice synthesis / transcription is not a buyer ask for that segment.
- **Reactivation criteria**: A paying or qualified-pipeline customer asks for audio traffic compliance / routing / cost-accounting.
- **Notes**: E62's `SchemaCodec` widening + `BillableUnits` cost registry already accommodate this typology if revived — no architectural rework needed.

### E64 — Image generation (sync) + artifact relay v1

- **Retired**: 2026-05-20
- **Reason**: Same as E63 — no enterprise compliance buyer signal for image-gen as a primary use case.
- **Reactivation criteria**: Customer signal for image-gen traffic governance, OR Class-A image NSFW hook becomes a buyer ask before image-gen support itself.
- **Notes**: Artifact-relay framework was sketched in E62 §8 architecture decisions but not built; revival needs that block reopened.

### E65 — Async job orchestrator (Job table + NATS fan-out + webhook deliverer)

- **Retired**: 2026-05-20
- **Reason**: Was reserved as prerequisite for E66 (video). With E66 itself retired, E65 has no dependent epic remaining and no standalone buyer signal.
- **Reactivation criteria**: E66 reactivates, OR a customer asks for async-job style endpoints (Replicate Predictions wire, OpenAI Batches) as a primary surface.
- **Notes**: E62 declared `JobRef` / `JobStatus` / `ArtifactKind=job` to forward-compat this; full `AsyncAdapter` interface signature was deliberately deferred (E62 FR-7.5).

### E66 — Video generation (Veo / Sora / Runway + nexus.ext.*)

- **Retired**: 2026-05-20
- **Reason**: No enterprise compliance buyer signal. Video-gen is a creator-tools market, not a compliance market.
- **Reactivation criteria**: Same as E63/E64 — explicit customer ask.
- **Notes**: Video canonical was sketched as minimum subset (prompt + duration + aspect_ratio + seed) NOT Veo-locked; revival can pick up that framing.

### E67 — Modality-aware hooks expansion (image NSFW, voice clone, video frame scan)

- **Retired**: 2026-05-20
- **Reason**: Depends on E64 (image) or E66 (video) shipping first; with those retired, has no scaffolding to attach to.
- **Reactivation criteria**: E64 or E66 reactivates.
- **Notes**: E62's hook framework (Class A modality-applicability gating, `Hook.SupportsModality()`) already supports this if revived; only the concrete hooks would need to be written.

### E80 — SaaS multi-tenant migration

- **Retired**: 2026-05-20 (surfaced 2026-05-19)
- **Reason**: Nexus ships as a single-tenant on-prem / OSS-deployable product; the SaaS direction is dropped.
- **What stays in code anyway**: The `org_id` columns that exist today (Project / NexusUser / traffic_event / etc.) serve **internal multi-org structure inside a single tenant** — not SaaS cross-tenant isolation — and remain in place.
- **What was reverted**: Forward-compat hacks added in service of E80 (notably the E61 `semantic_cache_config.org_id` widening) — see commit `c902f4e07`.
- **Reactivation criteria**: Only re-evaluate if Nexus ever becomes a hosted commercial offering with Nexus Inc. as the data processor.

### E77 — Official product website

- **Retired**: 2026-05-20
- **Reason**: E76 (GitHub Wiki) is the chosen documentation + onboarding surface; one location per concept. A separate marketing/website would duplicate Wiki content and double the maintenance burden.
- **Reactivation criteria**: A go-to-market motion explicitly requires a non-GitHub branded surface (e.g. partner co-marketing, enterprise procurement that won't browse GitHub). Until then, Wiki is the public face.

### E83 — Client SDKs (Python + Go + TypeScript)

- **Retired**: 2026-05-20
- **Reason**: Customers already use upstream OpenAI / Anthropic SDKs transparently against Nexus's `/v1/*` endpoints — this works because of the canonical-bus architecture. Publishing dedicated SDKs gives marginal value (telemetry shortcuts, `nexus.ext.*` field helpers) at significant ongoing-maintenance cost across three package registries.
- **Reactivation criteria**: A customer explicitly asks for a Nexus-branded SDK with capabilities the upstream SDK can't deliver (e.g. multi-tenant VK rotation, programmatic routing-rule edits from application code).

### E84 — Compliance certifications + audit-ready posture (SOC2 / ISO27001 / GDPR / HIPAA)

- **Retired**: 2026-05-20
- **Reason**: Nexus Gateway is open source. Certification is an enterprise-of-record obligation owned by each deploying organisation, not by the upstream project. Compliance work belongs in the deployer's audit programme, not in this repo's engineering roadmap.
- **Reactivation criteria**: Only if Nexus ever becomes a hosted commercial offering with Nexus as the data processor — which is currently out of scope (E80 SaaS direction also retired 2026-05-20).
- **Notes**: Repo-level support for any deployer pursuing certification (SBOM generation, structured audit logging, change-log evidence) stays in scope as ordinary engineering work — just not as a tracked epic.

---

## Maintenance

- Epic numbers in this file are **NOT reused**. If a retired idea is revived, it gets a fresh epic number from the next-free slot.
- When you retire an epic from `roadmap.md`, add it here with the retirement date, reason, and reactivation criteria — do NOT delete the row from `roadmap.md` history; just move the active block here.
- The next free epic number is tracked in `roadmap.md` Maintenance checklist, not here.
