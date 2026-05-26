# Prerequisites

Nexus Gateway requires four tools before the bootstrap script can run. Missing any one of them causes the script to fail immediately with a clear error; the table below lists the exact minimum versions and where to get them. Most macOS Apple Silicon machines have all four after a standard developer setup; Linux users on x86_64 and arm64 are also fully supported. Windows is supported via WSL2.

---

## Required tools

| Tool | Minimum version | Why |
|---|---|---|
| **Node.js** | 20+ | npm workspaces require npm 10+, which ships with Node 20; the Control Plane UI dev server and the seed script both require it |
| **Go** | 1.25+ | All five Go services share a `go.work` workspace at the repo root; the workspace syntax and toolchain directives used here require 1.25+ |
| **Docker** | any recent (with Compose v2) | Hosts PostgreSQL 16, Valkey 8, and NATS JetStream via `docker-compose.yml`; `docker compose` (not `docker-compose`) is required |
| **OpenSSL** | any CLI version | Used by the bootstrap script to generate a random `CREDENTIAL_ENCRYPTION_KEY`; falls back to a fixed dev key if missing but warns |

## Installing the tools

Install Node.js from [nodejs.org](https://nodejs.org) (LTS is fine). Install Go from [go.dev/dl](https://go.dev/dl) — pick the latest 1.25.x release. Install Docker Desktop from [docker.com](https://www.docker.com); Docker Desktop ships Compose v2 by default. OpenSSL ships pre-installed on most Linux distributions and macOS; on WSL2 install via `sudo apt install openssl`.

## Verifying the tools

Run these commands to confirm the tools are present and meet the minimum versions:

```bash
node --version       # should print v20.x.x or higher
npm --version        # should print 10.x.x or higher
go version           # should print go1.25 or higher
docker compose version   # should print v2.x.x (note: "compose", not "compose-plugin")
openssl version      # any version is fine
```

If Docker Desktop is running but `docker compose version` fails, ensure Compose v2 is enabled. In Docker Desktop: **Settings → General → Use Docker Compose V2** must be checked.

## Go workspace requirement

All five Go services share a `go.work` workspace at the repo root. Every module under `packages/` references its sibling packages via pseudo-version `require` lines that are only syntactically required — the workspace file provides real resolution. The critical consequence: if `go.work` is absent from the build context (e.g. `GOWORK=off` is set, or only a subdirectory was cloned), Go falls back to downloading the repo's own modules from GitHub instead of using the local source tree. The build succeeds against an old remote snapshot, silently masking local changes. The fix is always: ensure `go.work` is present and `GOWORK` is unset.

Verify the workspace is active:

```bash
go env GOWORK   # should print the absolute path to go.work, not empty string
```

## Docker Compose v2 requirement

The bootstrap script uses `docker compose` (subcommand form, Compose v2). The legacy `docker-compose` (hyphenated, v1) is no longer supported by Docker and is not guaranteed to be present. Verify with:

```bash
docker compose version   # expect Docker Compose version v2.x.x
```

If `docker-compose` is installed but `docker compose` is not, Docker Desktop is likely running an old version. Upgrade to any Docker Desktop released after 2022.

## OS support notes

**macOS Apple Silicon (arm64)** is the primary development platform. All services build and run natively on arm64; Docker Desktop for Apple Silicon supports multi-arch images so the PostgreSQL, Valkey, and NATS containers work without Rosetta. The macOS Desktop Agent uses `NETransparentProxyProvider` (Network Extension API) which requires macOS 13 Ventura or later — this applies only to the agent, not to the server-side services or the UI.

**Linux x86_64 and arm64** are fully supported. Use the native package manager to install Go (or download the tarball from go.dev), then install Docker Engine + Compose v2. The bootstrap script is written in Bash and relies only on POSIX tools that are standard on any distribution. The Linux Desktop Agent uses systemd and iptables-based capture (experimental).

**Windows via WSL2** is supported. Install Ubuntu in WSL2, then install all four tools inside the WSL2 environment. Run all commands from the WSL2 terminal. Docker Desktop for Windows with the WSL2 backend enabled is the recommended Docker setup; the `docker` CLI inside WSL2 calls the Windows Docker Desktop daemon automatically. A native Windows Desktop Agent is on the roadmap but not yet available.

## What the bootstrap script needs

When the prerequisites are satisfied, `./scripts/dev-start.sh` performs the complete local bring-up: it starts the Docker infrastructure (PostgreSQL, Valkey, NATS), installs npm dependencies, runs Prisma schema push and seed, generates the Compliance Proxy dev CA (requires OpenSSL), and prints the per-service start commands. The full script walkthrough is in [Quickstart](Quickstart). If any prerequisite check fails the script exits immediately with an actionable error message naming the missing tool and linking to where to install it.

The script creates the repo-root `.env` from `.env.example` on first run. All Go services read this file automatically at boot via `packages/shared/core/bootenv` — no `source .env` command is needed in the terminal. The file contains dev-safe default values for every secret placeholder; it must never be committed (it is gitignored).

## Port availability

Before running the bootstrap, confirm that the following ports are free on localhost:

| Port | Service |
|---|---|
| `:3000` | Control Plane UI |
| `:3001` | Control Plane |
| `:3040` | Compliance Proxy runtime API |
| `:3050` | AI Gateway |
| `:3060` | Nexus Hub |
| `:55532` | PostgreSQL (Docker) |
| `:6437` | Valkey (Docker) |
| `:4222` | NATS (Docker) |

Check with `lsof -nP -iTCP:<port> -sTCP:LISTEN`. If a port is taken, find the process with `lsof -nP -iTCP:<port> -sTCP:LISTEN | awk 'NR>1 {print $2}'` and stop it. The most common conflict on developer machines is a local Postgres instance on `:5432` — note Nexus uses `:55532` for Postgres to avoid exactly this collision.

## What gets installed by the bootstrap

`./scripts/dev-start.sh` does not install any system-level dependencies — that is the developer's responsibility. What the script does install, in isolated locations that do not affect the rest of the machine:

- **npm workspace packages** — installed into `node_modules/` at the repo root via `npm install`. This includes the Control Plane UI dependencies, the db-migrate tooling (Prisma CLI, tsx, dotenv), and the shared `ui-shared` package.
- **Prisma client artifacts** — generated into `tools/db-migrate/node_modules/.prisma/` as part of `npx prisma db push`. These are local to the tools directory.
- **Dev CA key pair** — `packages/compliance-proxy/dev-certs/ca.crt` and `ca.key`. These are gitignored and generated on the local machine only. They are not placed in any system trust store — the dev CA is only trusted by explicit `--cacert` flags in curl or `SSL_CERT_FILE` in Python.

Nothing is installed into `/usr/local/`, `/usr/lib/`, or any system directory. The repo is self-contained.

## Checking that Docker Compose has Valkey support

The `docker-compose.yml` uses a service named `valkey` with the `valkey/valkey-bundle:8-trixie` image (BSD-licensed, Redis-wire-compatible, includes the `valkey-search` module for the semantic cache roadmap item). Older Docker Compose installations that only know about the legacy `nexus-redis` service from before the Valkey migration (E61, 2026-05-20) will not start the cache layer correctly. If `docker ps` shows a container named `nexus-redis` that is still running from a prior clone, stop and remove it before running the bootstrap:

```bash
docker ps --filter "name=nexus-redis"
docker stop nexus-redis && docker rm nexus-redis
```

Then re-run `./scripts/dev-start.sh`.

---

## Canonical docs

- [`local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — service lifecycle, log paths, and the admin API helper contract
- [`dev-start.sh`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/dev-start.sh) — the bootstrap script that checks these prerequisites

**Adjacent wiki pages**: [Quickstart](Quickstart) · [Troubleshooting First Run](Troubleshooting-First-Run) · [First Admin Login](First-Admin-Login)
