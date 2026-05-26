# Dev Code Doc Lockstep

*Audience: contributors making code changes that touch documented subsystems.*

Code / doc lockstep is the rule that when a PR touches code in a region covered by a doc, every matching doc must update in the **same commit**. Docs are the single source of truth — if code drifts from a doc, the doc says one thing and the code does another, and the next reader is misled. The rule is enforced by `scripts/check-doc-lockstep.mjs` (run at `npm run check:doc-lockstep`) against the PR diff, and wired into both pre-commit and CI.

---

## What triggers a lockstep update

The mapping from code globs to required docs lives in `scripts/doc-lockstep.config.mjs`. Each entry lists one or more code glob patterns and one or more doc files that must appear in the PR diff when any matching code file changes.

The current locked pairs are:

| Entry name | Code glob(s) | Required doc(s) |
|---|---|---|
| `cost-estimation` | `ai-gateway/internal/ingress/proxy/proxy*.go`, `internal/cache/layer/pricing.go`, `execution/estimator/**`, normalize codecs | `cost-estimation-architecture.md` |
| `provider-adapter` | `ai-gateway/internal/providers/specs/**` | `provider-adapter-architecture.md` |
| `normalize-codecs` | `shared/transport/normalize/{codecs,extract,core}/**` | `normalization-architecture.md` |
| `thing-config-sync` | thingclient, hub things, configdispatch packages | `thing-config-sync-architecture.md`, `configuration-architecture.md` |
| `iam-identity` | `control-plane/internal/iam/**`, `shared/identity/iam/**` | `iam-identity-architecture.md` |
| `macos-ne-fail-open` | `agent/platform/darwin/NexusAgent/NexusAgentExtension/**` | `agent-ne-fail-open-architecture.md` |
| `jobs-rollup` | hub rollup and merge job defs | `metrics-rollup-architecture.md`, `jobs-architecture.md` |
| `audit-traffic-event` | `ai-gateway/internal/platform/audit/**`, hub consumer | `cost-estimation-architecture.md`, `observability-architecture.md` |
| `cache-multi-tier` | `ai-gateway/internal/cache/{core,semantic,freshness,budget,stream}/**` | `cache-multi-tier-architecture.md`, `cost-estimation-architecture.md` |
| `admin-api-openapi` | `control-plane/internal/handler/**` | any matching `docs/users/api/openapi/**` YAML |
| `cp-ui-feature` | `control-plane-ui/src/pages/**` | any matching `docs/users/features/cp-ui/**` |
| `agent-ui-feature` | `agent/ui/src/**` | any matching `docs/users/features/agent-ui/**` |

A single code change often touches multiple locked pairs. A new admin endpoint changes `control-plane/internal/handler/**` (triggers `admin-api-openapi`) and may also change `control-plane-ui/src/pages/**` (triggers `cp-ui-feature`) and `control-plane/internal/iam/**` (triggers `iam-identity`). All three doc trees must update in the same commit.

---

## Running the check

```bash
# Against origin/main (CI mode — same as what CI runs):
npm run check:doc-lockstep

# Against only staged files (fast local check):
node scripts/check-doc-lockstep.mjs --staged
```

When a check fails, the script reports which code files matched a glob, which doc files were expected but missing from the diff, and a waiver hint. The waiver hint is the exact text to include in the PR description if the change is legitimately exempt.

---

## Adding a new entry

When a new architecture doc is added, add a corresponding lockstep entry in the same PR. Edit `scripts/doc-lockstep.config.mjs`:

```javascript
{
    name: 'my-new-subsystem',
    code: [
        'packages/ai-gateway/internal/my-new-area/**',
    ],
    docs: [
        'docs/developers/architecture/services/ai-gateway/my-new-architecture.md',
    ],
    waiverHint: 'Describe what a contributor should update when this code changes.',
},
```

Adding the entry in the same PR as the architecture doc ensures the lockstep protection starts immediately.

---

## What is not a substitute

- A passing `tsc -b` or `go build` does not mean docs are aligned. Code can compile and the doc can still claim something the code no longer does.
- Touching only the `updated:` frontmatter field without changing the doc body is a red flag in review — the doc should reflect what the code now does.
- Adding a `TODO: update doc` comment in code is explicitly forbidden under the no-prod-todos rule. Update the doc now, or carve out the change.

---

## Waivers

Skipping the lockstep requires explicit user approval recorded in the PR description or commit message. Acceptable waivers:

- Trivial mechanical changes (typo fix, comment-only commit, dependency version bump that doesn't change behavior).
- Cherry-pick or revert where the original commit already covered the docs.
- Migration timestamp rename where the migration is functionally unchanged.

Not acceptable waivers:

- "Will update doc in a follow-up PR." Doc PRs that ship after the code is the bug this rule prevents.
- "It is only a small refactor." Small refactors that drift the doc mean the doc says one thing and the code does another by inches.

---

## Canonical docs

- [`scripts/doc-lockstep.config.mjs`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/doc-lockstep.config.mjs) — the authoritative mapping from code globs to required docs
- [`scripts/check-doc-lockstep.mjs`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/check-doc-lockstep.mjs) — the enforcement script
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — the "Code / doc lockstep (binding)" rule in full

**Adjacent wiki pages**: [Contributing](Contributing) · [Dev SDD Pipeline](Dev-SDD-Pipeline) · [Dev Testing Coverage](Dev-Testing-Coverage) · [Dev Code Review Checklist](Dev-Code-Review-Checklist)
