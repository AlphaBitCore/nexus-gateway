# Epic Status Tracker — moved

> **This file has moved. The canonical living tracker is now `docs/dev/roadmap.md`.**

Reason for move: the tracker is the right surface for both internal maintainers AND any future public reader (OSS evaluators, customer technical staff). Living it in `docs/dev/_internal/` signalled "maintainer scratch" and discouraged outside readers from finding it. The new location `docs/dev/roadmap.md` is the same content (every epic + its detailed block) plus three additions:

- Strict `Status` enum (`Shipped | In-progress | Planned | Draft | Deferred | Cancelled`).
- AI-session query examples (how to grep for "what's not done").
- Multimodal-family dependency graph (E62 → E63/E64 → E65 → E66).

This stub stays so that links pointing at `_internal/epic-status.md` don't 404. Update any references in `CLAUDE.md`, memory files, or other docs to point at the new path.

Last living version of this file: see `git log -1 -- docs/dev/_internal/epic-status.md` (commit `2274dd50` was the latest pre-move state).
