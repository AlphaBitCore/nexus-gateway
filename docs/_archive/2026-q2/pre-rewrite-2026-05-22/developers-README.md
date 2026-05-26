# Developers

Documentation for contributors — how the system works, how to build a
feature, and the AI vibe-coding workbench that supports it all.

## Sections

| Folder | What's there |
|---|---|
| [`architecture/`](./architecture/README.md) | System architecture. The README is the **trigger map**: "if you edit X, read Y first." Binding. |
| [`workflow/`](./workflow/) | The AI vibe-coding workbench: SDD pipeline, conventions, local-dev debugging, testing, coverage methodology, session-handoff template |
| [`specs/`](./specs/) | Per-epic spec bundles — requirements + SDDs grouped by epic number |
| [`roadmap.md`](./roadmap.md) | In-flight + queued maintainer roadmap |

## Quick paths

- **New to the codebase** → read in order: `../README.md` → `architecture/overview.md` → pick a feature, open the matching `architecture/services/<svc>/*.md`
- **Picking up an existing feature** → `architecture/README.md` (trigger map) → epic bundle in `specs/`
- **Building a new feature** → `workflow/ai-workflow.md` (SDD pipeline + Plan-first discipline)
- **Adding a binding rule** → `../../.cursor/rules/` + update `../../CLAUDE.md`
- **Triggering an automation** → `../../.claude/skills/` (see catalog: `workflow/ai-skill-catalog.md`)

## The trigger map

[`architecture/README.md`](./architecture/README.md) lists every architecture
doc and the file glob it covers. **Editing code in an area listed there
without reading the doc first violates the binding pre-edit rule.**
Enforced by `npm run check:arch-doc-triggers`.

If you touch code in an area NOT listed there, that's a signal: either the
architecture is undocumented (raise it) or this is a new subsystem (add doc
+ row, in the same PR).
