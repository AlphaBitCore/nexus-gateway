# `.claude/skills/`

Invocable procedures the AI agent fires as `/skill-name` in Claude Code.
Each skill is a self-contained runbook with trigger keywords, pre-
conditions, steps, and a verification gate.

The **full catalog**, including buckets (deployment / testing /
architecture / debug / audit), per-skill OSS portability notes, and
adaptation guidance for forks lives at:

→ **[`docs/developers/workflow/ai-skill-catalog.md`](../../docs/developers/workflow/ai-skill-catalog.md)**

The catalog explains:

- What each of the 24 skills does
- Which are tightly coupled to this repo's prod infra (`taskforce10x.com`)
  and how to swap them for a fork
- When to write a new skill vs extend an existing one
- The structural template every skill follows

For the broader AI vibe-coding workflow these skills slot into, read
[`docs/developers/workflow/ai-workflow.md`](../../docs/developers/workflow/ai-workflow.md).
