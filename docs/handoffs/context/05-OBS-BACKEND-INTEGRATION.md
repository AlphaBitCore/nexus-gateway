# obs-api ↔ Arena integration contract

The seam between **Kash's obs-api** (decides when to run, streams results to the
site) and **Kanishk's Arena** (the AWS fleet obs-api controls). This is the
"verify in detail" doc — if both sides build to this, they connect on first try.

Kanishk's plan (7/6) matches the design. This pins the four points where the
two halves must agree exactly.

## Confirmed + scope for Kanishk (start here)

**Your two confirmations — both accurate in `arena-profile.json`:**
- **Tag key:** `nexus:arena-role` (values: each gateway id, plus `loadgen`, `mock`).
- **Health ports/paths:**

| gateway id | box | port | health path |
|---|---|---|---|
| `nexus-hooks-on` | 1 | 3050 | `/healthz` |
| `nexus-hooks-off` | 1 | 3050 | `/healthz` |
| `agentgateway` | 1 | 8080 | `/healthz` |
| `bifrost` | 1 | 8080 | `/metrics` |
| `litellm` | 1 | 4000 | `/health/liveliness` |

(competitor ports/paths are best-effort — verify against each image's actual
readiness endpoint when you bake it; correct in the profile if any differ.)

**Fleet count — each gateway ENTRY is its own box.** `nexus-hooks-on` and
`nexus-hooks-off` are the same image in two configs → **two boxes**, so results
show side-by-side live. Current lineup = **5 gateway boxes + 1 mock + 1 loadgen
= 7** (final set pending Tieben).

**ec2.go scope split — build Start/Stop now, NOT Run's metric half yet:**
- ✅ **`Start`** (DescribeInstances-by-tag → StartInstances → health-gate on the
  table above) and **`Stop`** (StopInstances → verify) are fully specified —
  build these now. Plus your IAM policy, watchdog, provisioning script.
- ⛔ **`Run`'s metric collection** depends on the phone-home ingest endpoints
  (`/internal/pending-run`, `/internal/metrics`) that are **Kash's next task**
  and gated on the phone-home-vs-SSM decision (below). Don't implement Run's
  metric pump against a guessed contract — Start/Stop + lifecycle is your
  critical path; Run's collection lands once the ingest endpoints exist.

**Open methodology item (tag Tieben) — affects your mock sizing.** Do the
gateways run **in parallel** (all 5 boxes driven at once — snappy UX, but the
single mock box then serves ~5× the preset RPS and could become the bottleneck,
and it diverges from the published *mutual-exclusion* methodology) or
**sequentially** (matches published numbers, but one run cycles through all
gateways so it's ~5× longer)? This decides whether one mock box suffices or the
mock must be scaled. Not a blocker for Start/Stop/IAM/watchdog — flag it before
finalizing the mock box.

## Where obs-api runs (decides everything else)

**obs-api runs on a small ALWAYS-ON control box (t3.small), not on a fleet
instance.** The fleet (gateways + loadgen + mock) is stopped between runs, so
obs-api cannot live on it — it has to be up to receive the Run request that
starts the fleet. The control box is outside the stopped set.

## 1. Discovery — by TAG, not instance IDs ✅ change made

obs-api finds the fleet by tag, so you can re-provision/replace instances
without reconfiguring obs-api. Tag every fleet instance:

```
nexus:arena-role = <gateway-id> | "loadgen" | "mock"
```

(`discovery.tagKey` in `arena-profile.json`.) The IAM policy and the watchdog
**scope on this same tag** — one source of truth.

## 2. IAM scope — your 3 actions ARE enough, given the loadgen model below

Your scope (start/stop/describe on the tagged fleet) is exactly right **iff**
obs-api never has to remotely execute the load test. It doesn't, under the
recommended model:

- ✅ `ec2:DescribeInstances`, `ec2:StartInstances`, `ec2:StopInstances` — scope
  with a condition on `aws:ResourceTag/nexus:arena-role`. Note: `Describe*`
  can't be resource-scoped by AWS (it's list-wide) — that's expected and fine;
  start/stop carry the tag condition.
- ❌ **No `ssm:SendCommand`, no SSH.** obs-api does not push commands to the
  loadgen box. See #3.

> **The one decision to confirm at the sync:** loadgen **phone-home (pull)** vs
> **obs-api push (SSM)**. Phone-home keeps your IAM at exactly 3 actions. Push
> (SSM RunCommand) would add `ssm:SendCommand` + `ssm:GetCommandInvocation` to
> obs-api's role — broader than your plan. **Recommend phone-home.**

## 3. Triggering the run + collecting metrics — loadgen phones home (pull)

Per-run lifecycle:

1. Visitor hits Run → obs-api `StartInstances` on the tagged fleet.
2. obs-api health-gates: polls each gateway's `http://<private-ip>:<port><healthPath>`
   (ports/paths are in `arena-profile.json` per gateway) over the private VPC
   network until all healthy.
3. The **loadgen box is pre-baked to phone home on boot**: its `docker compose
   up` runner calls obs-api `GET /internal/pending-run` (control-box private
   addr, shared-token auth), gets `{runId, rps, durationSec, targets[]}`, runs
   the load test, and streams `--live-json` samples to obs-api
   `POST /internal/metrics` (same token). obs-api relays them to the browser SSE.
4. On completion obs-api collects the final summary, `StopInstances`, verifies
   stopped, writes the history snapshot.

What you build on the loadgen box: the boot runner (pull spec → run loadtest →
stream metrics). What I build on obs-api: `GET /internal/pending-run` +
`POST /internal/metrics` (internal, token-auth) — the next obs-api task; the
`ec2` driver already documents consuming from the resulting ingest channel.

`--live-json` on nexus-loadtest (periodic `{gateway,rps,latP50Ms,latP99Ms,ttftP95Ms,memUsedPct,cpuPct,okPct,ts}`)
is the shared metric shape (`driver.Metric`).

## 4. Timeouts — must be LAYERED, not equal ✅ change made

```
longest preset (soak 30m) + start/stop overhead (~5m)   ≈ 35m
  <  obs-api in-process run ceiling                          40m   (orchestrator)
  <  your independent auto-stop watchdog                     45m   (backstop)
```

Keep this ordering. The watchdog must only ever fire when the in-process
ceiling has already failed to stop the fleet — never as the normal path. (The
public `quick-verify` preset is ~90s, so this only matters for the admin soak.)

## Also on obs-api's side (not AWS, so not your IAM)

- obs-api needs the **site repo deploy key** to commit history snapshots (a
  GitHub credential on the control box, not an AWS permission).
- CORS on obs-api locks to the site origin in prod (B2).

## Open, pending Tieben

- Final gateway set + the **preset RPS = Agent-Gateway audit-on saturation
  point** → dropped into `arena-profile.json` (`saturationRps` + preset `rps`).
  Tieben's ETA: streaming done ~04:45 CST, nostream ~09:45 CST, data+report
  tomorrow evening. Nothing above waits on it — only the number does.

## Verified-matching checklist (for the sync)

- [x] pre-baked, stopped-between-runs, c6i.4xlarge, one box/gateway + mock + loadgen
- [x] scoped IAM = start/stop/describe (sufficient under phone-home)
- [x] independent 45-min watchdog as backstop (layered below at 40m in-process)
- [x] billing alarm
- [x] discovery tag key `nexus:arena-role` confirmed
- [x] health ports/paths confirmed (table above; verify competitor endpoints when baking)
- [x] fleet count clarified (each gateway entry = 1 box; nexus on/off = 2 boxes; 7 total)
- [ ] **confirm phone-home vs SSM** (plan-of-record: phone-home → your 3-action IAM is correct; SSM would add ssm:SendCommand + GetCommandInvocation)
- [ ] **parallel vs sequential + mock sizing** (tag Tieben — decides one mock box vs scaled)
- [ ] ec2.go: Start/Stop built now; Run metric-collection after Kash's ingest endpoints land
