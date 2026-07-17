# Story B — periodic saturation campaign importer

**Question answered:** saturation capacity / concurrency behaviour, imported from
the existing rig `ABC/llm-gateway-benchmark` (one gateway per c6i.4xlarge,
host-native, dedicated mock + loadgen, gp3 disks, health/cooldown gates,
generator-health invalidation, N-round medians). **The rig is not rebuilt here.**

Story B rows are **never** merged into a Story A table — the two answer different
questions on different topologies. `../shared/validation/validator.py`
`assert_no_cross_story()` enforces that in code.

## Built (this foundation)
- Story B runs share the same canonical schema (`../shared/schemas/run.schema.json`),
  with `story: "B"` and `run_mode: "saturation_campaign"`.
- The validator + version-lock apply to imported campaigns too.

## Deferred — infra lane (needs the rig archive; Tiebin's design)
Not built here, and intentionally **not stubbed**:
- `importer/` + `scripts/import_rig_campaign.py` — parse a rig campaign archive,
  **lift results verbatim (never recompute)**, reject if the generator-health block
  is absent/unhealthy or raw artifacts / versions.lock are missing, then append to
  `results/history.jsonl` joined with that campaign's lock.

`results/` will hold imported campaign records once the importer lands.
