# E74 — Decisions log (macOS pf-intercept replacement of NETransparentProxyProvider)

> Live decision log produced during E74 implementation. Captures (a) the locked decisions inherited from the planning session and the Requirements doc, (b) brainstormed sub-decisions taken during SDD / Code phases, and (c) deferred items routed to user approval. Every entry carries date, decision ID, alternatives considered, the rule chosen, the rationale, and the source (Opus self-brainstorm, sub-agent recommendation, user-approval).

> Format: append-only. If a decision is reversed, append a new entry referencing the prior ID — do not edit history. Code/doc lockstep applies: implementation MAY NOT diverge from a recorded decision without a new entry overriding it.

---

## Locked decisions inherited from Requirements (verbatim)

These were settled before E74 SDD began. SDD MAY refine sub-decisions inside each; SDD MAY NOT reverse the top-level direction.

| ID | Decision | Source |
|---|---|---|
| D1 | **Architectural direction: pf path on top of existing `tlsbump`**. pf anchor + `rdr` rules → daemon listener → `BumpFlow`. **Sub-decision deferred to SDD**: full replacement of NE vs hybrid (NE for default app traffic + pf only for the five gap-class flows). | Planning session 2026-05-21, locked in `e74-macos-pf-intercept.md §1.4` |
| D2 | **Zero new Apple submissions / certifications**. Existing Apple Developer Program / Developer ID certs / notarization continue unchanged. NE entitlement kept inert (not revoked). `com.apple.developer.networking.networkextension` removed from pf-only build target. "Allow System Software" gate removed. | Planning session 2026-05-21 |
| D3 | **Per-process attribution: `libproc` / `proc_pidpath`, NOT Endpoint Security**. ES entitlement is harder than NE — refused. Residual helper-process attribution gap documented as Out of Scope. | Planning session 2026-05-21 |
| D4 | **Install-flow friction reduction is a Requirements-level NFR**, not a side effect. Surface in product review + release notes (NFR-7). | Planning session 2026-05-21 |

---

## SDD-phase decisions (brainstormed, dated)

Entries below are produced during the SDD drafting Phase (Phase 1-2 of the implementation programme). Each entry resolves one open question and binds the SDD story it touches.

### DEC-001 — D1 sub-decision: pf is the steady-state default; NE retained as escape-hatch only

- **Date**: 2026-05-21
- **Question**: D1 sub-decision deferred to SDD — full replacement of NE vs hybrid (NE for default app traffic + pf only for gap-class flows). Documented as deferred in `e74-macos-pf-intercept.md §1.4 D1`.
- **Alternatives considered**:
  - **A — Full replacement**: pf is the only steady-state mode. NE Swift code stays in tree (build-target-gated, not compiled into pf-only `.pkg`) for escape-hatch use only via `interceptMode="ne"`. The "hybrid" mode is retired.
  - **B — Long-lived hybrid**: NE handles default app traffic, pf handles the five gap-class flows. Both stacks coexist in steady state.
  - **C — pf-default + NE-fallback**: pf is default; daemon auto-falls-back to NE when pf install fails. Both stacks always shipped in the binary.
- **Decision**: **A — Full replacement with NE as bounded escape-hatch**. `interceptMode` enum collapses to `pf | ne`; `hybrid` is removed from FR-5.1's three-value enum. NE Swift code remains in git (build-target-gated) for the duration of the cutover window; deletion of NE Swift code is **out of scope for E74's main PR** and is reconsidered in Phase 8 cleanup only after pf is gated stable in prod.
- **Rationale**:
  1. Maintaining two intercept stacks doubles the surface area where E62/E73/future-typology code needs review per change. Linux already runs single-stack (iptables REDIRECT + daemon listener); single-stack maintenance is the proven baseline.
  2. Hybrid's "two intercept paths to one MITM" creates a class of decision-routing bugs (which path serves this flow?) absent from the single-path model. CLAUDE.md "less is more" applies: a hybrid dial admins must understand is worse than a single dial they don't.
  3. The original Requirements §1.4 D1 listed hybrid as a "prototype mode" for telemetry gathering. With pf empirically working on macOS 26 (verified at brainstorm time: `pfctl` available, `/dev/pf` accessible by root) and the libproc attribution stack already production-grade (`darwin/proc/processmeta_darwin.go` shipping today), the prototype-data argument is weak — first-principles call says "Linux-equivalent single-stack is correct".
  4. `interceptMode="ne"` retained as escape-hatch is sufficient for the corner case "pf doesn't work on this specific Mac" — operators flip back to NE without touching binary distribution.
- **Affects**:
  - **FR-5.1** (Requirements): enum collapses from `ne | pf | hybrid` to `ne | pf` (default `pf` after FR-7 acceptance).
  - **FR-5.2** (Requirements): the hybrid-mode-as-prototype clause is retired; the decision-routing single-source-of-truth requirement is preserved (decisions live in `domain.Engine`, not split across NE Swift + pf code).
  - **Phase 4 code subtasks**: 4 active subtasks (pf rules / listener / libproc / interceptMode + fail-open). Hybrid-decision-routing subtask removed.
  - **Phase 8 cleanup**: NE Swift code listed as a deferred-cleanup candidate, not a Phase-8 immediate deletion. Re-evaluation gated on prod telemetry.
- **Source**: Opus self-brainstorm (first-principles); written 2026-05-21.

### DEC-002 — Q1: pf rule scope — redirect ALL outbound TCP 443; daemon-side `domain.Engine` decides

- **Date**: 2026-05-21
- **Question** (E74 §8 Q1): redirect all 443 vs domain-IP-resolved per-IP rules.
- **Alternatives considered**:
  - **A — Redirect all TCP 443**: pf catches every outbound TCP 443; the daemon's loopback listener consults `domain.Engine` on every accepted connection to choose inspect / passthrough / deny.
  - **B — Per-IP rdr rules**: agent's existing domain → IP resolver pre-resolves intercept-listed domains and writes per-IP `rdr` rules; pf only redirects flows whose dst IP matches the rule.
- **Decision**: **A — Redirect all TCP 443**.
- **Rationale**:
  1. Symmetry with Linux: `packages/agent/internal/platform/linux/linux_linux.go:240-275` does exactly this — `getOriginalDst` then `p.handler.HandleConnection(intercepted)` with `domain.Engine` doing the decision. Replicating the same model on macOS minimises code divergence and lets the listener share idioms (timeouts, error envelopes, audit shape).
  2. Per-IP rdr rules require continuous IP-cache refresh as DNS rotates. The Linux `Reconciler` already does this in a different layer (iptables) — moving the same logic to pf duplicates it without benefit.
  3. The hot path "listener consults `domain.Engine`" is in-memory non-blocking sub-millisecond. No latency motivation to push the decision into pf.
  4. CLAUDE.md "less is more": one mechanism (listener-side decision), not two (rule-cache + listener fallback).
- **Affects**: **FR-1.3, FR-1.7** (Requirements): pf rule shape stays at "redirect all 443"; domain-scoped decision is in `domain.Engine` not in pf.
- **Source**: Opus self-brainstorm; reinforced by Linux reference implementation.

### DEC-003 — Q2 + Q5: SO_ORIGINAL_DST equivalent — DIOCNATLOOK ioctl on `/dev/pf`

- **Date**: 2026-05-21
- **Question** (E74 §8 Q2 + Q5 — same problem, dual-numbered): what's the macOS equivalent of Linux `SO_ORIGINAL_DST`?
- **Alternatives considered**:
  - **A — `ioctl(/dev/pf, DIOCNATLOOK, &pfioc_natlook)`**: the canonical pf-style original-destination lookup. Accepted in `<net/pfvar.h>`. Used by FreeBSD's `pfctl` and downstream tools.
  - **B — Private socket option**: macOS has no public `SO_ORIGINAL_DST` socket option; private kernel APIs would require entitlement we can't obtain (per D2).
- **Decision**: **A — DIOCNATLOOK**. Implementation via cgo (`#include <net/pfvar.h>`) inside a thin seam package `pfintercept/natlook/` (≤80 lines including `cgo` boilerplate). The seam exposes a Go interface `OriginalDstResolver.Resolve(localPort, remoteAddr, remotePort) (dstIP, dstPort, error)` so tests can mock the ioctl boundary.
- **Rationale**:
  1. `golang.org/x/sys/unix` has no macOS pf bindings; cgo is the only path.
  2. The kernel call sequence is well-documented: open `/dev/pf` (`O_RDWR`, requires root), populate `pfioc_natlook` struct with `(saddr, daddr, sport, dport, direction=PF_OUT)`, ioctl `DIOCNATLOOK`, read back `rdaddr` / `rdport` = original destination. Same pattern used by squid + transocks + ttyd on FreeBSD-derived systems.
  3. macOS 14 + 15 + 26 all ship `/dev/pf` and the `DIOCNATLOOK` ioctl number is stable. The risk Q5 hedges (different syscall on different macOS) is theoretical — Apple has not changed pfvar.h's ioctl numbers since the pf port.
  4. Test seam at `OriginalDstResolver` interface means listener tests don't need root or `/dev/pf` access; 100% coverage achievable via mock.
- **Affects**: **FR-2.1** (Requirements). New package `packages/agent/internal/platform/darwin/pfintercept/natlook/` (cgo, build-tag `darwin` only).
- **Source**: Opus self-brainstorm; cross-referenced against existing Linux `getOriginalDst` (`linux_linux.go:570-606`) for interface shape.

### DEC-004 — Q3: bundle → uid resolution timing — Eager (at agent_settings push time)

- **Date**: 2026-05-21
- **Question** (E74 §8 Q3): resolve bundle list → uid set eagerly (at config-push time) vs lazily (at first-flow time).
- **Alternatives considered**:
  - **A — Eager**: on `agent_settings` push and on daemon start, look up each bundle's installed uid set via `LSCopyApplicationURLsForBundleIdentifier` + `stat(2)` on the executable path → uid. Write the resolved uid set into pf rules immediately.
  - **B — Lazy**: at first redirected flow from a yet-unresolved bundle, resolve uid then re-load pf rules.
- **Decision**: **A — Eager**.
- **Rationale**:
  1. Uid → bundle mapping is stable across a login session; the inputs (`/Applications/*.app` set, current user's uid) change only on app install/uninstall or user switch — both rare events that already trigger LaunchServices reindexing.
  2. Lazy resolution makes the **first** flow from a fresh-install app slip through unredirected (UDP/443 → QUIC succeeds because no pf rule yet). The "first time you try to use ChatGPT after installing Chrome" UX is the worst possible test case for an inspection product — admin sees no audit row and assumes the rule isn't working.
  3. Startup cost of LaunchServices lookup is well-bounded (≤100 ms for the typical 8-bundle list). Sub-second startup overhead is acceptable.
  4. Eager resolution also enables a unit-testable seam: `BundleResolver.ResolveUIDs([]bundleID) []uid` is a pure function over a mocked LaunchServices interface.
- **Affects**: **FR-1.4, FR-8.2** (Requirements). New seam `BundleResolver` (interface + LSCopy-backed impl + mock).
- **Source**: Opus self-brainstorm; reinforced by the "fresh-install Chrome → ChatGPT" UX scenario from `project_cursor_capture_investigation`.

### DEC-005 — Q4: listener port — fixed `13443`, yaml-overridable, NOT admin-UI-overridable

- **Date**: 2026-05-21
- **Question** (E74 §8 Q4): listener loopback port — fixed vs dynamic.
- **Alternatives considered**:
  - **A — Fixed `13443`**: hardcoded constant, easy to debug, occasional risk of collision with admin-managed local services.
  - **B — Dynamic allocation**: kernel-assigned port at start, recorded in `agent_settings` reported state. Zero collision but more reported-state surface.
  - **C — Fixed default + yaml/env override**: `13443` default, ops-level dial in agent yaml/env for the rare collision case; **NOT** exposed in admin UI.
- **Decision**: **C — Fixed default + yaml/env override, no admin UI surface**.
- **Rationale**:
  1. CLAUDE.md "less is more": no admin UI dial for what is effectively an ops-level escape valve.
  2. Linux uses fixed `127.0.0.1:19080` (`linux_linux.go:38`) without incident — collisions are rare enough that the fixed-default model is empirically correct.
  3. Yaml/env override preserves operator escape hatch for the rare collision without adding admin UI cognitive load.
  4. Dynamic allocation requires reporting-state extension (added field on the Thing's reported config), which is more code + more surface than the value adds.
- **Affects**: **FR-2.1** (Requirements). yaml field `pfIntercept.listenerAddr` in `packages/agent/internal/sync/schema/config.go` defaulting to `127.0.0.1:13443`. Loaded via `bootenv` per CLAUDE.md secrets binding (note: port is non-secret, lives in yaml not env).
- **Source**: Opus self-brainstorm; reinforced by Linux convention.

### DEC-006 — Q6: pf rule hot-reload — atomic anchor re-load via `pfctl -f`

- **Date**: 2026-05-21
- **Question** (E74 §8 Q6): on `agent_settings` shadow update, re-load pf rules atomically vs incremental edits.
- **Alternatives considered**:
  - **A — Atomic re-load**: write a new rules file, then `pfctl -a nexus-agent/transparent -f <new-rules>`. pf replaces the anchor's entire contents atomically.
  - **B — Incremental edit**: compute diff between old / new rules, apply only the delta.
- **Decision**: **A — Atomic re-load**.
- **Rationale**:
  1. pf documents anchor `-f` as atomic at the kernel level. The replace happens between two kernel ticks; established sockets are unaffected.
  2. Incremental edits require diff logic + pf rule parser. Both are sources of bugs without latency benefit.
  3. Linux's iptables reconciler also does atomic-style table rewrites (`iptables-restore --noflush`). Same idiom across platforms.
- **Affects**: **FR-1.2** (Requirements). pf rule installation path uses `pfctl -a ... -f -` (stdin) to avoid temp-file race; pf semantics treat this as atomic anchor replacement.
- **Source**: Opus self-brainstorm; reinforced by Linux reconciler shape.

### DEC-007 — interceptMode default value during cutover

- **Date**: 2026-05-21
- **Question** (not in §8 directly, but surfaced from DEC-001): with hybrid mode retired, when does the default flip from `ne` → `pf`?
- **Alternatives considered**:
  - **A — Big-bang flip in E74 main PR**: `default = "pf"` from the moment E74 merges.
  - **B — Two-step flip**: E74 ships with `default = "ne"` (no behaviour change for the install base); admins opt-in to `"pf"`. After 30 days of prod telemetry confirms gap-class closure (FR-7), a follow-up PR flips `default = "pf"`.
  - **C — Build-stamped default**: pf-only build target stamps `default = "pf"`; NE-included build target stamps `default = "ne"`. Operators choose at install time which build to deploy.
- **Decision**: **C — Build-stamped default**. The pf-only `.pkg` (built via `build-agent --target=pf-only`) compiles with `default = "pf"`. The current legacy `.pkg` continues to ship `default = "ne"` until explicitly retired via D1's cleanup gate.
- **Rationale**:
  1. C cleanly separates "what's installed" from "what's configured". An ops shop that opts into the new pf-only pkg gets pf by default; one that doesn't gets exactly today's behaviour. No surprise flips on existing install base.
  2. A is risky on a live install base (would silently change behaviour on first upgrade).
  3. B works but creates a 30-day window of "default may change under you" which is harder to reason about than C.
  4. Per CLAUDE.md "API / menu / route changes require IAM impact review" — IAM stays clean because the dial is build-stamped, not a new admin route.
- **Affects**: **FR-5.1, FR-6.1** (Requirements). `build-agent` skill's `--target=pf-only` argument sets a Go ldflag `-X 'main.defaultInterceptMode=pf'`; legacy target sets `-X '... =ne'`.
- **Source**: Opus self-brainstorm; surfaced from DEC-001 retirement of hybrid.

### DEC-008 — Test seam architecture for 100% coverage on cgo / syscall code

- **Date**: 2026-05-21
- **Question** (not in §8, surfaced from user binding "100% coverage on pf / gateway / data-connection paths"): how does an interface-poor cgo file achieve 100% statement coverage?
- **Alternatives considered**:
  - **A — Wrap every cgo call in a top-level interface**: `OriginalDstResolver`, `BundleResolver`, `PFRuleInstaller` as `interface { … }`. Production impls call cgo; tests use struct mocks. cgo glue files are minimal (≤30 lines each) and `[no statements]` per coverage tool semantics (cgo `import "C"` does not produce countable Go statements).
  - **B — Build tag**: cgo files behind `//go:build darwin && !test_skip_cgo`; mock files behind opposite tag. Coverage reports per-build separately.
  - **C — Accept <100% on cgo files; document in `.coverage-allowlist` with category D (OS-bound)**: this is the existing escape hatch for OS-bound code per CLAUDE.md unit-test binding.
- **Decision**: **A + bounded C**.
  - All non-cgo logic (rule rendering, decision routing, error mapping, recovery sequences, listener flow) lives behind A's interface seams → ≥100% coverage via mocks.
  - The cgo glue itself (the ≤30-line wrapper that calls `ioctl(DIOCNATLOOK)` or `LSCopy*`) is allowlisted under category D ("OS-bound") with a one-line rationale per `.coverage-allowlist` convention. The allowlist entry covers ≤2 files, ≤60 LOC total.
- **Rationale**:
  1. 100% coverage on real cgo / kernel-ioctl code is infeasible without root + `/dev/pf` in test env; CI runners can't satisfy.
  2. Interface seams give us coverage on the **logic** (which is where bugs live) without paying the kernel-access cost.
  3. The allowlisted glue is mechanically small — a reviewer can read 30 lines and judge correctness manually. CLAUDE.md's coverage policy already accommodates this via category D.
  4. This pattern is reusable for E63 / E64 (future audio / image OS-bound code).
- **Affects**: **FR-2.7, FR-3.5** (Requirements), `.coverage-allowlist` entries (added in same PR as code), NFR-12.
- **Source**: Opus self-brainstorm; reinforced by `feedback_unit_test_coverage_95` memory + existing allowlist precedent.

### DEC-009 — Daemon panic auto-removal mechanism

- **Date**: 2026-05-21
- **Question** (not in §8, but FR-4.8 binding): on daemon crash mid-flow, how does pf anchor get cleared within 30 s?
- **Alternatives considered**:
  - **A — Sibling LaunchDaemon `nexus-agent-pfclean`** that watches the main daemon and `pfctl -F` on its death.
  - **B — LaunchDaemon `KeepAlive=true` + on-restart cleanup**: main daemon's startup script first runs `pfctl -a nexus-agent/transparent -F all` before re-installing rules. If the main daemon is dead, no rules are installed; next launchd-restart triggers cleanup-then-reinstall.
  - **C — defer/signal handler in daemon Go code** runs `pfctl -F` on SIGTERM / panic recovery.
- **Decision**: **B + C in concert**.
  - C covers graceful and panic-recoverable exits (defer at daemon main's top level + signal handlers for SIGTERM / SIGINT). This is the **fast path** (sub-second cleanup).
  - B covers `kill -9`-style un-recoverable death where C has no chance to run. launchd's restart loop (`KeepAlive=true` + `ThrottleInterval=5`) brings the daemon back within ≤5 s; its first action is `pfctl -F` on the anchor. Worst-case anchor-stuck window is the throttle interval (5 s).
  - A is **rejected** — a sibling LaunchDaemon doubles the install / signing / notarization surface for a guarantee we already get from B + C.
- **Rationale**:
  1. B + C together meet the FR-4.8 30 s budget with a typical actual budget of ≤5 s.
  2. Single LaunchDaemon (the main `com.nexus.agent.plist`) is simpler than two; aligns with CLAUDE.md "delete instead of add".
  3. Linux already does C-equivalent (cleanup on signal in `linux_linux.go` `Stop()`); macOS gets the same shape.
- **Affects**: **FR-4.6, FR-4.8** (Requirements). `cmd/agent/main.go` signal handler updates; `dist/macos/com.nexus.agent.plist` `KeepAlive` + `ThrottleInterval` settings.
- **Source**: Opus self-brainstorm.

### DEC-010 — Reuse of existing `darwin/proc/processmeta_darwin.go` for libproc attribution

- **Date**: 2026-05-21
- **Question** (not in §8, surfaced from code reading): the existing `packages/agent/internal/platform/darwin/proc/processmeta_darwin.go` already implements `proc_pidpath` + `proc_pidinfo` + bundle resolution + helper-process display-name fallback. Reuse vs reimplement.
- **Alternatives considered**:
  - **A — Reuse existing `proc.ProcessInfo(pid)`** as the attribution layer for the pf listener. Add a thin adapter that maps `proc.Meta` → `proxy.FlowProcess` (the format `BumpFlow` consumes).
  - **B — Reimplement inside the new `pfintercept/` package**.
- **Decision**: **A — Reuse**.
- **Rationale**:
  1. The existing code is production-grade — it handles the "Electron Helper directory looks like a version string" edge case via `LooksLikeVersionString` + `BundleDisplayNameFromPath` (`processmeta_darwin.go:54-58`). Reimplementing risks regressing that fix.
  2. CLAUDE.md "delete instead of add" — extending the existing package is strictly better than creating a parallel one.
  3. The PID-from-socket part is what the pf listener actually needs to add — `findPIDBySocket` equivalent for macOS. macOS approach: read /dev/pf NATLOOK to recover original dst (DEC-003), recover source uid from the redirected socket's `getsockopt(SO_PEERCRED)` equivalent, then `proc_listpids(PROC_UID_ONLY, uid)` + match socket port via per-pid `proc_pidinfo(PROC_PIDFDVNODEPATHINFO)`. New code goes here; not in `processmeta_darwin.go`.
- **Affects**: **FR-3.1, FR-3.2** (Requirements). New file `packages/agent/internal/platform/darwin/pfintercept/pidfromsocket_darwin.go` (cgo); reuses existing `proc.ProcessInfo(pid)`.
- **Source**: Opus self-brainstorm; reinforced by code-reading `darwin/proc/processmeta_darwin.go:34-83`.

### DEC-012 — `domain.Engine` MUST stay shared between Compliance Proxy and Agent (user-binding)

- **Date**: 2026-05-21
- **Question** (user-raised mid-session, 2026-05-21): the pf path's daemon-side decision (DEC-002) uses `domain.Engine` to choose inspect / passthrough / deny. User binding: this engine must remain shared between Compliance Proxy and Agent — the pf path is not allowed to fork it or grow an agent-private variant.
- **Alternatives considered**:
  - **A — Keep `packages/shared/policy/domain` as the single source of truth**: pf listener imports it; cp imports it; tlsbump imports it; any new evaluation logic lands in `shared/policy/domain` and is available to all three. (Current state, verified by grep below.)
  - **B — Fork an agent-private `domain.Engine` variant** to allow macOS-specific quirks (e.g. helper-process attribution rules that cp doesn't have).
  - **C — Define a thin agent-side wrapper around `shared/policy/domain`** that adds macOS-specific pre-filters, leaving the shared engine pure.
- **Decision**: **A — `shared/policy/domain` stays the sole evaluation engine**. The pf listener calls `shared/policy/domain.Engine.Evaluate` directly. Any macOS-specific decision logic that surfaces during implementation (e.g. helper-process attribution affecting policy) is **first attempted in shared code** (so cp benefits too); only when there is a hard reason the logic doesn't generalise (cp doesn't have NEAppProxyFlow concept) does it move to an agent-side wrapper per Option C.
- **Rationale**:
  1. User explicit binding 2026-05-21: "domain.Engine 如果是纯逻辑的来判断这个domain/path 是要process、passthough的话 我希望是compliance proxy和 agent公用的".
  2. Verified current state — already shared:
     - `packages/shared/policy/domain/{types,engine}.go` is the canonical implementation.
     - Importers: `packages/compliance-proxy/**` (8 files: forward, listener, server, wiring × 4, cacheloaders, interception_domains_full), `packages/agent/**` (4 files: bridge, pipeline, dto_to_domainpolicy + tests), `packages/shared/transport/tlsbump/**` (3 files: bump, forward_handler, sse).
     - Total 15 production importers across 3 services + shared. Forking would invalidate the consistency guarantee that the same `interception_domain` row produces the same `inspect | passthrough | deny` decision regardless of which service evaluates it.
  3. CLAUDE.md "code/doc lockstep" + endpoint-typology-architecture §8.7 already requires three-source consistency (AI Gateway / CP / Agent produce byte-identical NormalizedPayload). Forking the decision engine would directly violate that invariant.
  4. The E62 multimodal framework explicitly builds on top of shared `domain.Engine` + shared `policy/hooks/` + shared `transport/normalize/` — fork would force E63 / E64 / E65 / E66 to multi-port.
- **Affects**:
  - **DEC-002 (Q1)**: reinforces — pf listener calls `shared/policy/domain.Engine.Evaluate` exactly as cp does, not a copy.
  - **Story S2 (listener)**: interface contract explicitly says "DomainEngine *domain.Engine" pointer is injected by wiring; the listener owns no domain-evaluation code.
  - **Story S5 (wiring)**: wiring constructs ONE `domain.Engine` instance per daemon and shares it across all consumers (bridge, listener, tlsbump). No per-listener engine.
  - **Phase 8 cleanup**: any agent-private path-decision helpers discovered are candidates for migration into `shared/policy/domain` (or deletion if duplicates of the shared logic).
  - **Phase X+1 review gate**: explicit check — `grep -RIl 'package domain' packages/agent` returns ZERO new files (only re-exports / imports are allowed).
- **Verification command** (run during Phase X+1 second-round review):
  ```
  # 1. Only the shared package defines `domain.Engine`:
  grep -RIlE 'type\s+Engine\s+struct' packages/ --include='*.go' | grep -v '_test.go'
  # Expected: exactly one file — packages/shared/policy/domain/engine.go
  # 2. Agent does not host its own evaluation logic:
  grep -RInE 'func.*Evaluate.*pfintercept' packages/agent/ --include='*.go'
  # Expected: zero matches
  ```
- **Source**: User binding 2026-05-21 (mid-session message); reinforced by current-state grep + endpoint-typology consistency rules.

---

### DEC-013 — CP and Agent share by default; differ only at the intercept boundary (user-binding)

- **Date**: 2026-05-21
- **Question** (user-raised mid-session, 2026-05-21, follow-up to DEC-012): does the "CP and Agent share `domain.Engine`" rule generalise to every layer that has overlapping semantics?
- **User wording** (verbatim, 2026-05-21): "compliance proxy和 agent 应该有很多东西是公用的 他们的差异化主要是一个是在服务端执行 一个是在客户端电脑上执行。compliance proxy的流量接入是直接监听端口就可以，agent在各个平台需要通过类似pf等技术拦截os流量。 所以他们是有很多的复用性的 也只有这样 才能达到我们说 三端合规一致"
- **Decision**: **Yes — full reuse is the binding default**. The CP and Agent are architecturally **the same forwarding proxy** with a different **intercept boundary**:
  - **CP**: listens on a TCP port; clients route to it explicitly.
  - **Agent**: needs OS-level capture (pf on macOS / iptables on Linux / WinDivert on Windows); flows are then handed to the same Go-side pipeline as CP.
- **Reuse boundary (what MUST be shared)**:
  - **Decision engine**: `shared/policy/domain.Engine` (already shared — DEC-012).
  - **Hook pipeline**: `shared/policy/hooks/`, `shared/policy/pipeline/`.
  - **MITM core**: `shared/transport/tlsbump/`.
  - **Normalize / canonicalisation**: `shared/transport/normalize/{core,codecs,extract}`, `shared/canonicalbridge`, `shared/canonicalext`.
  - **Traffic adapters**: `shared/traffic/` (per-provider request/response normalisation).
  - **Audit event format + emission**: `shared/audit/` types + `pipeline.AuditEmitter`.
  - **Cost stamping / streaming policy / payload capture / spillstore wire format**: all under `shared/`.
- **Intercept boundary (what MAY differ)** — exactly and only the layer above:
  - **CP**: `packages/compliance-proxy/internal/proxy/server/` — TCP listener accepting client connections.
  - **Agent — Linux**: `packages/agent/internal/platform/linux/` — iptables NAT REDIRECT + `getsockopt(SO_ORIGINAL_DST)` + listener.
  - **Agent — macOS (E74)**: `packages/agent/internal/platform/darwin/pfintercept/` — pf anchor + `rdr` + `DIOCNATLOOK` ioctl + listener.
  - **Agent — Windows**: `packages/agent/internal/platform/windows/` — WinDivert kernel capture + listener.
  - Each of these intercept layers terminates by handing a `(net.Conn, peekedClientHello, dstHost, dstPort, flowID, proc, BridgeDeps)` tuple to `shared/transport/tlsbump.BumpConnection` (the existing contract via `agent/internal/network/proxy.BumpFlow`). After that handoff, **everything is shared**.
- **Rationale**:
  1. User binding 2026-05-21: "只有这样 才能达到我们说 三端合规一致" — three-source consistency (AI Gateway / CP / Agent producing byte-identical `NormalizedPayload`, identical hook decisions, identical audit emission) is the load-bearing product guarantee. Forking any of the shared layers breaks the guarantee silently.
  2. Endpoint-typology §8.7 already documents the consistency invariant (E62 work). DEC-013 binds the **structural rule** that produces it.
  3. CLAUDE.md "delete instead of add": every time someone considers "should this go in agent/ or shared/?" — the default answer is shared, and the burden of proof is on the agent-private option.
  4. Forking has compounding cost — every future endpoint typology (E63 audio / E64 image / E66 video) would have to multi-port; reuse means E63-E66 each ship once across all three Things.
- **Affects** (broad — applies to every story under E74 and every future agent or CP epic):
  - **Story S1 (pf rules)**: lives in `packages/agent/internal/platform/darwin/pfintercept/pfrules/` because pf is darwin-specific. NOT shared.
  - **Story S2 (listener)**: the listener glue is darwin-specific (constructs the FlowProcess, calls DIOCNATLOOK), but immediately hands off to `shared/transport/tlsbump`. The Go listener file lives in `packages/agent/internal/platform/darwin/pfintercept/listener/`; the decision dispatch logic INSIDE the listener calls `shared/policy/domain.Engine` + `shared/policy/pipeline` directly.
  - **Story S3 (libproc)**: darwin-specific. Reuses `packages/agent/internal/platform/darwin/proc/` (already darwin-specific, per DEC-010).
  - **Story S5 (wiring)**: wiring composes the same `BridgeDeps` struct that NE wiring constructs today; only the intercept layer differs.
  - **Story S6 (build)**: the build target choice (`pf-only` vs `legacy`) only affects the intercept layer; shared code is in every build.
  - **Story S7 (gap-closure tests)**: explicitly verifies that the same `interception_domain` rule consulted via agent's pf path and via the CP path yields identical inspect|passthrough|deny decisions (this is the cross-service-consistency arm).
  - **Phase 8 cleanup**: any helper logic discovered in `packages/agent/` that has a CP equivalent in `packages/compliance-proxy/` is a candidate for promotion to `packages/shared/`. Conversely, anything in `packages/shared/` that turns out to be agent-only is a candidate for demotion (rare; check carefully).
  - **Phase X+1 review gate**: explicit check — every file added under `packages/agent/internal/platform/darwin/pfintercept/` must answer the question "could this live in shared/?" with a recorded reason. The reason is either "platform-specific intercept layer" (acceptable) or "logic I forgot to put in shared/" (the file moves to shared/ in this PR).
- **Verification commands** (run during Phase X+1 second-round review):
  ```
  # 1. New shared helpers introduced by E74 are reachable from CP wiring:
  git diff <base>..HEAD -- packages/shared/ | grep '^+++' | grep -v '_test.go'
  # For each new shared file, grep both packages/agent and packages/compliance-proxy for an import path
  
  # 2. No agent-private duplicates of shared logic:
  # diff -r packages/agent/internal/<X> packages/shared/<X>  (only when overlap suspected)
  ```
- **Source**: User binding 2026-05-21 (mid-session follow-up to DEC-012); reinforced by endpoint-typology §8.7 + the existing import-graph evidence cited under DEC-012.

---

### DEC-011 — Naming: `pfintercept` package + sub-packages structure

- **Date**: 2026-05-21
- **Question** (Code-phase prep): how to lay out the new code so it's discoverable + decomposable?
- **Alternatives considered**:
  - **A — Flat package** `pfintercept` with all files at top level (`rules.go`, `listener.go`, `pidlookup.go`, `natlook.go`).
  - **B — Subpackage decomposition**: `pfintercept/` (top-level interface + wiring), `pfintercept/pfrules` (anchor + rule template), `pfintercept/listener` (loopback listener + decision dispatch), `pfintercept/natlook` (DIOCNATLOOK cgo seam), `pfintercept/pidlookup` (PID-from-socket + reuse of `darwin/proc`).
- **Decision**: **B — Subpackage decomposition** with the four sub-packages above.
- **Rationale**:
  1. CLAUDE.md "directory size decomp binding": ≥10 files in a non-Bucket-A dir triggers decomposition planning. Even though `pfintercept` will start at ~6 files, the natural seams are clear and decomposing now avoids a refactor later.
  2. Each sub-package owns exactly one concept and one interface seam → unit tests can target one seam at a time → 100% coverage per sub-package is tractable.
  3. The cgo files (`natlook/natlook_darwin.go` + `pidlookup/pidlookup_darwin.go`) are isolated in their own sub-package, so the `.coverage-allowlist` entries (per DEC-008) are scoped narrowly.
- **Affects**: Phase 3 package structure; all of Phase 4 code; **Memory anchor** `project_e74_macos_pf_intercept` to record the layout.
- **Source**: Opus self-brainstorm.

---

## Code-phase decisions

Entries below are produced during code implementation when a sub-decision surfaces that the SDD did not pre-bind. These are typically about API shape, interface seams for testability, mock surfaces, error envelope wording, log-field naming.

### CODE-OQ-001 — PROC_ALL_PIDS single-pass vs uid-scoped two-pass for socket→PID lookup (defer to Phase 4)

- **Surfaced by**: S3 sub-agent during SDD drafting, 2026-05-21
- **Story**: S3 (libproc PID attribution)
- **Question**: `proc_listpids` can be called with `PROC_ALL_PIDS` (one syscall returns every PID) or `PROC_UID_ONLY` (one call per uid; multiple uids = multiple calls but smaller per-call result set). Which is faster on a typical Mac with 200-500 processes?
- **Constraint**: This decision needs measured data on a real machine. Defer to Phase 4 implementation; prototype both and benchmark.
- **Default if no measurement performed**: `PROC_ALL_PIDS` single-pass — fewer syscalls, simpler code, latency at the 200-500 PID scale is negligible (≤1 ms per call empirically).

### CODE-OQ-002 — IPv4-mapped IPv6 normalisation in socket tuple match (defer to Phase 4)

- **Surfaced by**: S3 sub-agent during SDD drafting, 2026-05-21
- **Story**: S3 (libproc PID attribution) + indirectly S2 (listener)
- **Question**: Does pf's `rdr` rule normalise IPv4-mapped IPv6 addresses (e.g. `::ffff:127.0.0.1`) to plain IPv4 when the socket tuple is later inspected via `proc_pidsocketinfo`? If yes, listener matching is single-stack; if no, dual-stack matching is required.
- **Constraint**: Empirical — run pf with a known IPv6 source + IPv4 dst, examine the `proc_pidsocketinfo` output. Defer to Phase 4.
- **Default if not addressed**: implement dual-stack matching defensively. Cheap, harmless overhead.

### CODE-OQ-003 — build-prod.sh: single script with `TARGET` branch vs new wrapper

- **Surfaced by**: S6 sub-agent (T6.1.4)
- **Story**: S6 (build-agent skill)
- **Question**: extend existing `build-prod.sh` with a `TARGET` env-branch, or ship a sibling `build-prod-pf.sh`?
- **Default**: single script + `TARGET=` branch (less surface).
- **Decide at**: Phase 4 when reading `build-prod.sh`.

### CODE-OQ-004 — Host-app .entitlements drop on pf-only

- **Surfaced by**: S6 (T6.2.4)
- **Question**: must the host app's `NexusAgent.entitlements` also drop `com.apple.developer.networking.networkextension` + `com.apple.developer.system-extension.install` on pf-only, or only the extension's?
- **Default**: drop only the extension's; verify host app still codesigns cleanly on macOS 26.
- **Decide at**: Phase 4 with a codesign smoke.

### CODE-OQ-005 — Dormant NE bundle in pf-only `.pkg`

- **Surfaced by**: S6 (T6.4.4)
- **Question**: include the NE bundle as a dormant payload (quick flip-back) or omit (smaller pkg)?
- **Default**: **omit**. Per DEC-001 the escape-hatch is `interceptMode="ne"` which works on the **legacy** pkg; flipping to NE requires a re-install anyway — bundling the dormant payload is the worst of both worlds.
- **Decide at**: Phase 4 (record final decision in Code-phase log).

### CODE-OQ-006 — `SKIP_NOTARIZE=1` honoured by build-prod.sh

- **Surfaced by**: S6 (T6.7.3)
- **Question**: is `SKIP_NOTARIZE=1` already a recognised env var or does it need to be plumbed?
- **Decide at**: Phase 4 by reading the script.

### CODE-OQ-007 — RESOLVED in S2 — wire-signal naming for UDP-blocked flows

- **Surfaced by**: S7 sub-agent (T7.3 Gap 2)
- **Story**: depends on S2 listener interface
- **Resolution (this brainstorm)**: S2 listener does NOT emit a `traffic_event` row for pf-blocked UDP/443 flows. Reason: UDP is blocked AT THE PF LAYER before any listener accepts a connection — there's no flow to audit. Instead, S2 increments a Prometheus counter `nexus_agent_pf_udp_blocked_total{bundle=…}` so the cross-service consistency arm can verify the count delta. S7 amends its Gap 2 assertion accordingly.
- **Decided at**: S2 drafting (this PR).

### CODE-OQ-008 — RESOLVED in S2 — Prometheus metric names for pf listener

- **Surfaced by**: S7 (T7.8/T7.9)
- **Resolution (this brainstorm)**: Listener registers exactly four metrics:
  - `nexus_agent_pf_flows_accepted_total{decision="inspect|passthrough|deny"}` — incremented at listener.handleConn dispatch.
  - `nexus_agent_pf_udp_blocked_total{bundle="…"}` — incremented at the pf-rule shape level (via a sidecar `pflog` consumer, OR — simpler — derived as the difference between pf rule-hit counters from `pfctl -a … -sr -v -v`; defer the implementation choice to Code phase, but the metric name is locked).
  - `nexus_agent_pf_listener_accept_errors_total{reason="…"}` — for diagnostic.
  - `nexus_agent_pf_natlook_errors_total{stage="open|ioctl|parse"}` — for cgo seam diagnostics.
- **Decided at**: S2 drafting (this PR).

### CODE-OQ-009 — RESOLVED in S5 — reported-state field for interceptMode

- **Surfaced by**: S8 sub-agent (T8.6)
- **Resolution (this brainstorm)**: agent emits its applied interceptMode to Hub reported state at path `agent_settings.interceptMode` (matching the desired-state path symmetrically, per `thing-config-pull-model` symmetry binding). S8 UI reads the SAME path; the "currently applied" indicator compares desired vs reported.
- **Decided at**: S5 drafting (this PR).

### CODE-OQ-010 — settings.Handler audit dependency injection

- **Surfaced by**: S8 (T8 audit emitter)
- **Question**: is `audit` already in `settings.Handler.Deps`?
- **Decide at**: Phase 4 by reading `packages/control-plane/internal/api/handlers/settings.go`. If not, add to Deps with mock in tests.

### CODE-DEC-001 — Listener does host-level decision only; per-path DENY lives inside tlsbump

- **Date**: 2026-05-21
- **Surfaced by**: Phase 3 skeleton build — discovered `domain.Engine` real API is `MatchHost(host) → *InterceptionDomain` + `PathAction(domain, path) → PathAction`, NOT a single `Evaluate(host, path) → decision` call as the SDD draft assumed.
- **Resolution**: listener layer (S2) does HOST-LEVEL decision only — `MatchHost(host)` returns `nil` → passthrough; non-nil → inspect → dispatch to BumpFlow. PATH-LEVEL decision (PROCESS / PASSTHROUGH / DENY per path) is invoked **inside tlsbump.BumpConnection** after HTTP parse — that code path already exists and is shared with Compliance Proxy per DEC-013.
- **Why this is correct architecture**:
  1. At TCP-accept time, the listener only has the TLS SNI / IP — there is no HTTP path yet. Path-level decisions cannot happen at this layer.
  2. tlsbump already runs `domain.Engine.PathAction(domain, path)` per HTTP request inside the bumped TLS stream (`shared/transport/tlsbump/forward_handler.go`, `shared/transport/tlsbump/bump.go` — confirmed by grep on the importer list under DEC-012). CP follows the exact same flow.
  3. DENY at host level would block legitimate sub-paths on a host that mostly should pass — that's why pathAction exists.
- **Affects**:
  - **S2 SDD (e74-s2-loopback-listener-bumpflow.md)**: `handleConn` pseudocode rewritten; Metrics `FlowsAccepted` label set collapsed from `{inspect|passthrough|deny}` to `{inspect|passthrough}`; AC2.2/AC2.3/AC2.4/AC2.5 reworded; AC2.5 removed (listener emits no DENY).
  - **`listener/types.go` skeleton**: `FlowsAccepted` doc comment updated.
  - **S7 gap-closure tests**: Gap-3 / cross-service consistency arm assertions stay correct (they assert the same `interception_domain` rule produces the same end-to-end outcome on agent vs CP — that's HTTP-request-level, which is where the decision actually lands).
- **Source**: Opus self-audit during Phase 3 skeleton build (real API vs SDD draft alignment); recorded immediately to prevent Phase 4 implementers from coding to the wrong API.

---

### CODE-DEC-003 — Eliminate NATLOOK entirely; SNI is the sole source of dst host (supersedes parts of DEC-003)

- **Date**: 2026-05-21
- **Surfaced by**: Phase 4 implementation — `net/pfvar.h` is NOT in the macOS userland SDK (kernel-only since macOS 10.x). cgo `#include <net/pfvar.h>` fails to compile. Reconstructing the `pfioc_natlook` struct + DIOCNATLOOK ioctl number from Apple OSS source is possible but brittle across macOS versions.
- **Brainstorm**:
  - **A — Reconstruct pfioc_natlook struct in cgo**: fragile across macOS versions; Apple may change layout silently.
  - **B — Use `pfctl -s state -v` shell-out + parse**: slow + brittle text parsing per call.
  - **C — Eliminate NATLOOK; use SNI as the sole dst host signal**: pf rules redirect TCP/443 only; TCP/443 in 2024+ is TLS-mandatory; TLS 1.2+ mandates SNI. The listener can fully serve all real flows from SNI alone.
- **Decision**: **C — SNI-only routing**. The listener uses SNI peek (500 ms timeout per FR-2.2) as the authoritative dst host. For pass-through flows, the listener dials `<sni-host>:443` via DNS — same path tlsbump uses internally. Non-TLS flows on TCP/443 (rare; would be a malformed client or unusual protocol) gracefully close as failed-handshake.
- **Architectural win — alignment with DEC-013 reuse**: CP also derives dst host from the wire (CONNECT method), not from the kernel. With CODE-DEC-003, the agent's pf listener and CP's TCP listener share the same "host from wire" pattern. The structural symmetry between CP + Agent (per user's DEC-013 binding) is now even tighter — both consult `domain.Engine.MatchHost(host)` with a host derived from the wire, not from any kernel NAT table.
- **Affects**:
  - **Story S2 (listener)**: `handleConn` no longer calls NATLOOK. SNI peek is the first action after accept. Listener `Config.NATLooker` field becomes optional (nil OK); the listener sub-agent's current work using NATLooker still compiles — it just delegates to SNI when NATLooker is nil. Phase 5 refactor passes nil.
  - **Story S3 (libproc)**: unchanged — still needed for FlowProcess attribution via PID from socket tuple.
  - **`pfintercept/natlook` package**: keep Resolver interface + Mock for future use (e.g. if Apple ever ships pfvar.h in userland, we plug in `RealResolver`); REMOVE the cgo `resolver_darwin.go` body — it's not in the production path.
  - **PFPlatform constructor**: drop the `natlook.NewRealResolver()` call; pass nil NATLooker to listener.
  - **CODE-OQ-002 (IPv4-mapped IPv6 in socket tuple)**: also moot — no NAT-table lookup happens, no socket-tuple mismatch.
  - **DEC-003 + Requirements §FR-2.1**: SUPERSEDED in part. The "DIOCNATLOOK ioctl on /dev/pf" clause is replaced by "SNI from the ClientHello via 500 ms peek". The Requirements doc remains valid history; this DECISIONS entry is the controlling decision.
- **Edge case acknowledgement**: clients that send non-TLS on TCP/443 will fail handshake at the listener's SNI peek step (timeout → empty SNI → close gracefully). This is rare in 2024+; acceptable per CLAUDE.md "less is more / delete instead of add".
- **Source**: Opus self-brainstorm (Phase 4 build failure → pivot to simpler architecture); reinforced by CP listener pattern (TCP-listen + extract host from wire).

---

### CODE-DEC-002 — DIOCNATLOOK cgo body shipped complete; Phase 9 verifies on a real Mac with sudo (SUPERSEDED by CODE-DEC-003)

- **Date**: 2026-05-21
- **Surfaced by**: Opus self-deliberation during Phase 4 wiring — `pfintercept/natlook/resolver_darwin.go` needs a real cgo body for `ioctl(/dev/pf, DIOCNATLOOK, …)`. Per user gate "no defer / no followup", writing a TODO stub is unacceptable.
- **Brainstorm**:
  - **A — Ship cgo stub returning error; mark "Phase 9 to verify"**: violates user gate ("no followup").
  - **B — Ship complete cgo body per pfvar.h documented contract; verify in Phase 9 with sudo on a real Mac**: structural correctness from documentation + runtime verification later. Standard pattern for OS-bound cgo code; existing `darwin/proc/processmeta_darwin.go` is the precedent (production code shipped + verified via dogfood).
  - **C — Don't ship pf RealResolver; daemon-Start fallback to NE on macOS**: defeats the whole epic.
- **Decision**: **B**. Write the complete cgo body now using the documented `pfvar.h` struct layout + the public DIOCNATLOOK ioctl number. Phase 9 verify executes the path on a real Mac under sudo + active pf rules; if Phase 9 surfaces a struct-layout drift (macOS 26 changed pfvar.h vs documentation), the fix is in this file and is a Phase 9 task, not deferred follow-up.
- **Affects**: `pfintercept/natlook/resolver_darwin.go`. Stays allowlisted per DEC-008 (cgo + OS-bound). No new follow-up tasks; the file ships complete.
- **Source**: Opus self-brainstorm; reinforced by `darwin/proc/processmeta_darwin.go` shipping precedent.

---

### CODE-OQ-011 — Install-flow doc filename + macOS arch doc §3a insertion point

- **Surfaced by**: S9 sub-agent
- **Question**: exact filename for "install and enrol" feature doc under `docs/users/features/agent-ui/` (no obvious match in the current directory listing); exact heading-hierarchy position for the new §3a in `agent-macos-platform-architecture.md`.
- **Default**: if no install-and-enrol doc exists, create `docs/users/features/agent-ui/install-and-enrol.md` stub; insert §3a immediately after the existing §3 "macOS-specific path conventions" by inspecting the current doc at Phase 6 edit time.
- **Decide at**: Phase 6 by reading the docs in question.

---

## Cleanup-phase decisions

Entries below record dead-code / stale-doc / unused-asset deletions, with strict pre-delete verification evidence (grep proofs, last-touched dates, blast-radius checks). Per user binding: nothing is deleted without two-pass verification.

<!-- Append entries from here. Template:

### DEC-CLEANUP-NNN — Delete <path>

- **Date**: YYYY-MM-DD
- **Path(s)**: <files / dirs>
- **Type**: dead code / obsolete doc / unused asset / superseded module
- **Pre-delete verification**:
  - `git log --all -- <path>` last-touched: <date>
  - `grep -RIn '<symbol>' --include='*.go' --include='*.swift' --include='*.md'` returns: <count + sample matches>
  - Blast radius assessment: <imports / cross-refs / runtime callers>
  - Sub-agent two-pass review: <agent ID, conclusion>
- **Decision**: DELETE / KEEP / DEFER
- **Rationale**: <text>
- **Source**: Opus + sub-agent two-pass

-->

---

## Deferred to user approval

Entries below are decisions where Opus's first-principle reasoning surfaced enough risk that user sign-off is the right gate. Each entry records the question, the recommendation, and the user's recorded answer.

<!-- Append entries from here. -->
