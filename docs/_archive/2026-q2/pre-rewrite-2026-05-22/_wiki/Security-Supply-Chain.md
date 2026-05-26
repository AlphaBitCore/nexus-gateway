# Security Supply Chain

*Audience: security reviewers and operators evaluating the software supply chain posture of Nexus Gateway.*

Nexus Gateway is published under the Apache 2.0 license. The key supply-chain security properties are: Ed25519 signature verification on agent auto-updates, a sibling-only `go.work` replace-directives contract that prevents silent use of stale upstream snapshots, Valkey (BSD 3-Clause) as the cache store rather than Redis (SSPL), and a ≥95% Go test coverage gate that acts as a regression net against silent breakage in security-critical paths. This page covers each of these in turn.

---

## License posture

Nexus Gateway is licensed under **Apache 2.0**. See [`LICENSE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/LICENSE).

Apache 2.0 permits commercial use, modification, distribution, and private use. It requires attribution (preservation of copyright and license notices) and does not require source disclosure for modifications. It includes an explicit patent grant and a patent retaliation clause.

Key dependencies and their licenses:

| Dependency | License | Note |
|---|---|---|
| Valkey 8 | BSD 3-Clause | Cache store; BSD keeps the stack free of SSPL / Commons Clause |
| Go standard library | BSD 3-Clause | |
| PostgreSQL driver (`pgx`) | MIT | |
| NATS JetStream | Apache 2.0 | |
| Echo (HTTP framework) | MIT | |

Valkey was chosen over Redis specifically to avoid the SSPL license that Redis 7.4+ carries. BSD 3-Clause is compatible with Apache 2.0 and imposes no restrictions on commercial or enterprise deployments.

## Agent auto-update signing

The agent autoupdater verifies both SHA-256 integrity and an **Ed25519 signature** before applying any update bundle. The signature covers the SHA-256 hash of the binary (`ed25519.Verify(pubKey, sha256(file), sig)`). A bundle whose signature does not verify is rejected and the temporary file is deleted.

The `Updater` is configured with a `Config.PublicKey` supplied by the caller at construction time. If no public key is provisioned, the updater logs a warning and disables itself — updates are rejected rather than applied without verification. There is no fallback to unsigned updates.

The download is performed over the mTLS-pinned HTTP client (`client.HTTPClient()`), pinning the connection to the enrolled device's certificate.

Crash-loop detection provides rollback: if the new binary crashes immediately after swap, the previous binary (kept at `binaryPath + ".rollback"`) is atomically restored on the next boot.

The agent binary itself is built and code-signed through the `build-agent` skill, which is the single source of truth for signing identities, provisioning profiles, notarytool keychain profile, and macOS launch-constraint requirements. The canonical signing flow ensures that the binary distributed to endpoints carries Apple Developer ID signatures and passes macOS Gatekeeper and Notarization checks.

## go.work sibling-only replace directives

The monorepo uses a `go.work` workspace. The `replace` directives in `go.work` and individual `go.mod` files must reference sibling modules only — modules inside this repository. Breaking this invariant causes `GOWORK=off` builds (e.g. CI builds on some container images, or a developer running with the workspace disabled) to silently pull stale snapshots from GitHub instead of the local module, producing builds from a different version of shared code than what the workspace targets.

This is a binding convention enforced in [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) "Go — replace directives sibling-only contract". Any `replace` directive pointing outside the repository requires explicit justification.

## Test coverage gate

Every Go package in `packages/**` must maintain ≥95% statement coverage under `go test -cover -count=1 ./...`, or be listed in `scripts/.coverage-allowlist` with an allowed category and rationale. The coverage gate is enforced by `scripts/check-go-coverage.sh` as a pre-commit hook on staged Go packages.

For supply-chain purposes, the coverage gate acts as a regression net: a change to a security-critical path (credential decryption, VK auth, hook pipeline, kill-switch propagation) that removes code coverage will fail the pre-commit check, surfacing the gap before it reaches main.

Tests must assert observable business behavior and named failure modes — not just call functions to bump a percentage.

## Dependency management

All Go module dependencies are pinned by the `go.sum` file. The `go.work.sum` covers the workspace-level combined dependency set. Direct dependency updates require a deliberate `go get` invocation and a corresponding `go.sum` update, both visible in code review.

The compliance proxy and agent use no dynamic plugin loading. All code paths are compiled into the binary at build time.

## Build reproducibility

The macOS agent binary is built through the `build-agent` skill — the single source of truth for signing identities, provisioning profiles, and notarytool keychain configuration. Building the agent outside this skill is prohibited; improvising around it has caused Network Extension signing failures. The skill encodes macOS 26 launch-constraint requirements, so the resulting binary satisfies the OS security policy that governs system extensions.

For the four server-side Go services (Hub, Control Plane, AI Gateway, Compliance Proxy), builds are deterministic for a given Go toolchain version and `go.sum` state. The server services are compiled with the workspace (`go.work`) active, so `packages/shared/` is always built from the in-tree source rather than a cached module download.

## Reviewer checklist for supply-chain changes

When a PR adds or upgrades a Go dependency:

1. Verify the dependency license is compatible with Apache 2.0. BSD 2/3-Clause, MIT, and Apache 2.0 are all acceptable. SSPL, Commons Clause, and AGPL require escalation.
2. Check that the `go.sum` update is included (missing `go.sum` update = build would fail on a clean checkout).
3. Confirm that no `replace` directive in `go.work` or any `go.mod` points outside the repository.

When a PR modifies the agent build or signing configuration, the `build-agent` skill must be invoked and the resulting binary must pass Gatekeeper and Notarization before the PR is merged.

---

## Canonical docs

- [`LICENSE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/LICENSE) — Apache 2.0 license text
- [`docs/developers/architecture/services/agent/agent-autoupdater-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-autoupdater-architecture.md) — autoupdater Config, Ed25519 verify, atomic swap, crash-loop rollback
- [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) — Go `go.work` sibling-only replace directive binding

**Adjacent wiki pages**: [Security Threat Model](Security-Threat-Model) · [Agent Auto Update](Agent-Auto-Update) · [Security Compliance Posture](Security-Compliance-Posture) · [Dev Testing Coverage](Dev-Testing-Coverage)
