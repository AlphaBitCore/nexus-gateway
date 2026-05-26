# Archive

Historical documentation. Anything here is **frozen in time** — kept for
archaeology, not for active reference. If you're an OSS visitor wondering
"is this current?", the answer is: **no**, look at the live trees under
[`../developers/`](../developers/) and [`../users/`](../users/) instead.

## Partitioning

By calendar quarter. New archived material goes under `<year>-q<n>/<bucket>/`.

## Buckets

| Bucket | What goes here |
|---|---|
| `brainstorms/` | `/brainstorm`-style design exploration docs — RFC-shaped, not always implemented as written |
| `programs/` | Multi-session program planning artifacts: baselines, gap-maps, structural deltas, status snapshots |
| `handoffs/` | Session-to-session handoff documents written when context was filling up |

## Current contents

- `2026-q2/brainstorms/` — 48 design exploration docs from the 2026-04
  through 2026-05-06 superpowers-era brainstorms.
- `2026-q2/programs/` — 15 program plans / baselines / handoffs from the
  test-coverage-95% program, the open-source readiness review, and the
  go-package-domain reorganization.
- `2026-q2/handoffs/` — the E60 attestation feature handoff.

## When to add something here

- A program closed out (the test-coverage-95% program is the worked example).
- A multi-session program produced a handoff doc and the next session
  picked it up — archive the original handoff.
- A design exploration was superseded by an implemented architecture doc
  in [`../developers/architecture/`](../developers/architecture/README.md) —
  move the exploration here with a one-line pointer at the canonical doc.

Don't archive an active doc. If you're not sure it's done, leave it where
it is.
