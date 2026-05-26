# License

Nexus Gateway is released under the Apache License, Version 2.0. This is a permissive OSS license that allows commercial use, modification, and distribution with minimal restrictions. The full license text is in [`LICENSE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/LICENSE) at the repository root. The `NOTICE` file records the copyright holder and third-party attributions required by the license.

---

## What Apache-2.0 permits

| Activity | Permitted |
|---|---|
| Use the software commercially | ✅ — no royalties, no restrictions on commercial deployment |
| Modify the source code | ✅ — fork, adapt, build on top of it |
| Distribute copies or derivative works | ✅ — in source or binary form |
| Grant sub-licenses | ✅ — under the same license or compatible terms |
| Use contributor patents | ✅ — each contributor grants a royalty-free patent license covering their contributions |
| Use privately (no distribution) | ✅ — no copyleft obligation when you don't distribute |

## What Apache-2.0 requires

When distributing Nexus Gateway or a derivative work:

1. **Include a copy of the license** — attach the `LICENSE` file (or its text) to any distribution.
2. **State changes made** — modified files must carry a notice that they were changed.
3. **Preserve attribution notices** — retain all copyright, patent, trademark, and attribution notices from the source.
4. **Reproduce the `NOTICE` file** — if the original work includes a `NOTICE` file (Nexus Gateway does), derivative works must reproduce its content in at least one of: a NOTICE file, the source/documentation, or a UI display where such notices normally appear. The [`NOTICE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/NOTICE) file records the copyright holder (AlphaBit Core) and attribution for bundled third-party dependencies.

The `NOTICE` file is informational only — its contents do not modify the license terms.

## What Apache-2.0 does not cover

- **Warranty** — the software is provided "AS IS", without warranty of any kind. Operators assume all risk of deployment.
- **Liability** — contributors are not liable for damages arising from use or inability to use the software.
- **Trademarks** — the license does not grant permission to use trade names, trademarks, or service marks of AlphaBit Core, except as needed to describe the origin of the work.
- **Third-party components** — each bundled dependency carries its own license. The `NOTICE` file lists the primary runtime dependencies; the full graph is in `go.work.sum` and `packages/*/package.json`.

## Third-party dependencies

Nexus Gateway bundles or links against a number of open-source libraries. Notable license categories:

| License | Example dependencies |
|---|---|
| Apache-2.0 | `nats-io/nats.go`, `prometheus/client_golang`, `aws/aws-sdk-go-v2`, `go.opentelemetry.io/otel*`, TypeScript, Prisma |
| MIT | `labstack/echo/v4`, `jackc/pgx/v5`, `golang-jwt/jwt/v5`, `tidwall/gjson`, React, Vite, `@tanstack/react-query` |
| BSD-2-Clause | `redis/go-redis/v9`, `bits-and-blooms/bloom/v3` |
| ISC | `coder/websocket` |
| MPL-2.0 | `hashicorp/golang-lru/v2` |

The Valkey 8 cache image (`valkey/valkey-bundle:8-trixie`) is BSD-licensed. This was a deliberate choice to keep the project's OSS license posture clean — see [`Security-Supply-Chain`](Security-Supply-Chain) for the full supply-chain review.

For the complete dependency list, see the [`NOTICE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/NOTICE) file and the `go.work.sum` / `packages/*/package.json` files in the repository.

## Copyright

Copyright 2026 AlphaBit Core (https://alphabitcore.com/)

---

## Canonical docs

- [`LICENSE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/LICENSE) — full Apache License 2.0 text
- [`NOTICE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/NOTICE) — copyright notice and third-party attribution

**Adjacent wiki pages**: [Security Supply Chain](Security-Supply-Chain) · [Community Governance](Community-Governance) · [Community Maintainers](Community-Maintainers) · [What Is Nexus Gateway](What-Is-Nexus-Gateway)
