# benchmark/v3

The replacement for `benchmark/v2`'s measurement core, built to survive hostile
external review (this will be open-sourced). Two stories that answer different
questions and are **never mixed in one table**.

> **Status — foundation only.** This tree currently ships the *validation +
> provenance foundation* (schema, mandatory validator, version-lock, provenance
> capture, tests). The ECS/Fargate deployment (Story A runner) and the rig-campaign
> importer (Story B) are **deferred** — see "Deferred" below. No cross-gateway
> ranking or public claim may be produced yet.

## Purpose

`benchmark/v2`'s governance (hooks runtime proof, S-02 dataset, quarantine idea) was
sound, but its measurement core — closed-loop self-throttling load, censored
percentiles, co-located topology, t-class fleet, floating image tags, unverified
upstream — is not valid for cross-gateway latency or capacity claims. v3 rebuilds
that core with pre-registered, symmetric validation and full provenance.

## Story A vs Story B

| | Story A | Story B |
|---|---|---|
| Question | latency over time, per gateway/version | saturation capacity / concurrency |
| Load | continuous, low, **open-loop** (~50 rps) | rig saturation campaign |
| Topology | one ECS/Fargate service per gateway, dedicated in-VPC mock | rig: one gateway per c6i.4xlarge |
| Source | this repo (ECS) | imported from `ABC/llm-gateway-benchmark`, verbatim |
| Claims | per-gateway trends only | capacity, with full disclosure |

Story A and Story B rows are structurally separated (`story` field + separate
`results/` dirs) and `shared/validation/validator.py::assert_no_cross_story()`
raises if anything tries to merge them.

## What may be claimed / what may not

- **May:** per-gateway latency trends over time (Story A); saturation capacity with
  the rig's disclosed methodology (Story B); version-over-version comparisons via the
  lock.
- **May not:** cross-gateway rankings from Story A; any claim from a run that failed
  the validator; anything from `benchmark/v2` (see its `PRE-GATE — NOT PUBLISHABLE`
  marker); any "Nexus wins X" that wouldn't survive resolving every disclosed
  asymmetry against Nexus.

## Vendor-operated benchmark disclosure

This is a vendor-operated benchmark. Posture differences (auth/audit/logging) are
tabulated next to every number. Nexus is built **from source in this repo** at a
locked commit — no opaque AMI — and provenance enumerates exactly which services ran.

## Non-negotiables (both stories)

1. Open-loop for any latency claim; offered load is an input, never a function of a
   gateway's speed.
2. Every request content-validated (`chatcmpl-llm-mock` id + min tokens); a failed
   check is a terminal outcome, not a silent success.
3. Every terminal outcome accounted for (success/timeout/4xx/5xx/broken/invalid == total).
4. Versions resolved → locked → archived (`config/versions.lock`).
5. No burstable compute (Fargate dedicated vCPU, or pinned c-class).
6. Explicit disk IOPS/throughput wherever disk is on the write path (audit is disk-heavy).
7. Symmetric validation, pre-registered criteria; invalid runs quarantined WITH raw
   artifacts, never deleted.
8. Nexus built from source at the locked commit; running services enumerated.

## Layout

```
config/          story-a.yaml, gateways.yaml, validation.yaml, versions.lock
shared/
  schemas/       run.schema.json, provenance.schema.json   (canonical contracts)
  validation/    validator.py        (gateway-agnostic rules + assert_no_cross_story)
  version_lock/  version_lock.py     (resolve channels → pins, diff locks)
  provenance/    provenance.py        (build the reproducibility record)
scripts/         validate_run.py, resolve_versions.py, capture_provenance.py
story_a/         results/ (+ deferred ecs/, loadtest/ — see story_a/README.md)
story_b/         results/ (+ deferred importer/ — see story_b/README.md)
tests/           stdlib unittest
```

## How to reproduce Story A (once the runner lands)

The mandatory pipeline — the validator is a stage, not an optional script:

```
resolve_versions.py  →  (deploy service + run open-loop load)  →  capture_provenance.py
   →  validate_run.py  →  publish to story_a/results/  OR  quarantine
```

## How Story B imports rig campaigns (once the importer lands)

`import_rig_campaign.py` lifts a rig campaign's results **verbatim** (never
recomputes), rejects it if the generator-health block is absent/unhealthy or raw
artifacts/lock are missing, and appends to `story_b/results/history.jsonl`.

## Version-lock policy

`resolve_versions.py` resolves each gateway's release channel to an exact
version+digest (+ commit for Nexus) and writes `config/versions.lock`. When a pin
changes, the run's time series is annotated at that version boundary, so "did the
new release regress TTFT" is a lookup, not a re-run.

## Validation / invalidation policy

`validate_run.py` schema-checks the run then applies `validator.py`'s pre-registered
rules (open-loop, upstream verified, complete outcome accounting, no burstable, disk
spec when audit-on, digest-pinned, provenance/lock/raw present, warmup present,
services enumerated, timeout-rate ceiling). Rules are **gateway-agnostic** — the same
bad input invalidates any gateway. Invalid runs move to `results/quarantine/` with a
`.reasons.json`, raw retained.

## Raw artifact retention

Every run keeps its raw results, provenance, and the campaign lock; discarded/invalid
runs keep theirs too (quarantined, not erased).

## Deferred (not in this foundation)

- Story A ECS/Fargate deploy + open-loop runner (`story_a/ecs`, `story_a/loadtest`).
- Story B rig importer (`scripts/import_rig_campaign.py`, `story_b/importer`).
- Hooks A/B runtime-convergence carry-forward from v2 (hardened).

These need AWS / the rig archive and are the infra lane; they are documented here and
in the story READMEs but intentionally not stubbed.

## Running the tests

```
pip install -r requirements.txt          # jsonschema + PyYAML
cd benchmark/v3 && python -m unittest discover -s tests -v
```
