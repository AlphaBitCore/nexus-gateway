# Claude Code Task Prompts — Nexus onto the Fleet (Jul 8)

> Each task below is a self-contained prompt you can paste into Claude Code.
> Shared context every task should load first:
> - `docs/handoffs/program.md` + `docs/handoffs/context/10-BACKEND-MASTER-PLAN.md`
>   (design) and `docs/handoffs/REMAINING-WORK-CHECKLIST.md` (action list).
> - **Repo caveat:** the June-19 local checkout is STALE. Work against the CURRENT
>   repo (`AlphaBitCore/nexus-gateway`, container work in PR #81 / branch
>   `feature/container-deploy`). Verify head with `git log --oneline -20` before editing.
> - **Model note:** the 4 images include security-adjacent code (vectorscan PII
>   scanning, credential encryption). Use Opus 4.8 for those tasks; Fable 5 may refuse.
> - Decision of record: **Nexus reaches the fleet via private Amazon ECR** (Option 1).
>   Docker Hub (public) stays on the roadmap but is NOT the live-run blocker.

---

## TASK 1 — Diagnose & fix the failing build test (DO FIRST, blocks everything)

**Goal:** Understand and fix the failing test in the code that builds the 4 Nexus
images, so the built images can be trusted. Do NOT skip, `t.Skip`, or delete the
test to make it pass — diagnose the root cause.

**Non-goals:** Don't publish anything yet. Don't refactor unrelated packages.

**Steps:**
1. In the current repo, reproduce the failure: run the build + `go test ./...`
   for the packages behind the 4 images (`nexus-hub`, `nexus-ai-gateway`,
   `nexus-compliance-proxy`, console/control-plane) and the container build path
   (`packages/**`, plus any `smoke-redaction` / `hs-selfcheck` gates). Capture the
   exact failing test name + output.
3. Root-cause it. Likely suspects given history: vectorscan link/self-test
   (`HSSelfTest`, `go version -m` tag check, `FAT_RUNTIME`/`vsstatic` linkage), a
   `GOWORK=off`/sibling-replace drift, a missing build dep (e.g. `libsqlite3-dev`),
   or a coverage-rule failure (repo requires ≥95% per package). State which.
4. Fix the real cause. If it's a genuine product bug (e.g. scanner silently
   falling back to RE2), fix the code; if it's a test asserting the wrong thing,
   fix the assertion and explain why.
5. Re-run until `go test ./...` for the affected packages is green, AND the
   vectorscan gates pass (build-time `HSSelfTest` + runtime `hs-selfcheck` prints
   `scanRC=0 matches=1`).

**Acceptance:** paste the failing-test output, the root-cause explanation, the
diff, and the green re-run. Confirm no test was skipped/deleted to pass.
Report back the cause in one paragraph before moving on.

> **Jul 8 diagnostic pass (Kash, in this repo — partial, environment-limited):**
> Ran on `feature/container-deploy` (`bde69d53`, current, not the stale checkout).
> - The **default RE2 path is green** — `go test ./policy/hooks/matcher/` passes
>   (`ok … matcher 0.345s`). So the failure is NOT in the pure-Go build.
> - The matcher's substantive tests (`vectorscan.go`, `hs_selftest.go`,
>   `cgo_scan_limit`, `detection_bound`, `seed_residual_gate`, `perrule_profile`,
>   `prescan_debug`) are all `//go:build vectorscan` — they need CGO + a static
>   **libhs**, which is NOT installed in this environment and can't be produced
>   here (the ~20-min libhs compile happens inside the ai-gateway/compliance-proxy
>   Dockerfiles). The `matcher` package is NOT in `scripts/.coverage-allowlist`,
>   so under `-tags vectorscan` it must hit ≥95% via those tagged tests.
> - **Conclusion:** the failure is almost certainly in the **vectorscan-tagged /
>   in-Docker gate path** (build-time `HSSelfTest`, runtime `hs-selfcheck`, the
>   `vsstatic` linkage, or the ≥95% coverage gate when the tagged tests don't run),
>   not the RE2 path. It is **not reproducible without libhs + the Docker build**.
> - **To finish this task, an executor needs one of:** (a) Kanishk's exact failing
>   test name + output, or (b) a libhs-equipped environment (or just run the two
>   image Dockerfiles, which compile libhs and run the gates). Do NOT guess-fix.

---

## TASK 2 — Publish the 4 images to private Amazon ECR (after Task 1 green)

**Goal:** Make Nexus pullable by the two arena Nexus boxes from a private ECR in
the arena's AWS account, the same way every other gateway box pulls its image.

**Non-goals:** No public Docker Hub push here (separate, James-gated). Don't bake
into an AMI. Don't weaken the redaction gate.

**Steps:**
1. Create ECR repos (or a script under `obs-backend/aws/` that does):
   `nexus-console`, `nexus-hub`, `nexus-ai-gateway`, `nexus-compliance-proxy`,
   `nexus-db-migrate`.
2. Build all five from PR #81's Dockerfiles for `linux/amd64`, tag each
   `→ <acct>.dkr.ecr.<region>.amazonaws.com/nexus-<svc>:<gitsha>` (immutable tag).
3. **Gate the push of the two scanning images** (`ai-gateway`,
   `compliance-proxy`) on the runtime redaction smoke — run the built image, POST
   seeded PII, assert `[REDACTED_*]`. Fail the push if it doesn't redact.
4. Push; capture each image DIGEST (`repo@sha256:...`).
5. Add a minimal instance-role policy snippet (`ecr:GetAuthorizationToken` +
   `BatchGetImage`/`GetDownloadUrlForLayer`, scoped to those repos) for the two
   Nexus boxes.
6. Point `docker-compose.full.yml` (arena Nexus box variant) at the ECR refs;
   document the `aws ecr get-login-password | docker login` step.

**Acceptance:** the five images exist in ECR; `docker pull` of each digest works
from a box carrying the instance role; the redaction smoke gated the two scanning
images (paste the smoke output); the digests are recorded (they feed run
provenance). Provide the ECR push script + the IAM snippet as files.

> **Jul 8 (Kash): the two files are authored + pushed** on `feature/container-deploy`
> (`c53f96e8`): `deploy/docker/ecr-publish.sh` (mirrors `docker-publish.yml`'s
> exact 5-image/Dockerfile recipe + the vectorscan `hs-selfcheck` push gate,
> targets private ECR with an IMMUTABLE git-sha tag, records digests) and
> `deploy/docker/ecr-pull-policy.json` (pull-only instance-role policy scoped to
> the 5 `nexus-*` repos). Syntax-checked (`bash -n`, JSON valid). **NOT run** —
> the actual create/build/push still needs AWS creds for the arena account AND
> Task 1 green (a red scan path must never publish).

---

## TASK 3 — Close the loadgen metric-source gaps (before any publishable number)

**Goal:** Make per-gateway `memUsedPct` real for every gateway (not just Bifrost)
and give `cpuPct` a source, and replace the assumed `--live-json` parser with the
real format. Until this is done, runs are demo-only, not publishable.

**Context:** `obs-backend/aws/loadgen/` currently scrapes Bifrost's Prometheus
`process_resident_memory_bytes`; other gateways report `0` (honest, not
fabricated); `cpuPct` has no source. The agentgateway-OOM memory chart is the
headline visual, so memory must be real across all 5 gateways.

**Steps:**
1. Decide + implement the mechanism (recommend a lightweight host-metrics reader
   per gateway box — e.g. read `/proc` or node-exporter — over loadgen-side
   guessing). Wire `memUsedPct` + `cpuPct` for all 5 gateways into the
   `driver.Metric` samples posted to `/internal/metrics`.
2. Replace the assumed `--live-json` parse with the real
   `nexus-loadtest --live-json` schema once available; until then, keep the
   parser behind a clearly-marked adapter and a fixture test.
3. Keep the honesty rule: any metric with no real source emits `0`/`null`, never
   a fabricated value.

**Acceptance:** a `sim`/`compose` run shows non-zero real memory + CPU for each
gateway; unit test covers the real `--live-json` shape; no fabricated values.

---

## TASK 4 — Branch hygiene + merge (quick, low-risk)

**Goal:** Unify the backend branches cleanly before the first supervised run.

**Steps:**
1. Rename `dashboard-v1` → `feat/b1-arena` (it's the Arena backend, not a dashboard).
2. Reconcile/merge `feat/backend-contract-v2` (`2da2747`) and `feat/b1-arena`
   (`98707f5`) — both touch `obs-backend/`. Run `go test ./...` green post-merge.
3. `git log --oneline -20` on each branch; confirm no stray untracked head commits.

**Acceptance:** one merged line, `go test ./...` green, branch renamed.

---

## After these land (not Claude Code — people/ops)
- **Supervised first FULL live run** (A3): Run button → fleet start → health-gate
  → loadgen → SSE → done → stop → verified snapshot in the timeline; watchdog
  never fires. Needs Tasks 1–2 (Nexus boxes pulling from ECR) + governance bake.
- **Governance bake** (A5): one Nexus box hooks-on, one hooks-off, per
  `NEXUS_GOVERNANCE_TOGGLE.md`.
- **Add agentgateway to the fleet** — intentionally held out; add once the
  crash-under-load-as-expected handling is confirmed end-to-end.
- **Permanent control server** — replace today's temporary obs-api control box.
- **Tieben (G2):** final preset RPS (note: ~24k is where agentgateway starts
  failing with logging on — that's near the saturation anchor) + streaming vs
  non-streaming + run-mode confirm.
- **James (G3):** methodology sign-off before the button goes public.
- **Docker Hub (G5):** public publish — roadmap, not a live-run blocker.
