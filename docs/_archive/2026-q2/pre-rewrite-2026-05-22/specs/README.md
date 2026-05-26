# Specs

Per-epic spec bundles. Each `eNN/` directory holds the requirements + SDD
documents for one epic. OpenAPI specs for the same epic live under
[`../../users/api/openapi/`](../../users/api/openapi/) grouped by API surface
(not by epic).

## Layout

```
specs/
├── e3/                    ← one directory per epic
│   └── e3-s5-config-sync-remediation.md
├── e56/
│   ├── e56-responses-api.md       ← requirement
│   ├── e56-s1-endpoint-format-route.md  ← per-story SDD
│   ├── e56-s2-codec-request.md
│   └── ...
├── e62/
│   └── ...
└── misc/                  ← non-epic-numbered specs (one-off fixes)
```

Naming convention:
- `eNN-<name>.md` — epic-level **requirement** (Functional / Non-Functional / User Roles / Constraints / Glossary / MoSCoW)
- `eNN-sM-<name>.md` — story-level **SDD** (user-story statement + tasks + acceptance criteria)
- `eNN-sM-<name>.yaml` — story-level OpenAPI spec, lives under `../../users/api/openapi/<surface>/`

## Workflow

The SDD pipeline (see [`../workflow/ai-workflow.md`](../workflow/ai-workflow.md)):

```
Plan + Todo → Architecture → Requirements → SDD → OpenAPI → Code → Tests → Verify
```

So a new feature lifecycle touches:
1. Read the architecture doc(s) listed in [`../architecture/README.md`](../architecture/README.md)
2. Write `eNN-<name>.md` (requirement) here
3. Break into stories — one `eNN-sM-<name>.md` per story, here
4. Write `eNN-sM-<name>.yaml` per story under `../../users/api/openapi/<surface>/`
5. Implement
6. Test
7. Verify against the SDD's acceptance criteria
