# arch-doc-trigger-check

Verify the architecture doc trigger map lockstep, optionally help add a new row when shipping a new arch doc.

The trigger map (`docs/developers/architecture/README.md`) is the canonical "what to read when editing X" index. Every `docs/developers/architecture/**/*-architecture.md` must have a row, and every row must point to a real doc. The lockstep is enforced by CI.

Use this skill when:

- You added or renamed a `docs/developers/architecture/**/*-architecture.md`.
- You're about to merge a PR and want to confirm the trigger map is consistent.
- A teammate's PR is failing `check:arch-doc-triggers`.

---

## Quick check

```bash
npm run check:arch-doc-triggers
```

Output explains:
- `OK -- N architecture doc(s) referenced in trigger map.` → consistent.
- `FAILED -- Missing trigger-map row for: docs/developers/architecture/<service>/X.md` → add a row.
- `FAILED -- Trigger-map references missing doc: docs/developers/architecture/<service>/X.md` → fix the path or remove the dangling row.
- `WARNING -- N '(planned)' marker(s) in table rows.` → drop on merge.

## Adding a row for a new arch doc

1. Open `docs/developers/architecture/README.md`.
2. Find the table.
3. Append a new row matching the existing pattern:

```md
| <Editing area / file glob description> | `docs/developers/architecture/<service>/<your-new-doc>.md` |
```

The "editing area" cell should be descriptive enough that a contributor reading the trigger map can find their edit area without code inspection. Conventions:

- Lead with file globs in code-quoted form (`packages/.../foo/**`).
- Include synonyms (e.g., "interception domain rules" + "domain pattern matching").
- End with the conceptual topic (e.g., "TLS bump", "credential pool health").

4. Run `npm run check:arch-doc-triggers` to confirm.
5. Commit alongside the new arch doc.

## Adding a `(planned)` row while drafting

If the doc is in flight in another PR but you want to land the trigger row first:

```md
| <Editing area> | (planned) `docs/developers/architecture/<service>/<your-new-doc>.md` |
```

The `(planned)` marker tells the check script + future contributors that the doc is being drafted. Drop the marker on merge.

## When a doc is being subsumed

If `<old>.md` is being absorbed into `<new>.md` (e.g., `credential-state.md` → `credentials-architecture.md`):

1. Update the trigger row to point to the new doc: `\`docs/developers/architecture/<service>/<new>.md\` *(subsumes \`<old>.md\`)*` to flag the relationship.
2. Leave the old file with a one-line redirect during a transition PR, OR delete it in the same PR that lands the new doc (greenfield phase per CLAUDE.md).

## When a doc is renamed

1. Rename the file (`git mv`).
2. Update the row in `architecture-doc-triggers.md`.
3. Update any cross-references in other arch docs (`grep -rn old-name docs/`).
4. Run `npm run check:arch-doc-triggers`.

## Verifying CLAUDE.md doc-trigger contract

The CLAUDE.md "Architecture doc triggers" section delegates to `docs/developers/architecture/README.md` and `.cursor/rules/architecture-doc-triggers.mdc`. These three artifacts MUST stay in agreement:

- CLAUDE.md says "see `docs/developers/architecture/README.md`".
- The Cursor rule cross-links to the same.
- The check script enforces every `*-architecture.md` is in the map.

If any of these three drifts, the doc-trigger contract is broken. Fix in the same PR.
