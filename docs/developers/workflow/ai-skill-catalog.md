# AI skill catalog

Skills are the on-demand procedures the AI agent fires as `/skill-name`. Each one
is a self-contained runbook: trigger keywords, preconditions, ordered steps, and
a verification gate. They live under `.claude/skills/`, which is the source of
truth for what exists — this catalog describes what each does and how they group.

Skills are the on-demand counterpart to the always-on `.cursor/rules/*.mdc`
bindings. A rule is loaded into every editor session and enforces an invariant;
a skill is invoked deliberately to walk a procedure. Many pair: `add-provider-adapter`
with `provider-adapter-canonical-openai.mdc`, `iam-impact-review` with
`iam-impact-review.mdc`, `ne-fail-open-audit` with `ne-fail-open.mdc`.

## Authoring procedures

Walk a multi-step change end-to-end.

- **`spec-writing`** — write the requirement spec before implementation, the
  spec-driven-development entry point.
- **`doc-write`** — write or rewrite a doc against the code-anchored protocol;
  pairs with `doc-review`.
- **`add-cp-ui-section`** — add a Control Plane UI section, item, or route,
  threading IAM, i18n, design tokens, the `useApi` query-key shape, the sidebar
  mapping, tests, and the feature doc — the workflow that touches the most binding
  rules at once.
- **`add-provider-adapter`** — onboard a new provider adapter (vendor, wire
  format, or consumer surface) under the provider-adapter architecture rules.
- **`add-shadow-key`** — add or change a Thing shadow key, auditing the three
  separate paths that create or modify `thing_config_template` rows.

## Audit and review gates

Check a surface against its binding contract before merge.

- **`doc-review`** — audit a doc claim-by-claim against the code; the pre-commit
  gate for `doc-write`.
- **`adapter-conformance-check`** — audit AI Gateway adapter codecs against the
  provider-adapter rules, catching per-adapter logic that leaked into the generic
  dispatcher, un-canonicalized ingress, bypassed error helpers, prefix-lists with
  no evidence, and missing wiring.
- **`wire-capability-probe`** — establish what a provider wire can actually carry
  before a refusal is written into a catalog or a codec. Three probes separate
  "the wire cannot" from "we send the wrong form" from "the model cannot"; a 422
  that read as a capability limit turned out to be an unused mapping. Pairs with
  `adapter-conformance-check`: this one decides what the translation should be,
  that one checks the translation obeys the rules.
- **`arch-doc-trigger-check`** — verify the architecture-doc trigger-map lockstep:
  every `*-architecture.md` has a row and every row points at a real doc.
- **`frontend-arch-review`** — audit the Control Plane UI and Agent Dashboard
  against the design-token / CSS-framework architecture, catching hex/rgba
  literals, raw numerics in inline styles, and stale token fallbacks.
- **`gap-review`** — review gaps between the spec documents and the architecture,
  requirements, OpenAPI specs, code, and tests.
- **`i18n-gap-check`** — scan frontend i18n keys across both bundles and report
  missing keys and hardcoded English strings.
- **`iam-impact-review`** — the five-step IAM impact audit when an admin endpoint,
  sidebar item, or route is added, moved, or renamed, catching drift between UI
  `allowedActions` and the handler's `iamMW` guard.
- **`ne-fail-open-audit`** — the safety-critical audit before merging any change
  to the macOS Network Extension transparent proxy, which sits in the host's
  outbound packet path and must fail open.
- **`oss-secret-scan`** — a deterministic scan for leaked secrets, PII, and
  production infrastructure before an open-source release; the agent only triages
  the script's candidates.
- **`pre-edit-reader`** — walk the three-doc pre-edit reading requirement
  (architecture, feature, conventions).
- **`project-review`** — a multi-role full-system review built from specialist-role
  prompts.

## Testing and smoke

Exercise running services and cross-check the results.

- **`smoke-gateway`** — full-surface AI Gateway smoke: every catalog model across
  non-stream, SSE, and a two-turn cache check; auto-manages routing rules,
  cross-checks `traffic_event` rows, diffs Prometheus counters, and auto-fixes.
- **`test-all`** — the full end-to-end program (preflight, smoke, Go integration,
  protocol, AI-judge, and Playwright UI layers) via `tests/run-all.sh`; the single
  "did my change break something" entry point.
- **`test-compliance-proxy`** — smoke the Compliance Proxy's HTTPS MITM interception,
  pipeline, and `traffic_event` rows.
- **`test-cursor-adapter`** — a synthetic test for the Cursor protobuf normalizer,
  verifying the normalized traffic-event shape.
- **`test-geminiweb-adapter`** — a synthetic test for the Gemini Web `batchexecute`
  normalizer.
- **`test-openai-responses`** — a synthetic test for the OpenAI Responses-API
  ingress on the local gateway.

## Local, build, and deploy

- **`run-local`** — bring up the full local stack (PostgreSQL, Valkey, and NATS via
  docker-compose, the four Go services, and the Vite UI) from a clean clone.
- **`build-agent`** — build, sign, notarize, and package the macOS NexusAgent; the
  single source of truth for the signing and packaging sequence.
- **`prod-deploy`** — deploy all services to the production instance (tag, build,
  upload, swap binaries, ordered restart, verify nodes, smoke), and apply a specific
  DB migration when needed.

## Production ops and debug

- **`prod-login`** — log into the production Control Plane via OAuth + PKCE, cache
  the bearer token, and expose the `cp_login` / `cp_curl` helper family for
  subsequent admin API calls.
- **`prod-debug`** — diagnose production issues across service logs, DB queries,
  Redis, NATS, config/shadow state, metrics, and known failure patterns.
- **`frontend-bug-trace`** — trace a frontend bug full-stack (page → component →
  API → handler → DB) and verify the fix with a real login and DB queries.

## Skills coupled to production infrastructure

A fork cannot run some skills unchanged:

- `prod-login`, `prod-deploy`, and `prod-debug` target this repo's production
  deployment; their configuration comes from `tests/.env.prod`. A fork repoints
  them by supplying its own `tests/.env.<target>` (the loader contract is in
  [local-dev-debugging.md](local-dev-debugging.md)).
- `build-agent` depends on Apple signing and notarization credentials.
- `test-cursor-adapter` and `test-geminiweb-adapter` default to a production
  compliance proxy but accept a user-supplied proxy endpoint.

The swap point is the env file, not the skill body — keeping production hostnames
and credentials out of the repo is exactly what `oss-secret-scan` enforces.

## Writing a new skill versus extending one

Write a **new** skill for a distinct, repeatable procedure with its own trigger
and verification gate that no existing skill covers. **Extend** an existing skill
for a new case of a procedure it already owns — a new adapter test arm belongs in
`smoke-gateway`, a new audit rule in the matching `*-audit` or `*-check` skill.
Every skill keeps the same shape: trigger keywords, preconditions, ordered steps,
and a verification gate.

## References

- `.claude/skills/` — the skill directory; the source of truth for what exists
- `.claude/skills/README.md` — the directory's own entry pointer
- `.cursor/rules/` — the always-on IDE bindings skills pair with
- [ai-workflow.md](ai-workflow.md) — the development workflow these skills slot into
