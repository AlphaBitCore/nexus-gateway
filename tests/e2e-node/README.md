# tests/e2e-node — OpenAI Node SDK compatibility (AP-3)

The Node half of the AP-3 acceptance criterion: an **unmodified** `openai` Node
SDK must reach the Nexus AI Gateway with only `baseURL` and `apiKey` changed.

This suite is the **mirror** of `tests/e2e-python/sdk_compat/`. It covers each
scenario category once — chat, streaming, tools, streamed tool arguments,
structured output, vision, reasoning, embeddings, errors. The exhaustive
per-case matrix (60 cases) lives on the Python side; a second full copy in
another language would double the maintenance for the same evidence.

## Setup

```bash
npm install
```

## Run

Point it at any deployment whose provider credentials work — local included:

```bash
NEXUS_TEST_TARGET=local npx vitest run
```

Every completion answers `502 PROVIDER_UNAVAILABLE` when the target's provider
credentials are missing, expired, or encrypted under a key that deployment no
longer holds. Check with the Control Plane's credential probe
(`POST /api/admin/credentials/{id}/probe`) before assuming a compat failure — a
502 `PROVIDER_UNAVAILABLE` is an environment problem, never a compat one. For a
shared staging target, copy `tests/.env.stg.example` to `tests/.env.stg`, set
`NEXUS_TEST_VK`, and use `NEXUS_TEST_TARGET=stg`.

Through the unified runner:

```bash
bash tests/run-all.sh --full --phase sdk-compat-node --target stg
```

## Notes

- **SDK version.** AP-3's text says "Node.js SDK v4.x", but openai-node's
  current major is 6 (`openai@^6.49.0` here). The suite tests what callers
  actually install today; the tested version is recorded in
  `docs/users/api/openai-sdk-compatibility.md`.
- **Env loading.** `lib/loadenv.mjs` mirrors `tests/lib/loadenv.py` and
  `tests/lib/loadenv.sh` — same file layering, same non-overload semantics, same
  loopback guard for `target=local`. Change all three together.
- **Model selection** is capability-gated via `GET /v1/models` (`pickModel` in
  `specs/helpers.mjs`) rather than pinned to model ids, which rot on every
  catalog reseed. A missing capability skips the spec; a missing embedding model
  fails, because embeddings is a named acceptance criterion.
- **Serial by design.** `fileParallelism: false` — the specs share one virtual
  key against live upstreams, and parallel files turn a provider rate limit into
  a spurious failure.
