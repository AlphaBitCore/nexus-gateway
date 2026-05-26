# E74-S9 — Documentation Lockstep for macOS pf-Intercept

> Story: e74-s9
> Epic: 74
> Status: Planning
> Date: 2026-05-21
> Requirements: `docs/developers/specs/e74-macos-pf-intercept.md` §FR-9.1–FR-9.6
> Source decisions: DEC-001, DEC-007, DEC-012, DEC-013
> Architecture: `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`; `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md`; `docs/developers/architecture/README.md`
> Memory: `project_e74_macos_pf_intercept` (to be created — T9.6)
> Blocked by: S1 (pf rules), S2 (listener), S3 (libproc), S4 (fail-open invariants), S5 (wiring), S6 (build target), S7 (gap-closure tests), S8 (admin-UI interceptMode dial) — all must exist before this story's cross-references are authoritatively correct.
> Blocks: E74 PR merge — CLAUDE.md code/doc lockstep binding requires this story's deliverables to land in the same PR as the S1–S8 code.

---

## 1. User Story

As a **Nexus Gateway maintainer or operator picking up an E74-related page**, I want every architecture doc, feature doc, and operator runbook affected by the pf-intercept introduction to be updated in the same PR as the code — so that the docs describe the system as it actually runs (not as it ran before E74), the `check-doc-lockstep` gate passes on every future PR touching the pf path, and a new contributor can read one coherent document without being surprised by NE-only prose sitting next to pf code.

---

## 2. Tasks

### T9.1 — Update `agent-macos-platform-architecture.md`: add §3a "Interception path (pf)" and Recovery commands

File: `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`

- Insert a new subsection **§3a — Interception path (pf) (E74)** immediately after the existing §3 "macOS-specific path conventions" heading. Content to add:
  - Anchor name and parent slot: `nexus-agent/transparent` under the system `anchor` table.
  - pf rule lifecycle: installed at daemon start via `pfctl -a nexus-agent/transparent -f -` (stdin, atomic anchor replacement per DEC-006); removed at daemon stop via `pfctl -a nexus-agent/transparent -F all`.
  - Redirect scope: all outbound TCP 443 (and optionally 80 when admin policy enables plain-HTTP capture), excluding loopback (`127.0.0.0/8`, `::1`) and the daemon's own uid (`! user nexus-agent`) per FR-1.5–FR-1.6.
  - QUIC/UDP redirect: only for uids on the `quicFallbackUIDs` set (resolved from `agent_settings.forceQUICFallbackBundles` at install/refresh time per FR-1.4); all other UDP is passed through untouched.
  - Original destination recovery: `DIOCNATLOOK` ioctl on `/dev/pf` (`net/pfvar.h`) keyed by `(saddr, sport, daddr, dport, PF_OUT)`; cgo seam in `packages/agent/internal/platform/darwin/pfintercept/natlook/` (DEC-003).
  - Hot-path call chain: kernel `rdr` → daemon loopback listener (`127.0.0.1:13443` default) → SNI peek (500 ms deadline) → `domain.Engine.Evaluate` → `proxy.BumpFlow` or `opaqueRelay` (FR-2.3–FR-2.4). Call chain diagram should mirror the Linux equivalent in `agent-linux-platform-architecture.md` for readability.
  - Process attribution: `proc_pidpath` + `proc_pidinfo` via reused `packages/agent/internal/platform/darwin/proc/processmeta_darwin.go`; helper-process attribution gap documented and quantified (NFR-9 threshold ≥90% on 24 h developer-machine test).
  - Per-flow decision table: `inspect` → `BumpFlow`; `passthrough` → `opaqueRelay`; `deny` → close with policy reason. Mirrors FR-1.7 and DEC-002.
- Add a **Recovery commands** subsection (sibling of §3a) listing:
  - `sudo pfctl -a nexus-agent/transparent -s rules` — inspect current anchor contents.
  - `sudo pfctl -a nexus-agent/transparent -F all` — flush anchor (fail-open recovery; also run by `build-agent` uninstall).
  - `sudo launchctl unload /Library/LaunchDaemons/com.nexus.agent.plist` — stop daemon and trigger cleanup-on-restart sequence (DEC-009 path B).

### T9.2 — Update `agent-macos-platform-architecture.md`: add "Relationship to Compliance Proxy" section (DEC-013)

File: `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`

This task is required by DEC-013 (user-binding 2026-05-21) and DEC-012. Add a top-level section titled **"Relationship to Compliance Proxy"** after the new §3a content.

Content to add:

- Opening paragraph: the macOS Agent and the Compliance Proxy are architecturally the same forwarding proxy. The only structural difference is the intercept boundary — CP listens on a TCP port; the Agent uses pf (macOS), iptables NAT REDIRECT (Linux), or WinDivert (Windows) to capture OS-level traffic and hand it to the same Go-side pipeline.
- Shared layers table: list the following shared packages and their roles:

  | Layer | Shared package | Purpose |
  |---|---|---|
  | Decision engine | `packages/shared/policy/domain` | `inspect \| passthrough \| deny` for every flow |
  | Hook pipeline | `packages/shared/policy/hooks/`, `packages/shared/policy/pipeline/` | PII / keyword / safety / redact hooks |
  | MITM core | `packages/shared/transport/tlsbump/` | TLS termination, cert minting, uTLS upstream dial |
  | Normalize / canonicalisation | `packages/shared/transport/normalize/`, `packages/shared/canonicalbridge/`, `packages/shared/canonicalext/` | Provider-agnostic payload extraction |
  | Traffic adapters | `packages/shared/traffic/` | Per-provider request / response normalisation |
  | Audit event format | `packages/shared/audit/` | Audit type definitions and emission helpers |

- Intercept boundary paragraph: state explicitly that only `packages/agent/internal/platform/darwin/pfintercept/` (macOS), `packages/agent/internal/platform/linux/` (Linux), `packages/agent/internal/platform/windows/` (Windows), and `packages/compliance-proxy/internal/proxy/server/` (CP) differ — and that each terminates by handing a `(net.Conn, peekedClientHello, dstHost, dstPort, flowID, proc, BridgeDeps)` tuple to `shared/transport/tlsbump.BumpConnection`.
- "Three-source consistency" cross-reference: link to `endpoint-typology-architecture.md §8.7` and state that DEC-013 is the structural rule that enforces it.
- Note that `domain.Engine` MUST NOT be forked into an agent-private variant (DEC-012); any new decision logic lands in `shared/policy/domain` first.

### T9.3 — Update `agent-ne-fail-open-architecture.md`: generalise framing and add pf-path equivalents under each Rule

File: `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md`

Two parts:

**Part A — Frontmatter and title generalisation (FR-9.2)**

- Change the frontmatter `title:` field from any NE-exclusive wording to: `Agent macOS Intercept Fail-Open Architecture`.
- Update the opening paragraph to note that this document covers both the NE path (legacy) and the pf path (E74), and that both paths enforce the same five fail-open invariants — the mechanism differs, the guarantee is identical.

**Part B — pf-path equivalent subsection under each Rule (FR-9.2)**

For each of the five existing Rules (Rule 1 through Rule 5), insert a **"pf path equivalent"** sub-block immediately after the rule's NE prose. Required content per rule:

- **Rule 1 — Synchronous intercept decision**: pf equivalent is the daemon listener's in-process `domain.Engine.Evaluate` call, which is synchronous and in-memory. No IPC round-trip; no async callback; no `requestDecision` 2 s timeout class. The listener never lets a redirected flow hang indefinitely — `net.Conn.SetDeadline` is set at accept time; a failed `BumpFlow` returns a policy error and closes the socket. FR-4.1 citation.
- **Rule 2 — Fail-open timeout**: pf path replaces the `requestDecision` 2 s timeout with a 500 ms SNI peek deadline (FR-4.2). If SNI peek times out, the listener falls through to `opaqueRelay` (fail-open: traffic passes, content not inspected). Dial timeout for upstream is bounded separately. No flow can hang waiting on an absent daemon — if the daemon is absent, pf rules are absent (DEC-009 cleanup), so kernel `rdr` does not redirect flows; normal routing resumes.
- **Rule 3 — No enforcement without relay**: pf path honours this via the `deny` branch in the listener's decision dispatch — a `deny` closes the socket with a policy reason but does not silently discard or hold packets. No pf-level blocking outside the daemon listener's decision. FR-4.3 citation.
- **Rule 4 — Bundle-ID allowlist source of truth**: pf path derives QUIC intercept scope from the same Hub-pushed `agent_settings` blob that the NE path read (DEC-006 atomic reload). The daemon writes the resolved `quicFallbackUIDs` to pf rules at each `agent_settings` push; no hardcoded enforcement list in pf rules. FR-4.4 citation.
- **Rule 5 — System service exclusion**: pf path explicitly excludes `mdnsresponder` (mDNS/UDP), `configd` (DHCP), `apsd` (APNS), `nsurlsessiond`, `kdc`, `ntpd` by uid in the `rdr` rule exclusion list; the list is derived from the same allowlist as today's NE `isProtectedSystemProcess` check. FR-4.5 citation. Add note: the `! user nexus-agent` self-exclusion (FR-1.5) is the pf-layer guard against infinite-loop self-intercept.

### T9.4 — Update `docs/developers/architecture/README.md`: add trigger-row for `pfintercept` package glob

File: `docs/developers/architecture/README.md`

- Locate the existing trigger-row table that maps code globs to architecture docs.
- Add one new row:

  | Code glob | Architecture doc(s) |
  |---|---|
  | `packages/agent/internal/platform/darwin/pfintercept/**` | `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` |

- Verify after adding: `npm run check:arch-doc-triggers` must pass (A9.1 acceptance criterion).

### T9.5 — Update `docs/users/features/agent-ui/` install-flow doc: pf-only build removes "Allow System Software" gate (FR-9.4, DEC-007)

File: the existing install-and-enroll flow document under `docs/users/features/agent-ui/` (exact filename to be confirmed during implementation against the current doc set — search for the doc that describes the macOS install flow including the system extension prompt).

- Locate the prose that describes the "Allow System Software" or "System Extension" approval step in System Settings → Privacy & Security.
- Replace or annotate it with:
  - Heading variant: **"System extension prompt (NE legacy builds only)"** — scope the prompt description to `interceptMode="ne"` builds.
  - Add a note: "pf-only builds (shipped as of E74 as the `--target=pf-only` `.pkg`) do not install a system extension and do not require the Privacy & Security approval step. The install completes after the standard `.pkg` installer without any additional user action." (per DEC-007, NFR-7).
  - If the doc has a screenshot or step-by-step numbered list, add a conditional marker like "(legacy NE install only)" next to the relevant step rather than deleting content, so operators managing mixed install bases can still follow the legacy flow.

### T9.6 — Create memory anchor `project_e74_macos_pf_intercept` (FR-9.6)

File: the user memory file at `~/.claude/projects/<project>/memory/project_e74_macos_pf_intercept.md` (exact path resolved by the Claude Code harness at session time).

Content to record:

- Epic goal: replace `NETransparentProxyProvider` with a `pf` anchor + `rdr` path on macOS to close the five gap classes (NE opt-in surface, QUIC blind spot, fail-open visibility loss, per-hop latency, process attribution drift).
- Load-bearing code paths: `packages/agent/internal/platform/darwin/pfintercept/` (anchor: `nexus-agent/transparent`; listener: `127.0.0.1:13443`; natlook: `DIOCNATLOOK` cgo in `natlook/`; attribution: reuses `darwin/proc/processmeta_darwin.go`).
- Shared pipeline: all of `shared/policy/domain`, `shared/transport/tlsbump`, `shared/transport/normalize`, `shared/policy/hooks`, `shared/audit` — identical to CP path (DEC-013).
- `interceptMode` enum: `ne | pf` (hybrid retired per DEC-001); default is build-stamped (DEC-007).
- Key bindings: DEC-012 (`domain.Engine` stays shared); DEC-013 (CP and Agent share all layers except intercept boundary); fail-open anchor flush via `pfctl -a nexus-agent/transparent -F all` (DEC-009 path B + C).
- Branch: `feature/E74-E75`; docs story: `e74-s9-doc-lockstep.md`.

---

## 3. Acceptance Criteria

- **A9.1** — `npm run check:arch-doc-triggers` passes after T9.4's trigger-row addition; specifically the glob `packages/agent/internal/platform/darwin/pfintercept/**` resolves to `agent-macos-platform-architecture.md` without a "missing trigger" error.
- **A9.2** — `npm run check:doc-lockstep` passes when run against the E74 implementation PR diff; no mapped code file in `packages/agent/internal/platform/darwin/pfintercept/**` appears in the diff without a corresponding change in `agent-macos-platform-architecture.md` or `agent-ne-fail-open-architecture.md`.
- **A9.3** — `agent-ne-fail-open-architecture.md` frontmatter `title:` no longer contains the word "NE" exclusively; it reads "Agent macOS Intercept Fail-Open Architecture" (per FR-9.2). A grep for `title:.*NE` in the frontmatter block of that file must return no match.
- **A9.4** — Each of Rules 1–5 in `agent-ne-fail-open-architecture.md` contains a "pf path equivalent" subsection. Verified by grepping the file for "pf path equivalent" — must return five matches.
- **A9.5** — `agent-macos-platform-architecture.md` contains a "Relationship to Compliance Proxy" section. Grep for the heading string returns exactly one match.
- **A9.6** — Cross-references between `agent-macos-platform-architecture.md` and `agent-ne-fail-open-architecture.md` are consistent: both docs cite FR-4.x clauses consistently, and any pf-path anchor name (`nexus-agent/transparent`) or listener port (`13443`) mentioned in one is mentioned identically in the other where applicable.
- **A9.7** — The install-flow doc in `docs/users/features/agent-ui/` scopes the "Allow System Software" step to NE builds only. A reader following the pf-only install path can complete install without encountering that step.
- **A9.8** — Memory anchor file `project_e74_macos_pf_intercept.md` exists and records at minimum: the anchor name, listener port, `interceptMode` enum values, and the DEC-013 shared-layers list.
- **A9.9** — No doc file edited in this story uses the word "hybrid" in the context of `interceptMode` without qualification (e.g. "hybrid mode was retired in E74 per DEC-001"); the retired value must not appear as a live option in any updated doc section.
- **A9.10** — The E74 PR description includes a "Doc lockstep checklist" block with one checkbox per T9.1–T9.6 task, each ticked before the PR is approved for merge. Reviewer verifies the checklist is complete, not just present.

---

## 4. Interface Contract

This story is doc-only. There is no new Go package, no new API route, and no schema change.

**Single-PR lockstep requirement (binding):** ALL doc updates in T9.1–T9.6 MUST land in the same PR as the E74 code (S1–S8). This story's task list is the canonical "what to update" checklist for the PR author and reviewer. The PR description MUST include a checklist confirming each task is complete before merge.

**`check-doc-lockstep.mjs` contract:** T9.4's trigger-row addition is what makes future E74-area PRs fail the lockstep check if docs are omitted. Without T9.4, the check has no registered mapping for `pfintercept/**` and silently passes even when the architecture doc is not updated. T9.4 must therefore be done before, or in the same commit as, the first code that lands under `pfintercept/`.

**Runbook creation rule (FR-9.5):** if no agent recovery runbook exists under `docs/operators/ops/runbooks/` at the time of the E74 PR, T9.5's pf recovery command must be placed in a new file `docs/operators/ops/runbooks/agent-recovery.md`. The new file is not in scope as a full runbook write-up — only the pf recovery procedure is required. A one-section stub (`## pf anchor emergency flush`) with the two commands from T9.1 is sufficient for this story; broader runbook content is a separate follow-up.

---

## 5. Dependencies

- **S1 (pf rules)** — T9.1's §3a "Interception path (pf)" section cross-references the anchor name, `rdr` rule shape, and install/remove commands that S1 implements. S1 must be finalised before T9.1 prose is locked.
- **S2 (listener)** — T9.1's hot-path call chain and T9.3's Rule 1 / Rule 2 pf equivalents reference the listener's accept/deadline/decision-dispatch behaviour. S2 must be finalised before T9.1 and T9.3 prose is locked.
- **S3 (libproc)** — T9.1's process attribution subsection cites the `proc_pidpath` reuse and the ≥90% accuracy NFR. S3 must be finalised before that prose is locked.
- **S4 (fail-open invariants)** — T9.3 (Rule 1–5 pf equivalents) directly maps to S4's implementation. S4 must be finalised before T9.3 is locked.
- **S6 (build target)** — T9.5's install-flow doc note ("pf-only builds do not require the Privacy & Security step") depends on DEC-007's build-stamped default being real code in S6. S6 must be finalised before T9.5 is locked.
- **S7 (gap-closure tests)** — no direct doc dependency, but the E74 PR includes S7 test results; the doc story's self-audit (Step 7 workflow) should reference that S7 passed before declaring T9 complete.
- **S8 (admin-UI interceptMode dial)** — T9.5's install-flow note is consistent with the UI change in S8. Both must be reviewed together.

CLAUDE.md lockstep binding: this story's deliverables merge with all of S1–S8 in the same PR sequence. The doc story does not merge ahead of the code; the code does not merge without the doc.

---

## 6. Out of Scope

- Reformatting or reorganising doc sections not directly triggered by E74 code changes. If an existing section has stale prose unrelated to E74, record it as a separate follow-up rather than bundling it here.
- Adding new architecture docs beyond what FR-9.1–FR-9.5 enumerate. E74 does not introduce a new subsystem — it replaces the intercept layer of an existing one.
- Updating `agent-linux-platform-architecture.md` or `agent-windows-platform-architecture.md` — those platforms are unchanged by E74.
- Translating doc content into languages other than English (CLAUDE.md English-only binding).
- Updating the E62 doc set (`endpoint-typology-architecture.md`) — E74 does not change endpoint classification semantics.
- Writing or updating the E75 stories — E74 and E75 share a worktree but are separate epics; doc updates for E75 are in E75's own SDD.
- Deleting NE Swift doc references — NE code stays in tree during the cutover window per DEC-001; doc references to NE remain valid until the Phase 8 cleanup decision is made.

---

## 7. References

- Requirements: `docs/developers/specs/e74-macos-pf-intercept.md` §FR-9.1–FR-9.6
- Decisions: `docs/developers/specs/e74/DECISIONS.md`
  - DEC-001 — `interceptMode` enum collapses to `ne | pf`; hybrid retired
  - DEC-007 — build-stamped default; pf-only `.pkg` stamps `default = "pf"`; legacy stamps `default = "ne"`
  - DEC-012 — `domain.Engine` stays shared between CP and Agent; no agent-private fork allowed
  - DEC-013 — CP and Agent share all layers except intercept boundary (user-binding 2026-05-21)
- CLAUDE.md bindings:
  - Code/doc lockstep: "doc updates land in the SAME PR as the code that triggers them"
  - `check-doc-lockstep.mjs` + `check:arch-doc-triggers` CI enforcement
  - Pre-edit 3-doc rule (architecture + feature + conventions) applies to each doc touch in this story
- Cross-reference SDDs: e74-s3-libproc-pid-attribution.md, e74-s4-fail-open-invariants.md, e74-s6-build-agent-pf-target.md
- Related architecture docs (read before editing):
  - `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`
  - `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md`
  - `docs/developers/architecture/README.md`
- Related feature docs (read before editing T9.5):
  - `docs/users/features/agent-ui/` (install-flow and enrollment docs)
- Related operator docs (read before editing T9.5 runbook note):
  - `docs/operators/ops/runbooks/` (agent recovery runbook, or new file if absent)
- E86 cross-reference: `docs/developers/specs/e86-e2e-coverage-matrix.md` — the new `/test-macos-pf-agent` skill introduced by E74 (FR-7.6) must be registered in this matrix so future features cannot land without the pf-path acceptance gate; this registration is the responsibility of the S7 story, not S9, but the S9 PR review checklist should confirm it is present before merge.
- Lockstep config file: `scripts/doc-lockstep.config.mjs` — the E74 PR must add the `packages/agent/internal/platform/darwin/pfintercept/**` glob → `agent-macos-platform-architecture.md` mapping to this config, separate from (but consistent with) the `architecture/README.md` trigger row added in T9.4.
