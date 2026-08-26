# Container build definitions

Every Nexus Gateway image is defined here. CI does not contain build logic; it
invokes `scripts/release/*.sh`, so anything CI produces can be reproduced
locally (arm64 excepted — it needs arm64 hardware).

| Directory | Image | Notes |
|---|---|---|
| `buildbase/` | `nexus-buildbase` | Go toolchain + Vectorscan (libhs), `FAT_RUNTIME=OFF` |
| `services/` | `nexus-hub`, `control-plane`, `ai-gateway`, `compliance-proxy` | One builder stage, four distroless targets |
| `control-plane-ui/` | `control-plane-ui` | Vite build served by nginx |
| `db-migrator/` | `db-migrator` | Prisma schema push + seed, one-shot |

## Why a separate buildbase

The four Go services share one `go.work` module graph and one libhs install.
Building them from four independent Dockerfiles would compile libhs four times
per architecture. `buildbase` compiles it once; it is rebuilt only when the
Vectorscan pin or the Go version changes.

## Architecture variants

`FAT_RUNTIME=OFF` fixes the SIMD instruction set at compile time, so the amd64
images come in two flavours: a `x86-64-v2` baseline that runs anywhere, and an
`-avx2` variant. arm64 has no variant — every arm64 v8-A CPU has NEON, so the
`armv8-a` baseline runs everywhere.

## Build, run, verify — the whole loop

One command builds every image, one runs the stack from them, one proves it
works. All of them are run from the repository root.

### 1. Build

```bash
# Every image, for THIS host's architecture. Compiles Vectorscan, gates it,
# builds the four services against it, then the UI and the migrator — both of
# which take the repository root as their build context because they need files
# outside their own directory (the UI needs the packages/ui-shared workspace,
# the migrator the whole tools/db-migrate/ tree). First run takes ~10 minutes
# (the libhs compile); later runs reuse the cached buildbase layer.
scripts/release/build-images.sh --config baseline --version dev
```

This is the same script CI runs, with `--push --registry ghcr.io/alphabitcore`
added — there is no second definition of how an image is built.

It is not just a compile. It runs both release gates, and either one failing
stops the build:

- **the ISA gate** (`scripts/release/verify-image.sh`) disassembles `libhs.a`
  inside the buildbase and counts AVX2 (amd64) or SVE (arm64) instructions. A
  baseline image containing them would `SIGILL` on the CPUs the tag promises to
  run on;
- **the firing self-test**, inside the builder stage, runs three named tests
  against the linked engine. A `FAT_RUNTIME=ON` archive links cleanly and then
  returns zero matches forever, so "it compiled" is not evidence that content
  scanning works.

`--config avx2` builds the amd64-only variant and refuses to run on arm64. It
builds the four Go services only: neither Node image links libhs, so neither
has an instruction-set variant to build.

### 2. Run what you just built

```bash
cd deploy
./init-secrets.sh                        # generates .env: secrets + admin password
printf 'NEXUS_REGISTRY=local\nNEXUS_VERSION=dev\n' >> .env
docker compose up -d                     # add --profile compliance for the MITM proxy
```

Then open the UI on `http://localhost:8080`.

#### The console credentials

The **username is always `admin@nexus.ai`** — the seed fixes the administrator
identity, so it is not configurable.

The **password is generated per install**. `init-secrets.sh` prints it once,
when it creates `.env`:

```
  Admin sign-in: admin@nexus.ai
  Password:      <generated>
```

That is the only time it is printed, and it is several hundred lines of
`docker compose up` output before you get to a login form. It is also written
to `deploy/.env` (mode `600`, owner-only), so read it back from there whenever
you need it:

```bash
grep NEXUS_ADMIN_PASSWORD deploy/.env
```

To choose the password yourself instead, set `ADMIN_PASSWORD` on the run that
creates `.env` — before the stack's first `up`:

```bash
ADMIN_PASSWORD=<your-password> ./init-secrets.sh
```

Two things to know about that: `init-secrets.sh` never overwrites an existing
`.env`, so choosing a password after the fact means deleting `.env` and
regenerating every secret in it; and the migrator prints a warning on every
`up` if the value you chose is the seed's own published default, which is fine
for a local quickstart and must never reach an exposed deployment.

Every `docker compose up` also ends with a reminder of where to look, in
`docker compose logs db-migrator`:

```
==> [migrator] console sign-in: admin@nexus.ai
    password: the NEXUS_ADMIN_PASSWORD line in deploy/.env on the host
```

The password itself is deliberately not in that log line — container logs get
collected, and a super-admin password does not belong in a log aggregator.

If something else on your machine already owns 8080, 3050 or 3128, set
`NEXUS_UI_PORT` / `NEXUS_GATEWAY_PORT` / `NEXUS_PROXY_PORT` in `.env` instead of
editing `ports:` — the console's origin and the OAuth callback the migrator
registers follow those variables, and editing the port alone leaves a stack
nobody can log into.

### 3. Verify

```bash
scripts/release/smoke-compose.sh --registry local --version dev
```

This is the release gate, and it is the answer to "is this branch OK?". It
brings the stack up from those images, asserts around thirty things — the
seeded credentials really were rotated and the rotated ones really work, an
operator's own rows survive a second migrator run, the dependency volume is
installed once and reused, the audit spool is writable, the compliance profile
starts and answers — and then tears everything down, volumes included. It picks
free host ports if the defaults are taken, so it will not fight anything else
you have running.

## Tags, and which one the compose file can use

`build-images.sh` names its own output `local/<service>:dev-<config>-<arch>` —
that is the tag the release publishes per architecture. Because the compose file
interpolates a single `NEXUS_VERSION` into all six image references and cannot
resolve a per-architecture one, the script also applies, whenever it is not
pushing, `local/<service>:dev` for the baseline configuration and
`local/<service>:dev-avx2` for the AVX2 one. The two configurations therefore
never claim the same name, and `NEXUS_VERSION=dev` always means the portable
build.

The AVX2 tag covers only the four Go services — the UI and migrator have no AVX2
variant to build — so `NEXUS_VERSION=dev-avx2` cannot resolve a whole stack
locally without tagging those two yourself (`docker tag local/db-migrator:dev
local/db-migrator:dev-avx2`, likewise for `control-plane-ui`). The published
registries have the same shape for the same reason, except that there the
release aliases the two Node images so a `-avx2` value resolves; see the
architecture doc's tag contract.

Full design: `docs/developers/architecture/cross-cutting/deployment/container-image-architecture.md`
