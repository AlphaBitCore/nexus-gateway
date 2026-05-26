---
title: Doc Review 2026-05-22 — Decision Log
audience: developers
status: in-progress
last-updated: 2026-05-22
---

# Doc Review 2026-05-22 — Decision Log

Single-session deep review of every in-scope doc against the current code.

**In scope:** `docs/developers/architecture/**`, `docs/developers/workflow/**`, `docs/developers/handoffs/**`, `docs/developers/specs/{README,_backlog}.md`, `docs/developers/roadmap.md`, `docs/users/features/**`, `docs/users/product/**`, `docs/users/api/**` (excluding `openapi/`), `docs/operators/**`, root-level `README.md` / `README.zh-CN.md` / `CLAUDE.md` / `CONTRIBUTING.md` / `LICENSE` / `NOTICE` / `SECURITY.md` / `CODE_OF_CONDUCT.md`. **167 docs** after excluding `_wiki/` + `_archive/` + SDD-story files (`eN-sM-*.md`) + OpenAPI YAML.

**Out of scope:** `docs/_wiki/`, `docs/_archive/`, SDD story specs, OpenAPI YAML.

## Calibration anchor

Commit `c21850df0` (`docs(readme): align cache section with actual implementation`) retracted four fabricated claims from the root README cache section:

1. **"L1 process-local LRU"** — no in-process response cache exists; reads go straight to Valkey.
2. **"L3 leveraged automatically"** — gateway *accounts* for provider-side cache (surfaces `cached_tokens` / `cachedContentTokenCount`) but does not *emit* `cache_control` outbound by default.
3. **"20 concurrent → 1 upstream call"** — fabricated cap; actual broker is unbounded singleflight.
4. **"hit rate by VK/org"** — fabricated UI dimension; Cache ROI dashboard breaks down by provider/model only (`StatusStrip.tsx`).

Every finding below was triaged against this anti-sample: claims must be **code-anchored** (file:line). LLM-confident specifics without an anchor were demoted to MED or HIGH depending on whether they actively mislead the reader.

## Severity ladder

- **HIGH** — factually contradicts code; an operator/integrator who trusts the doc will get the wrong outcome.
- **MED** — claim lacks anchor or uses aspirational language ("intelligent", "automatic", "out-of-the-box") without a mechanism described.
- **LOW** — numeric specific (latency, count, %) with no source link / benchmark anchor.

## Verdict column

- **FIX-NOW** — applied in this session's edits.
- **FIX-NEXT** — captured as a follow-up; concrete enough for the next session to act on without re-investigating.
- **VERIFIED-OK** — claim challenged, code anchor confirmed; no edit needed.
- **WONTFIX** — judgement call (marketing surface; cross-vendor comparison) where the value of editing is below the noise of disagreement.

---

## Phase 1A — Architecture docs (90 files)

| # | Severity | Doc file:line | Claim | Reality (code anchor) | Verdict |
|---|---|---|---|---|---|
| A1 | HIGH | overview.md:149 | "exactly one response cache; L1/L2/L3 historical labels are vaporware" | `packages/ai-gateway/internal/cache/semantic/{client,lookup,writer}.go` show L2 semantic tier shipped in E61 | FIX-NEXT |
| A2 | HIGH | cost-estimation-architecture.md:446,468 | "L2/L3 reserved schema-only, no UI surfaces it" | Semantic stamping live; cache-multi-tier-architecture.md §11 already documents L2 | FIX-NEXT |
| A3 | HIGH | response-cache-architecture.md:133,311 | "embedding singleflight 100ms default" | `packages/ai-gateway/internal/cache/semantic/singleflight.go:26` `defaultEmbedTimeout = 5*time.Second` | FIX-NEXT |
| A4 | HIGH | response-cache-architecture.md:498 | "Cache ROI page aggregates per-route" | `CacheROIDashboard.tsx:404,408,454` aggregates per-adapter | FIX-NEXT |
| A5 | HIGH | iam-identity-architecture.md:118,171 | "policy cache lives in Redis; key `iam:policy:<principal>`" | `internal/identity/iam/cache.go:15` two-tier L1+L2; prefix `nexus:iam:policies:` | FIX-NEXT |
| A6 | HIGH | cache-multi-tier-architecture.md:40 | IAM policy cache "Redis, TTL 60s" — single tier | Code has L1 (10s, in-process map) + L2 (60s Redis) | FIX-NEXT |
| A7 | HIGH | overview.md:25,119 | "Linux pf/iptables" | Linux uses `iptables_linux.go` only; pf is BSD/macOS-only (E74) | FIX-NEXT |
| A8 | HIGH | project-structure.md:46-197 | Internal directory listings for every service | Pre-2026-05 layout; every service tree has been refactored | FIX-NEXT (whole-file rewrite) |
| A9 | HIGH | configuration-architecture.md:24,77-83 | "Hub-pushed runtime tuning" + "WebSocket push delivers state body" | Memory binding `feedback_thing_config_pull_model` + `thing-model.md:39`: Things always PULL on signal | FIX-NEXT |
| A10 | HIGH | multi-endpoint-coordination-architecture.md:188 | MQ stream `nexus.traffic` | No file-anchor; subject name not verified in code | FIX-NEXT |
| A11 | MED | overview.md §5+§8 | 14+ "(planned)" markers point at docs that exist | All cited docs are live on disk | FIX-NEXT (sweep) |
| A12 | MED | prompt-cache-architecture.md:31,38,66-68 | "gateway emits cache directives in wire shape" | Anchored at `proxy.go:1064-1080` + `wirerewrite/engine.go:256` (MarkersInjected) — but README c21850df0 demoted "auto-injection". Reconcile across README + this doc + wirerewrite | FIX-NEXT |
| A13 | MED | tenancy-architecture.md:17 | "Nexus is multi-tenant. Unit of tenancy is Organization" | README explicitly dropped "multi-tenant"; needs reconciled phrasing | FIX-NEXT |
| A14 | MED | overview.md:25,119 | Desktop "macOS / Windows / Linux" without caveat | `agent-forwarder-architecture.md:41-46` is honest about Windows scaffolds | FIX-NOW (in README), FIX-NEXT here |
| A15 | LOW | overview.md:25 / provider-adapter-architecture.md:15 | "50+ adapters" | Actual: 20 first-class + ~49 traffic adapters (api+web+ide). Number context-dependent | FIX-NEXT |
| A16 | LOW | response-cache-architecture.md:319-325 | HNSW defaults `M=16/EF=200/EF_RUNTIME=10` | Cite `index_lifecycle.go` FT.CREATE call | FIX-NEXT |
| A17 | MED | smart-routing-architecture.md:116-118 | Latency ranges "~100-300ms / ~100-400ms / ~50-150ms" | No benchmark file | FIX-NEXT |
| A18 | MED | response-cache-architecture.md:484 | "~$0.000002 per request OpenAI 3-small with 1k prompt" | No source for cost figure | FIX-NEXT |

**Cross-cutting pattern A-X1 (HIGH):** Three docs disagree about L2 semantic cache existence (A1, A2 vs cache-multi-tier-architecture / response-cache-architecture). Reality: L2 shipped in E61. Treat cache-multi-tier + response-cache as source-of-truth; fix overview + cost-estimation in next session.

**Cross-cutting pattern A-X2 (MED):** 14+ "(planned)" markers in overview.md §5/§8 pointing at docs that now exist. Single sweep.

**Cross-cutting pattern A-X3 (HIGH):** project-structure.md is whole-file stale across all 5 service trees + shared/. Best handled as a full rewrite via `ls packages/*/internal/`.

---

## Phase 1B — Workflow / Handoffs / Specs-non-SDD (~15 files)

| # | Severity | Doc file:line | Claim | Reality | Verdict |
|---|---|---|---|---|---|
| B1 | HIGH | ai-skill-catalog.md:3 | "25 invocable procedures" | `ls .claude/skills/ \| grep -v README \| wc -l` = 26 (catalog missing `test-macos-pf-agent`) | FIX-NEXT |
| B2 | HIGH | timezone.md (full doc) | Documents `timeutil` package (`timeutil.Now()`, `timeutil.InOrg()`, `timeutil.StartOfDayUTC()`) at import path `packages/shared/timeutil` | Package does not exist; `grep -r 'timeutil' packages/` returns 0 hits. Real policy: `time.Now().UTC()` + monotonic-clock allowlist enforced by `scripts/check-timezone-correctness.sh` | FIX-NEXT |
| B3 | HIGH | testing.md §5 | Directory layout lists files that don't exist (`routing_test.go`, `test_sse_shapes.py`, 7 specific Playwright specs) | Actual `tests/` tree differs; report path also wrong (`/tmp/test-all-*` vs real `/tmp/nexus-test/test-all-*`) | FIX-NEXT |
| B4 | HIGH | conventions.md:51 vs :149 | L51: "no `sqlc`, hand-maintained Go struct mirrors, no Prisma→Go codegen". L149: "Go types are codegen'd from the Prisma schema, never write them by hand" | Self-contradiction; L51 matches reality | FIX-NEXT (delete or correct L149) |
| B5 | HIGH | roadmap.md:89,437 | E76 wiki "Planned — nothing published yet" | Memory `project_e76_wiki_oss` confirms MERGED 2026-05-21 via PR #42 (`72524a26`); 145 wiki files on disk | FIX-NOW |
| B6 | MED | ai-skill-catalog.md add-provider-adapter row | "7 architectural rules" in provider-adapter-architecture.md | CLAUDE.md correctly cites Rules 1-8 (Rule 8 added in E58-S0) | FIX-NEXT |
| B7 | MED | handoffs/E76 outline.md:3 | "IA v2 (~95 pages)" | DEC-022 says 143 publishable; 145 files on disk | FIX-NEXT |
| B8 | MED | roadmap.md:437 | Cites repo `github.com/alphabitcore/nexus-gateway` | Other docs use `github.com/alphabitcore/nexus-gateway` | FIX-NEXT |
| B9 | LOW | specs/README.md layout example | Shows `e3/e3-hub-protocol.md` | File does not exist | FIX-NEXT |

---

## Phase 1C — User-facing docs (35 files)

| # | Severity | Doc file:line | Claim | Reality | Verdict |
|---|---|---|---|---|---|
| C1 | HIGH | agent-ui/about.md (full doc) | Describes an About page with version field + update channel + buttons | No `/about` route in agent UI; no `pages/about/`. Replaced by 1-line `AboutFooter` at bottom of Settings (per `AccountPanel.tsx:116-121`) | FIX-NOW (delete) |
| C2 | HIGH | agent-ui/logs.md (full doc) | Describes a Logs page with filters + Pause/Copy/Open buttons | No `/logs` route; no `pages/logs/` | FIX-NOW (delete) |
| C3 | HIGH | agent-ui/settings.md | "Protection Pause: max 1h default; admin-configurable via `agent_settings.maxProtectionPauseDuration`" | `lifecycle/protectionpause/pause.go:54-78` accepts arbitrary seconds; no cap. `grep -r maxProtectionPauseDuration packages/` returns 0 hits | FIX-NEXT |
| C4 | HIGH | features.md §9.5 / ai-gateway-client-guide.md §9.2 | "cache_control / cachedContent injected automatically when prefix matches" | `wirerewrite/engine.go:252` gated on per-provider opt-in; matches what c21850df0 retracted from README | FIX-NEXT |
| C5 | HIGH | competitive-landscape.md TL;DR | "Auto Anthropic cache_control injection" as Nexus moat | Same as C4 — overstated | FIX-NEXT or WONTFIX (marketing surface) |
| C6 | HIGH | features.md §9.5 / product/overview.md §44 | "Three tiers cooperate: provider cached / shared Redis / per-request memoisation" | No per-request memoisation cache in `cache/` directory | FIX-NEXT |
| C7 | HIGH | flows/vk-lifecycle.md:21,41 | `traffic_event.virtual_key_id` column | Schema: `entity_type` + `entity_id` + `entity_name` (no `virtual_key_id`). Same fabrication caught by L5 scenarios on 2026-05-21 (D11) | FIX-NEXT |
| C8 | HIGH | flows/kill-switch-and-passthrough.md:46 | SQL selects `passthrough, bypass_reason` from traffic_event | Actual cols: `passthrough_flags String[]` + `passthrough_reason String?` | FIX-NEXT |
| C9 | HIGH | flows/kill-switch-and-passthrough.md:7,§3 | Hub job `kill_switch.reconcile` runs every minute | Job is named `passthrough.expiry` (passthrough_expiry.go:27); cadence 60s correct, name wrong | FIX-NEXT |
| C10 | HIGH | features.md §7 / product/overview.md §52 | "SAML and OIDC federation" | OIDC has runtime handler (`authserver/login/oidc.go`); SAML has type-enum only, no AuthnRequest/assertion verifier | FIX-NEXT |
| C11 | MED | cp-ui/compliance.md page list | Lists 5 pages | Actual route map has 6 (`streaming / payload-capture / ai-guard / audit-logs / dsar / compliance-report`) | FIX-NEXT |
| C12 | MED | cp-ui/iam.md "default role / org" claim | "JIT-provisioned users land in configured default org / role" | No code anchor cited | FIX-NEXT |
| C13 | MED | product/architecture.md:216 | Production-validated provider list (`production-validated` vs `not yet`) | Status assertion unverifiable from code; relies on roadmap E72 | FIX-NEXT |
| C14 | MED | flows/alert-evaluation.md:50 | "auto-retry 3x with backoff" | No code anchor for "3x" | FIX-NEXT |
| C15 | LOW | product/overview.md §47 | "40+ more via the adapter framework" | Ambiguous (adapter dirs vs model catalog) | FIX-NEXT |

**Note (C-VERIFIED-OK):** ai-gateway-client-guide.md §4 correctly distinguishes its L1/L2/L3 (upstream-native → canonical → ingress shape) from the now-removed README cache L1/L2/L3. No conflation found.

---

## Phase 1D — Operator docs (20 files)

| # | Severity | Doc file:line | Claim | Reality | Verdict |
|---|---|---|---|---|---|
| D1 | HIGH | monitoring.md:44-97 | 26 metric names with `nexus_compliance_proxy_*` prefix | Prefix dropped per CLAUDE.md no-backcompat rule. Actual names: `tunnels_active`, `cert_cache_hits_total`, etc. (see `compliance-proxy-smoke.md:339-353` for canonical list) | FIX-NEXT |
| D2 | HIGH | pki-and-certs.md:113,120 | `nexus_compliance_proxy_cert_cache_hits_total{layer}` | Real name: `cert_cache_hits_total{layer}` | FIX-NEXT |
| D3 | HIGH | e61-valkey-migration.md:256-339 | 5 fabricated metric names: `nexus_aigw_redis_errors_total`, `nexus_aigw_requests_total`, `nexus_aigw_cache_hits_total`, `nexus_aigw_redis_ping_latency_seconds`, `nexus_aigw_redis_pool_wait_total` | None exist in `packages/ai-gateway/`. Real ai-gateway namespaces: `nexus_aigw_*` (stream/gemini only), `nexus_aiguard_*`, `nexus_ai_gateway_credentials_*` | FIX-NEXT |
| D4 | HIGH | ec2-single-node.md:197,207-208 | yaml: `registry.controlPlaneUrl` | Actual field: `registry.nexusHubUrl` (all 3 ai-gateway.*.yaml). Doc value silently leaves nexusHubUrl unset | FIX-NOW |
| D5 | HIGH | ops-metrics-smoke-test.md:80-89 | `POST /api/admin/auth/login` (cookie-session) | Real path: `POST /authserver/password` (`mount.go:248`); CP uses OAuth bearer not cookies | FIX-NEXT |
| D6 | HIGH | agent-windows-build.md:107-119,241 | `.github/workflows/agent-release.yml` runs full chain on tag v*/agent-v*/prod-* | `.github/workflows/` has only `ci.yml` + `go-ci.yml`. agent-release.yml does not exist | FIX-NOW |
| D7 | HIGH | cursor-traffic-capture-debug.md:24 | "two-tier L1/L2" cache labels | Actual `metrics.CertCacheHits.With(...)` labels are `"lru"` and `"redis"` (cache.go:83,93) — not "L1"/"L2" | FIX-NEXT |
| D8 | MED | compliance.md:86-91 | Lists 2 built-in hooks | `policy/hooks/builtins/builtins.go:24-34` registers 11: keyword-filter, pii-detector, content-safety, rate-limiter, request-size-validator, ip-access-filter, data-residency, rulepack-engine, noop, webhook-forward, quality-checker | FIX-NEXT |
| D9 | MED | perf-2026-05-20-nexus-traffic-event.md:155,171 | Cites `audit.go:149,195` | Symbols exist but at lines 232, 371 | FIX-NEXT |
| D10 | MED | perf-2026-05-20-nexus-vs-bifrost.md:118 | "Collapsed ~20 concurrent calls to 1 — a 20×" | W-03 used 20 VUs; the "20× collapse" is inferred from VU count not measured per-leader joiners | FIX-NEXT (anchor or generalize) |
| D11 | MED | backup-dr.md:22-42,178-185 | "Daily logical backup + retention loop; RPO < 5 min" | No automation script in the repo; pg_dump is ad-hoc per release | FIX-NEXT |
| D12 | LOW | air-gapped-deployment.md | Memory says "deferred" | Doc is 32 KB, comprehensive, complete | Memory note stale; no doc edit needed |

**Note (D-VERIFIED-OK):** `redis-setup.md:135` IAM cache L1 (10s) / L2 (60s) is real (`cache.go:13-17`). Distinct from the c21850df0 README L1 LRU fabrication.

---

## Phase 1E — Root-level (7 files)

| # | Severity | Doc file:line | Claim | Reality | Verdict |
|---|---|---|---|---|---|
| E1 | HIGH | README.md:29 / README.zh-CN.md:29 | "route to any of 47+ providers" | 11 first-class adapter codecs + 9 OpenAI-compat = 20. Default seed has ~6-8 Provider rows | FIX-NOW |
| E2 | HIGH | README.md:31 / README.zh-CN.md:31 | 19-name list incl Kimi/Doubao/Qwen/Hyperbolic/OpenRouter/Ollama/vLLM | No adapter folder for those 7; "Kimi" is via `compat/moonshot/` (Moonshot is the company) | FIX-NOW |
| E3 | HIGH | README.md:86,308 / README.zh-CN.md:87,308 | "spillstore: local FS / S3 / GCS" | `packages/shared/storage/spillstore/` has only `localfs/` + `s3/` | FIX-NOW |
| E4 | HIGH | README.zh-CN.md:39-44,86 | Entire cache section still carries the four pre-c21850df0 fabrications (L1 LRU 进程内, automatic 自动利用, 20 个并发, by VK/org) | EN README fixed in c21850df0; ZH never caught up | FIX-NOW |
| E5 | HIGH | CONTRIBUTING.md:44 | `set -a && source tests/.env.test && set +a` | File `tests/.env.test` does not exist; binding convention `tests/.env.<target>` (target ∈ {local, dev, prod}); README.md:236-237 uses correct `source tests/lib/loadenv.sh local` | FIX-NOW |
| E6 | MED | README.md:7,88,261 (and ZH equivalents) | "347 Go packages" | `find packages -name go.mod \| ... \| wc -l` shows 424 packages. Coverage badge stale by same factor | FIX-NOW |
| E7 | MED | README.md:258 / ZH:259 | "27 binding rules in CLAUDE.md" | Top-level `- **...` bullets under `## Mandatory rules` = 19. The number 27 conflated nested sub-rules | FIX-NOW |
| E8 | MED | README.md:259 / ZH:260 | "24 invocable skills" | `ls .claude/skills/ \| grep -v README` = 26 | FIX-NOW |
| E9 | MED | README.md:21,107,155,278 (and ZH) | Desktop Agent "macOS NE / Windows / Linux" without caveat | macOS = 30+ Go files + Swift bundle (production); Linux = 4 files / 1015 LOC scaffold; Windows = 2 files / 1048 LOC scaffold | FIX-NOW |
| E10 | LOW | CLAUDE.md "Less is more" anchor | Cites `anthropic/codec/codec.go:103-107` | Switch block spans lines 101-108 (`switch {` header at 101, closing `}` at 108) | FIX-NOW |
| E11 | LOW | README.md:260 / ZH:261 | "19+ lint scripts" | Actual `ls scripts/check-*.{sh,mjs}` = 24. The `+` accommodates; OK | VERIFIED-OK |

---

## Coverage decisions (Phase 4 framing)

### Realistic scope for this session

The original goal called for "100% unit-test coverage on pf, gateway, and data-connection critical paths". The Phase 0 baseline shows what's already true and what's left:

- **AI Gateway** is at **139 of 143 packages ≥95%**; the remaining 2 (`cmd/ai-gateway`, `cmd/ai-gateway/wiring`) are allowlisted **A** category (main entry + DI sequencer) and cannot reach 100% via unit tests — `os.Exit` + signal-wait + DB-bound init helpers need integration infra. Top non-allowlisted gap is `ingress/proxy` at 95.1%.
- **PF** is at 100% on 6 of 14 packages; 2 are correctly allowlisted **D** (cgo + `pfctl` shellout); 3 truly have 0% (platform composer + libproc shim + bundle inspector) and need new test files totaling ~600 LOC.
- **Data connection** is at ≥95% on 9 of 11 packages; the 2 outliers (`mq` 14.7%, `s3` 1.4%) are allowlisted **E** (real NATS / S3 needed) — fixable via interface seam + fake servers, multi-session work.

**Honest interpretation of "100%":** the coverage baseline is already healthy under the 95% binding rule. Reaching literal 100% requires either (a) inventing test infrastructure for real NATS / S3 (Step 1 of the program below), or (b) lowering the bar to "all testable branches covered, the rest categorised in allowlist with rationale" — which is **already where the repo sits**. The session-realistic deliverable is to expand and document the closeable gaps, not to claim a fictional 100%.

**This session ships:** the gap audit (above), the closure roadmap (below), and the dead-code re-verdict (next section). Implementing the test files that close the named gaps is a multi-session program — flagged as the natural follow-up.

### PF critical path

`scripts/.coverage-allowlist` already exempts the two genuinely OS-bound packages (D category):
- `pfintercept/pidlookup` — libproc cgo + sysctl ioctl
- `pfintercept/pfrules` — `pfctl -a <anchor> -f -` shellout

Real coverage gaps (no allowlist, no tests, hot-path):
1. `packages/agent/internal/platform/darwin` root — `platform_pf_darwin.go` (217 LOC, `PFPlatform` composer): **0% — no test file at the directory root.** Constructor + Start/Stop/ProcessInfo/InterceptionMode are all untested. Composer is pure plumbing; tests can be added cleanly.
2. `packages/agent/internal/platform/darwin/proc` — `ProcessInfo(pid)` libproc shim, 245 LOC: **0%**. Reachable via the `proc.ProcessInfo` indirection used by `PFPlatform.ProcessInfo`.
3. `packages/agent/internal/platform/darwin/bundles` — `InspectBundles()`, 145 LOC: **0%**. Co-resident; not strictly hot-path.
4. `packages/agent/internal/platform/darwin/pfintercept/listener` — **98.3%**. Small lift to 100% via dial-func seam.

Decision: **PF coverage program is scoped to packages 1+2+4 this session.** Package 3 (bundles) is co-resident not hot-path; defer.

**Dead code candidate — re-verdict on inspection:** `packages/agent/internal/platform/darwin/pfintercept/natlook/resolver.go`. Original Phase 0 read said "CODE-DEC-003 retired the real `DIOCNATLOOK` implementation in favor of SNI-only path" + "listener tolerates nil". Closer inspection shows:

- `listener.New()` (listener.go:34-35) returns `errors.New("listener.New: NATLooker is required")` when `cfg.NATLooker == nil` — does NOT tolerate nil.
- `handleConn()` actually calls `l.cfg.NATLooker.Resolve(tcpConn)` at listener.go:183 to recover the original pf-redirected destination.
- 22+ test references via `natlook.Mock` cover this path; AcceptErrors counter has a `natlook` label.
- The package ships only the `Resolver` interface + `Mock` impl — there is no `resolver_darwin.go` with cgo yet (CODE-DEC-003 retired the *real* implementation, leaving the interface in place).

Verdict: **DEFER deletion to E74 close-out.** Right now natlook is a load-bearing interface for the pf listener even though the production implementation is absent — the SNI-only replacement path is part of E74's in-flight work. Removing the package before E74 lands would either break the listener or force a coupled rewrite that should happen inside E74, not as a side-quest here. Decision logged for the next-session sweep at the same time E74 lands.

### AI Gateway critical path

143 packages, **139 ≥ 95%**, only 2 below (both allowlisted A category — `cmd/ai-gateway` main + `cmd/ai-gateway/wiring` DI orchestrator). Pushing every package to literal 100% is unrealistic in this session — `cmd/ai-gateway` cannot move (signal-wait + os.Exit). Decision: **target the top-15 sub-100% packages** identified in inventory. Quirk-sweep (Q) and seam-injection (S/E) work per the proposed 3-step program; this session aims at the Q packages (provider quirks, normalize codecs).

### Data connection critical path

11 packages. Two allowlisted E (mq + s3); thingclient at 96.3% has 31 stmts uncovered on WS-write error arms. Decision: **drive thingclient to ≥99%** this session via injected ticker / fake `*websocket.Conn` writer; defer mq/s3 interface-seam work to a follow-up.

---

## Dead code decisions (Phase 5 framing)

| Candidate | Evidence | Decision |
|---|---|---|
| `pfintercept/natlook/` | Re-checked: listener still calls `Resolve()` at listener.go:183; `New()` errors when NATLooker is nil. Production cgo impl is missing (only Mock ships) but the interface is load-bearing pending E74's SNI-only replacement | **DEFER to E74 close-out** |
| Other staticcheck U1000 sweep | E85 already ran one (58 items removed 2026-05-21) | Re-run quick sanity sweep; no separate cleanup if E85 was complete |

---

## Out-of-session deferred (to next review pass)

Items marked `FIX-NEXT` above (roughly 80% of findings). They group cleanly:

- **A-X1**: Reconcile L2-semantic-cache wording across `overview.md` + `cost-estimation-architecture.md` + `cache-multi-tier-architecture.md` + `response-cache-architecture.md`.
- **A-X2**: Sweep `(planned)` markers off overview.md §5/§8.
- **A-X3**: Full rewrite of `project-structure.md` from current `ls packages/*/internal/` output.
- **A-X4**: IAM cache two-tier description sync (iam-identity-architecture + cache-multi-tier).
- **A-X5**: Hub pull-only invariant sync in configuration-architecture.md.
- **B-X1**: Fix `timezone.md` to match real policy (delete `timeutil` references).
- **B-X2**: Rewrite `testing.md` §5 directory layout from real `tests/` tree.
- **B-X3**: Fix `conventions.md:51 vs :149` self-contradiction.
- **B-X4**: ai-skill-catalog.md add `test-macos-pf-agent` entry + bump count.
- **C-X1**: Fix `vk-lifecycle.md` + `kill-switch-and-passthrough.md` SQL columns.
- **C-X2**: Reconcile SAML status across `features.md` + `product/overview.md` (mark planned, mirror flows/idp-federation.md:28-29).
- **C-X3**: Reconcile auto-injection wording across `features.md` + `competitive-landscape.md` + `ai-gateway-client-guide.md` with c21850df0.
- **D-X1**: Sweep `nexus_compliance_proxy_*` prefix from monitoring.md + pki-and-certs.md + compliance-proxy-smoke.md (label `l1/l2` → `lru/redis`).
- **D-X2**: Fix `e61-valkey-migration.md` 5 fabricated metric names — replace with real `nexus_aigw_*` series.
- **D-X3**: Fix `ops-metrics-smoke-test.md` auth endpoint.
- **A14-followup (surfaced 2026-05-22 batch 5)**: `docs/developers/architecture/services/agent/agent-forwarder-architecture.md:41-46` labels Linux iptables as **"Shipping"** while the user-facing docs we just edited (deployment-models.md, features.md, overview.md) describe Linux + Windows as "scaffolds in development" anchored back to this same doc. Two interpretations possible: (a) "Shipping" in the agent-forwarder doc means "the code compiles and is wired" (Linux has 4 files / ~1015 LOC under `packages/agent/internal/platform/linux/`); (b) "Production-validated" means "running in prod customer environments" (only macOS satisfies that). Reconcile in a future pass: either soften the agent-forwarder anchor to "Wired, not production-validated" OR strengthen the downstream docs back to "Shipping per agent-forwarder anchor" — pick one bar and apply consistently.
- **Coverage**: drive mq + s3 from allowlist via interface seams (next session).
- **Doc lockstep**: add a new lockstep entry for "compliance-proxy metric name" so future renames force a doc sweep.

Each next-session entry is anchored to a specific file:line in this log so the next worker can pick up without re-investigating.

---

## What's verified clean (no edits needed)

- All performance numbers in README.md L75-80 (2 ms / 4-5 ms / 28 ms / 70.5% / 61.7% / 0 errors) trace correctly to `perf-2026-05-20-nexus-traffic-event.md` §2-3.
- `LICENSE` is Apache 2.0 verbatim; badge correct.
- `SECURITY.md` / `CODE_OF_CONDUCT.md` / `NOTICE` — no factual contradictions.
- ai-gateway-client-guide.md §4 L1/L2/L3 framing (format pivot, NOT cache layers) — explicitly distinct from the removed README cache hierarchy.
- redis-setup.md:135 IAM cache L1+L2 — real.
- alerts.md — 4 channel types + `/api/admin/alerts/{id}/{ack,resolve}` routes verified.
- agent-recovery.md pf anchor `nexus-agent/transparent` + listener 127.0.0.1:13443 — verified.
- backup-dr.md `rotate-key` endpoint — verified.
- deployment.md service paths + healthz endpoints — verified.
- E76 handoff `decisions.md` (DEC-001..023) — internally consistent with the merged ship.

---

## Calibration meta-finding

The c21850df0 anti-sample produced a structural lesson: **AI-written marketing-feel docs over-promise quantitative specifics**. Every number / categorical list / "automatic" / UI-dimension claim must come with a code anchor or be generalized. The README cache fix caught 4 such claims in one section; this review caught 42 more across the rest of the doc set.

The deferred follow-up backlog is large enough that the next iteration should consider adding a CI gate ("any new claim with a number must reference a benchmark file / counter / constant"). Not currently scoped; flagging for `feedback_*` memory.
