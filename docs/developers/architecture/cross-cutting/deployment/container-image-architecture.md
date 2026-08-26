---
updated: 2026-07-25
---

# Container image architecture

Container and release form factor for Nexus Gateway: multi-architecture
images published to GHCR and Docker Hub, a `docker compose` quickstart built
from those images, and self-contained Linux tarballs for operators who deploy
with systemd rather than containers.

This doc is the architecture source of truth for **everything under
`docker/**`, `deploy/**`, and `scripts/release/**`, plus
`.github/workflows/release.yml` and `.github/workflows/buildbase.yml`**. Any
change to a Dockerfile, the compose file, a release script, or these
workflows MUST update this doc (or `docs/operators/ops/container-deployment.md`)
in the same commit (Code/Doc Lockstep — see `.cursor/rules/code-doc-lockstep.mdc`).

This is a separate form factor from the AMI / bare-metal appliance
(`docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md`),
which packages the same four Go services plus Postgres/Valkey/NATS into one
disk image compiled on the machine class it runs on. The container form
factor pulls pre-built multi-architecture images instead, which is what
drives most of the design below.

## 1. Image inventory

Seven images are built; six are published. `nexus-buildbase` is a build
input that is also published to GHCR (`nexus-buildbase:vs5.4.12-amd64`,
`-amd64-avx2`, `-arm64` — see §7 for why these are three separate per-arch
tags, not a composed multi-arch manifest), but nothing in this repository
currently pulls it: `scripts/release/build-images.sh` and
`scripts/release/build-tarball.sh` both `docker build` a buildbase image
locally, keyed by the release version being built
(`nexus-buildbase:<version>-<config>-<arch>`), and never reference the
published `vs5.4.12*` tags. Publishing it is a convenience artifact for a
maintainer who wants the exact pinned Vectorscan build without waiting
through the ~10-minute compile — `docker pull
ghcr.io/alphabitcore/nexus-buildbase:vs5.4.12-amd64` and building
`docker/services/Dockerfile` with `--build-arg BUILDBASE=<that tag>` works
today — but it is not on the critical path of any script or workflow in
this repository, and no service image is currently "reproduced off-CI"
from it as a documented workflow.

| Image | Directory | Base | Notes |
|---|---|---|---|
| `nexus-buildbase` | `docker/buildbase/` | `debian:bookworm` | Go toolchain + Vectorscan (`libhs`), `FAT_RUNTIME=OFF`, installed to `/usr/local` |
| `nexus-hub` | `docker/services/` | `gcr.io/distroless/cc-debian12:nonroot` | |
| `control-plane` | `docker/services/` | same | |
| `ai-gateway` | `docker/services/` | same | |
| `compliance-proxy` | `docker/services/` | same | |
| `control-plane-ui` | `docker/control-plane-ui/` | builder `node:22-bookworm-slim`, runtime `nginx:1.27-alpine` | Vite build served by nginx |
| `db-migrator` | `docker/db-migrator/` | `node:22-bookworm-slim` | one-shot: Prisma schema push + `schema-extras.sql` + seed + credential rotation (bootstrap admin/assistant-key pair, and the demo tier's own 13 `NexusUser` / 12 `VirtualKey` / 5 `AdminApiKey` rows). Ships **thin** — see §8.1 |

`distroless/cc-debian12` carries glibc and `libstdc++`, which the cgo link
(`-lstdc++`) into `libhs` requires, runs as `nonroot`, and ships no shell —
the smallest base that can still host a cgo binary linked against a C++
static archive. Alpine/musl was not used: musl's C++/boost toolchain is a
known-fragile combination for a project the size of Vectorscan, and
`packages/agent/Dockerfile` already established debian/glibc as this
project's container runtime family.

`control-plane-ui` and `db-migrator` do not link `libhs`, so neither goes
through the buildbase — they build per-architecture directly from their own
Dockerfile against a plain Node base.

## 2. Why one builder stage compiles all four Go services

`nexus-hub`, `control-plane`, `ai-gateway`, and `compliance-proxy` share one
`go.work` module graph and one `libhs` install. Building them from four
independent Dockerfiles would compile and link Vectorscan four times per
architecture. `docker/services/Dockerfile` instead has a single builder
stage (`FROM ${BUILDBASE} AS builder`) that compiles all four binaries in one
loop, then four named runtime stages (`nexus-hub`, `control-plane`,
`ai-gateway`, `compliance-proxy`) each `COPY --from=builder` a single binary
into its own `distroless/cc-debian12:nonroot` target. `docker build --target
<svc>` selects which one to tag.

The Vectorscan firing self-test (§3) also runs inside this builder stage,
not in any of the four runtime images — the distroless runtime has neither a
Go toolchain nor the source tree to run `go test` against, so the proof that
the linked engine scans has to happen where the compiler ran.

The four per-package Dockerfiles that used to live at
`packages/{nexus-hub,control-plane,ai-gateway,compliance-proxy}/Dockerfile`
were deleted rather than kept as a parallel path: they were dev-grade
(Alpine base, no `-tags vectorscan`, no version stamp), nothing in the
repository referenced them, and `docker/services/Dockerfile` supersedes them
entirely.

## 3. The Vectorscan constraint: `FAT_RUNTIME=OFF`

The four Go services link `libhs` (Vectorscan) via cgo for content scanning
(PII detection, rule-pack matching). `libhs` is C++ built from source, so it
cannot be cross-compiled — `docker/buildbase/Dockerfile` builds it natively,
once per architecture.

**`FAT_RUNTIME` MUST stay `OFF`.** A `FAT_RUNTIME=ON` static archive linked
into a Go cgo binary resolves its CPU-dispatch IFUNCs to a no-op stub:
`hs_scan` returns zero matches with no error. Content scanning is silently
disabled and PII passes through unredacted — there is no crash, no log line,
nothing an operator would notice short of a compliance audit. The
`FAT_RUNTIME=OFF` archive is measurably smaller (11,331,324 bytes measured on
arm64 at the current pin, against roughly 19 MB for `FAT_RUNTIME=ON`), which is
the empirical tell if this regresses. Nothing
asserts that size; what is asserted is that `EXTRA_CMAKE_FLAGS` can only be
empty or exactly `-DBUILD_AVX2=ON`, checked inside the buildbase itself, since
that argument is expanded last on the cmake line and a `-DFAT_RUNTIME=ON`
smuggled through it would otherwise win over the flag set above it.

Because linking succeeds either way, a clean `docker build` is not evidence
the engine works. `docker/services/Dockerfile`'s builder stage therefore runs
a **firing self-test** right after compiling the four binaries:

```
go test -tags vectorscan -count=1 -v \
  -run '^(TestHSSelfTest|TestVectorscan_ScanUnderCgoLimit|TestVectorscan_ScanComplete_ReportsCompletion)$' \
  ./packages/shared/policy/hooks/matcher/
```

A failure aborts the build with an explicit message that the linked engine
does not scan and the binaries would pass PII through unredacted — the build
must not continue past that point.

Two properties of that command are load-bearing. The pattern is **anchored and
names each test exactly**, and the number of `--- PASS:` lines is **asserted to
be three**. `-run` is an unanchored regexp and `go test` exits 0 when nothing
matches, so a loose pattern is a gate that can quietly select nothing — or the
wrong thing: the earlier `'TestVectorscan_Scan'` named no function at all and
matched three tests by substring, one of which asserts that a scan after `Close`
returns no hits and therefore passes on a completely dead engine, while
`TestHSSelfTest` — written for exactly this purpose, failing unless `/secret/`
matches once — was never selected. `scripts/release/build-tarball.sh` runs the
same list against the same link recipe for the tarball binaries.

## 4. Instruction-set baseline

`FAT_RUNTIME=OFF` has a consequence: the SIMD instruction set `libhs` uses is
fixed at **compile time**, not detected at runtime. For the AMI appliance
this is harmless because the build instance and the run instance are the
same machine class (§8). For a published image it is not — the image is
compiled once and pulled onto arbitrary hardware, so an over-aggressive
instruction set produces a `SIGILL` on older CPUs at the first scan.

**Finding: `USE_CPU_NATIVE` already defaults to `OFF` in Vectorscan
`5.4.12`.** With it off, `cmake/archdetect.cmake` takes its `else()` branch
and derives the target architecture from fixed per-architecture variables
rather than from the build host's CPU — so a non-fat build is already
conservative by upstream default, before this project changes anything.
`docker/buildbase/Dockerfile` passes `-DUSE_CPU_NATIVE=OFF` explicitly anyway,
so a future release that starts defaulting it ON cannot silently retarget these
images at the build runner's own CPU — `option()` never overwrites a cache
entry that is already set. What is **not** pinned, and cannot be from the
outside, is the resulting floor itself: `x86-64-v2` and `armv8-a` come from
Vectorscan's own `cmake/cflags-x86.cmake` / `cmake/cflags-arm.cmake`. So
`scripts/release/verify-image.sh` disassembles the archive rather than trusting
the flags — but be precise about what it proves: it counts **AVX2** operands and
mnemonics on amd64 and **SVE** on arm64, so it catches the regression that
matters (a "baseline" archive that is really an AVX2 one) and would not catch a
default that moved the floor without reaching AVX2, such as an `x86-64-v3` build
that emits only BMI2 and FMA. Widening that pattern needs an amd64 baseline
archive to prove it produces no false rejects, which is why it is still open.

| Platform | Configuration | cmake target | Effective compiler flag | Tag |
|---|---|---|---|---|
| amd64 | baseline (default, `EXTRA_CMAKE_FLAGS=""`) | `x86-64-v2` | `-march=x86-64-v2 -mtune=generic` (`-msse4.2`) | default multi-arch tag |
| amd64 | avx2 (`EXTRA_CMAKE_FLAGS="-DBUILD_AVX2=ON"`) | `core-avx2` | `-march=core-avx2` (`-mavx2`) | `-avx2`, amd64-only |
| arm64 | baseline (only configuration) | `armv8-a` | `-march=armv8-a -mtune=generic` | default multi-arch tag |

Every arm64 v8-A CPU has NEON, so `armv8-a` runs everywhere and needs no
variant — microarchitecture fragmentation is an x86-only problem here.
`-DBUILD_AVX512=OFF` is passed unconditionally; AVX-512 is not offered as a
variant.

## 5. The two release gates

`FAT_RUNTIME=OFF` and the instruction-set pin are each backed by a gate that
measures the claim instead of assuming it. They catch different failure
modes and neither substitutes for the other:

1. **Engine firing** (§3, inside `docker/services/Dockerfile`'s builder) —
   proves the linked `libhs` actually scans. A `FAT_RUNTIME=ON` regression
   links cleanly and produces a working binary that silently never redacts
   anything; only running the matcher catches that.

2. **ISA baseline** (`scripts/release/verify-image.sh`, run by
   `scripts/release/build-images.sh` once per buildbase, before any service
   image is built from it) — proves a "baseline" build genuinely carries no
   AVX2 (amd64) or SVE/SVE2 (arm64) instructions, and that an "avx2" build
   genuinely does.

   This gate disassembles `libhs.a` **inside the buildbase image**, not the
   linked service binary, and that choice is load-bearing: Go's standard
   library compiles hand-written, runtime-CPUID-gated AVX2 assembly for its
   crypto routines into every amd64 binary. That code is safe on pre-AVX2
   CPUs precisely because it dispatches at runtime, but `objdump` cannot
   distinguish it from `libhs`'s compile-time-fixed AVX2 — scanning a linked
   service binary would reject every correct baseline image while proving
   nothing about the case that actually matters. `libhs.a` is where
   `FAT_RUNTIME=OFF` fixes the instruction set, so it is the artifact that
   has to be measured.

   The match patterns are structural, not an exhaustive mnemonic
   enumeration: any `%ymm` register operand is AVX2 by construction, plus a
   named list of the mnemonics Vectorscan's byte-scanning matchers (Shufti,
   Truffle, Teddy) actually emit. `vzeroupper` is deliberately excluded — it
   is VEX-encoded but not AVX2-specific and can appear in ABI transition
   sequences, so including it would be the one pattern capable of failing a
   correct baseline archive.

A third check, `scripts/release/smoke-compose.sh`, gates the *published*
image tags before the GitHub Release is created (§8) — it is a
compose-level acceptance test, not an ISA or engine check, and is covered in
§8 and in the operator doc. It brings the compose stack up against the
exact-version tag (`<version>`, immutable per release) and asserts
`/v1/models` enforces virtual-key admission and every seeded credential was
rotated (see the operator doc's "Credential rotation" section) — no
provider credentials exist in CI, so the gate cannot exercise a full chat
completion; the enforcement + rotation checks are what it actually proves.

It also carries the sign-in flow to its end: authorization code → token →
one authenticated `/api/admin/me`. That last hop is the point. A credential
check stops at "the password is correct", and a console can be unusable well
past that — the control plane verifies admin tokens against a JWKS it fetches,
and if that address is unreachable from inside its container it rejects tokens
it has just issued, so sign-in succeeds and the SPA bounces straight back to
the login form. Asserting only the password step reports PASS on a deployment
nobody can log into, which is the same shape of blind spot as authenticating
over a `redirect_uri` no user of the deployment can produce.
Ordering is load-bearing here: `release.yml`'s `publish` job composes only
the immutable `<version>` tag before this gate runs. The mobile/rolling
tags (`<major.minor>`, `latest`, `latest-avx2`) are composed, and the Docker
Hub mirror runs, only AFTER the smoke passes — a failed smoke must not have
already moved `latest` forward on either registry.

## 6. The tag contract

| Tags | Platforms | libhs configuration |
|---|---|---|
| `<version>`, `<major.minor>`, `latest` | multi-arch manifest: `linux/amd64` + `linux/arm64` | amd64 `x86-64-v2`, arm64 `armv8-a` |
| `<version>-avx2`, `latest-avx2` | `linux/amd64` only | `core-avx2` |

The `-avx2` variant exists only for the four Go services
(`nexus-hub`, `control-plane`, `ai-gateway`, `compliance-proxy`) — the ones
that link `libhs`. `control-plane-ui` and `db-migrator` are published only
under the default multi-arch tags.

`deploy/docker-compose.yml` never references an `-avx2` image; the AVX2
variant is an opt-in the operator chooses explicitly by setting
`NEXUS_VERSION=<version>-avx2` in `.env` (see the operator doc for the
`/proc/cpuinfo` check and the `SIGILL` consequence of guessing wrong).

`docker buildx imagetools create` composes the per-architecture images built
by the release matrix (`amd64-baseline` + `arm64` → default tags,
`amd64-avx2` → `-avx2` tags) without rebuilding — it copies manifests by
digest, so the published multi-arch tag and its per-architecture legs are
byte-identical to what the CI matrix produced and verified.

## 7. Registry and provenance

Primary registry: `ghcr.io/alphabitcore/<image>`. Mirrored to
`docker.io/alphabitcore/<image>` under the same image names — `imagetools
create` copies the already-built-and-verified GHCR manifest rather than
rebuilding, so the two registries serve the identical digest. Note the
image names themselves are NOT uniformly `nexus-`-prefixed: only
`nexus-hub` and `nexus-buildbase` are; `control-plane`, `ai-gateway`,
`compliance-proxy`, `control-plane-ui`, and `db-migrator` are not (see §1's
table for the full name list). Mirroring covers every tag the release
composes, including the amd64-only `-avx2` / `latest-avx2` variants (§6) —
Docker Hub is not a partial mirror of GHCR's tag set.

Every one of the six **published service/UI/migrator images** — everything
in §1's table except `nexus-buildbase` — carries OCI labels
`org.opencontainers.image.revision` (the commit SHA), `.version`, `.source`,
and `.licenses`. Every Go binary is stamped with the same commit through the
existing `-X main.buildVersion=<version>@<revision>` ldflag, so a running
binary and its container image can each answer "which commit produced me"
independently of CI run metadata. `nexus-buildbase` itself carries none of
these labels — it is a build-time convenience artifact rather than a
released contract (see §1), so its own provenance was not considered
load-bearing enough to justify the label-stamping wiring `buildbase.yml`
does not currently have.

After the smoke passes and both registries are mirrored, the `publish` job
records the resulting manifest-list digest for every published tag (the
exact-version and `-avx2` tags, on both registries) via `docker buildx
imagetools inspect` into `dist/release/IMAGE-DIGESTS.txt`, which is attached
to the GitHub Release alongside the compose file, `.env.example`,
`init-secrets.sh`, the Linux tarballs, and `SHA256SUMS` — the release's own
record of exactly which digest each tag resolved to at publish time,
independent of whatever either registry serves later.

`.github/workflows/release.yml` is a `workflow_dispatch` with explicit
`version` and `ref` inputs — nothing is inferred from a tag push. CI
contains no build logic of its own: every step invokes
`scripts/release/build-images.sh`, `scripts/release/verify-image.sh`,
`scripts/release/build-tarball.sh`, or `scripts/release/smoke-compose.sh`, so
any CI result is reproducible on a maintainer's laptop of a matching
architecture (arm64 excepted — it needs arm64 hardware; there is no QEMU
step in this pipeline).

`build-images.sh` covers **all six** images, which is what makes that
reproducibility claim hold: the buildbase, the four Go services against it,
and then `control-plane-ui` and `db-migrator`. The two Node images link no
`libhs` and therefore have no instruction-set variant, so the script builds
them on the baseline leg only and the `avx2` leg leaves them alone — matching
the tag contract in §5, where the `-avx2` tags exist for the four Go services
alone. A second definition of how any image is built — an inline `docker
build` in the workflow, or a command a maintainer is expected to type
alongside the script — would be a copy free to drift from the one the release
actually publishes.

`nexus-buildbase` is versioned by what determines its contents, not by the
service release version, and rebuilt only when the Vectorscan pin or the Go
toolchain version changes — `.github/workflows/buildbase.yml` triggers on a
push to the default branch touching `docker/buildbase/Dockerfile` itself or
the workflow file, or on manual dispatch. The branch restriction matters
because this workflow republishes the pinned `vs5.4.12-*` tags in place
(no version bump per push) — an unrestricted trigger would let a push to
any branch touching the Dockerfile overwrite what that tag resolves to.

## 8. Quickstart compose topology

`deploy/docker-compose.yml` is self-contained and references published
images only (no `build:` keys) — it is a different artifact from the
repository-root `docker-compose.yml`, which is development infrastructure
for running services from source and is unaffected by any of this.

Startup order, enforced by `depends_on` + healthchecks: `postgres` /
`valkey` / `nats` become healthy → `db-migrator` runs once
(`restart: "no"`, gated by `service_completed_successfully` on every
dependent service) → `nexus-hub`, `control-plane`, `ai-gateway`,
`control-plane-ui` start. Only the UI (`8080`) and the AI Gateway (`3050`)
are published to the host by a default `up` — the optional compliance profile
publishes `3128` as well; Postgres, Valkey, and NATS stay on the internal
compose network — a smaller attack surface, and it lets this stack coexist
with a developer's running dev stack without port collisions.

### 8.1 Writable state in a distroless runtime

Four services run from `gcr.io/distroless/cc-debian12:nonroot` as uid 65532, and
two of them need a writable directory: the ai-gateway's audit spill root
(`/var/lib/nexus`) and the compliance proxy's NDJSON fallback root
(`/var/lib/nexus-proxy`). The systemd form factor grants both through the units'
`ReadWritePaths=`; the compose form factor mounts a named volume at each.

Mounting the volume is necessary but not sufficient, and the reason is worth
recording because it is silent when wrong. Docker initialises a fresh named
volume from the image's directory at that path — **including its owner** — so a
mount point that does not exist in the image is created `root:root` and the
service cannot write inside it. `docker/services/Dockerfile` therefore grafts an
empty directory into each of the two runtime stages with
`COPY --from=builder --chown=65532:65532`; the distroless base has no shell, so
there is no `RUN mkdir` to do it with.

What the failure looks like without that: the service starts, serves, and logs a
single WARN. Both services resolve their overflow policy through
`shared/audit/lossmode` and default to `spillblock`, which `WithoutDurableSink`
degrades to `block` when no spool is wired — so neither drops, but both lose the
disk buffer that absorbs a burst, and the back-pressure moves to the emitting
path. For the compliance proxy that path is an intercepted TLS connection, which
makes the degraded mode more disruptive there than at the gateway.
`scripts/release/smoke-compose.sh` asserts both directions — no `create spool
directory` line in either service's log, and the spool directory actually present
in each volume.

### 8.2 The migrator ships thin, and what that costs

`docker/db-migrator/Dockerfile` carries `tools/db-migrate`'s source and its
dedicated lockfile but **no `node_modules`**: the dependencies are installed on
the first run for a given lockfile into the `migrator-deps` named volume that
`deploy/docker-compose.yml` mounts over that directory, and every later `up`
reuses the volume. The trade is explicit and belongs in this document because it
changes the compose stack's dependency surface: **a first `docker compose up`
reaches `registry.npmjs.org`**, so the two image registries are no longer the
whole egress requirement. The install is staged — it builds the new tree beside
the volume and swaps it in only after `npm ci` exits 0 — because installing in
place would let a failed reinstall destroy a working dependency set, and every
service gates on `db-migrator: service_completed_successfully`, which would
leave the stack unstartable at any version until the registry came back.

Two gates hold the property in place, both in `scripts/release/smoke-compose.sh`:
the first migrator pass must report installing, and a second pass against an
unchanged lockfile must report reuse. Losing install-once is otherwise silent —
it just reinstalls on every `up` and still exits 0.

`control-plane-ui` runs nginx with `deploy/nginx/nexus-ui.conf`, routing
`/api/*` and `/oauth/*` /`/authserver/*` to `control-plane`, the `/v1*` /
`/v1beta*` / `/openai/deployments*` / `/api/paas*` ingress family to
`ai-gateway`, and `/ws` plus the Hub's internal-facing routes to
`nexus-hub`. It mirrors `nexus-ami/artifacts/configs/nginx-nexus.conf` so
the container and appliance form factors expose the same surface, with two
deliberate differences: it listens on plain HTTP (TLS terminates in front of
the quickstart, not inside it), and it proxies to compose service names
rather than `127.0.0.1`.

**The compliance proxy is not part of the default `docker compose up`.** It
sits behind `docker compose --profile compliance up -d` because MITM
interception is an opt-in posture, not because it is unfinished: `init-secrets.sh`
generates the CA pair into `deploy/compliance-ca/`, `deploy/docker-compose.yml`
mounts that directory read-only at the path the service's config expects, and
the profile works out of the box. Enabling it publishes a third host port
(`3128`) in addition to `8080` and `3050`, and every client whose TLS it
intercepts must trust the generated CA — see the operator doc for both.

`init-secrets.sh` (§9) and the credential-rotation contract it feeds —
covering both the bootstrap admin/assistant-key rotation and the demo
tier's own `NexusUser`/`VirtualKey`/`AdminApiKey` rotation
(`docker/db-migrator/rotate-demo-secrets.mjs`) — are covered in the
operator doc, which is the operational source of truth for running this
stack; this section covers only the static topology.

## 9. Linux tarball

`scripts/release/build-tarball.sh` produces
`nexus-gateway-<version>-linux-<arch>.tar.gz` for operators who deploy with
systemd rather than containers. It reuses the `nexus-buildbase` image to
compile the same four binaries, but with `libstdc++` and `libgcc` linked
**statically** via `-extldflags`, rather than the dynamic link the container
images use (where the runtime base already carries `libstdc++`). A tarball
that still needs the host distribution's C++ runtime to match would defeat
the point of shipping a tarball — `ldd` is run against every produced binary
as part of the build and the build fails if a dynamic `libstdc++` or
`libgcc_s` dependency remains.

The same firing self-test from §3 is re-run here, linked with the identical
`-extldflags` recipe the shipped binaries use, so the proof that Vectorscan
scans covers the binaries actually being packaged, not a differently-linked
stand-in.

The archive stages `bin/` (four static binaries), `ui/` (the built Vite
`control-plane-ui` dist), `systemd/` (the four `deploy/systemd/*.service`
units), and `config/` (each service's prod-shape `*.config.yaml` plus the
repository-root `.env.example` as `env.example`). A `SHA256SUMS` file
accompanies each architecture's tarball; see the operator doc for the
install sequence and what the tarball intentionally does not include (a
database migrator, a reverse proxy, and Postgres/Valkey/NATS themselves).

## 10. Relationship to the AMI build

`docker/buildbase/Dockerfile` and `scripts/release/build-tarball.sh` are
both derived from `nexus-ami/scripts/build-binaries.sh`, which established
the `FAT_RUNTIME=OFF` requirement and the firing self-test for this project
in the first place. The container build keeps the same Vectorscan tag,
commit pin, and cmake flags (`-DFAT_RUNTIME=OFF -DBUILD_AVX512=OFF`).

The difference is §4's instruction-set baseline gate, which has no AMI
counterpart. The AMI compiles `libhs` on the same machine class the AMI then
runs on (Packer builds and boots the same instance type family), so there is
no cross-hardware gap for it to guard against — whatever instruction set the
build instance's CPU implies is also what every launched instance gets. A
published container image has no such guarantee: it is built once in CI and
pulled onto whatever hardware an operator's cluster happens to run, so §4's
gate exists specifically to make the container form factor's baseline claim
verified rather than assumed.

## 11. Related docs

- `docs/operators/ops/container-deployment.md` — operator-facing quickstart,
  credential rotation, tag selection, tarball install, upgrade path.
- `docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md`
  — the AMI / bare-metal appliance form factor this design derives its
  Vectorscan handling from.
- `docker/README.md` — build definitions index + local build commands.
- `nexus-ami/scripts/build-binaries.sh` — prior art for `FAT_RUNTIME=OFF`
  and the firing self-test.
