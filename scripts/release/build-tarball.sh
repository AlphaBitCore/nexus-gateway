#!/usr/bin/env bash
# build-tarball.sh — produce a self-contained Linux bundle for operators who
# deploy with systemd rather than containers.
#
# Binaries link libstdc++ and libgcc statically. The Vectorscan cgo build
# otherwise carries a dynamic libstdc++ dependency, and a tarball that fails to
# start on a distribution with a different C++ runtime defeats the point of
# shipping a tarball at all.
set -euo pipefail

VERSION="${VERSION:-dev}"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    *) echo "ERROR: unknown flag $1" >&2; exit 1 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

ARCH="$(docker version --format '{{.Server.Arch}}')"
REVISION="$(git rev-parse HEAD)"
BUILDBASE_TAG="nexus-buildbase:${VERSION}-baseline-${ARCH}"
OUT="dist/release"
name="nexus-gateway-${VERSION}-linux-${ARCH}"
# Stage under a top-level directory NAMED after the release ("$name/"), not
# directly in a bare temp dir, so the tarball itself has a top-level
# directory. Extracting a tarball whose members are bin/, ui/, systemd/,
# config/ directly at the archive root scatters those into whatever
# directory the operator happened to run `tar xzf` from; a named top-level
# directory is what makes `tar xzf nexus-gateway-....tar.gz` land everything
# under one place instead.
STAGE_ROOT="$(mktemp -d)"
STAGE="$STAGE_ROOT/$name"
mkdir -p "$STAGE"
trap 'rm -rf "$STAGE_ROOT"' EXIT

# The firing self-test's test set, named exactly and anchored. `-run` is an
# unanchored regexp and `go test` exits 0 when nothing matches, so a loose
# pattern can silently select nothing — or the wrong tests: this package also
# holds TestVectorscan_ScanDuringClose, which asserts a scan after Close returns
# NO hits and therefore passes on a dead engine. Both values below move together
# with this list; the run asserts the number of PASS lines, so a rename fails
# the build instead of quietly reducing what is proven. Same list as
# docker/services/Dockerfile.
SELFTEST_PATTERN='^(TestHSSelfTest|TestVectorscan_ScanUnderCgoLimit|TestVectorscan_ScanComplete_ReportsCompletion)$'
SELFTEST_COUNT=3

echo "==> [tarball] ensuring buildbase exists"
docker image inspect "$BUILDBASE_TAG" >/dev/null 2>&1 || \
  docker build -f docker/buildbase/Dockerfile -t "$BUILDBASE_TAG" docker/buildbase/

# The tarball is the artifact most exposed to arbitrary CPUs — systemd installs
# it on whatever hardware the operator owns, with no image manifest to constrain
# the platform — so its baseline claim gets measured here rather than inherited
# from whatever else happened to run first on this machine. In CI the buildbase
# usually already exists because build-images.sh ran and gated it, but that is
# step ordering, not a guarantee: a maintainer building a hotfix tarball locally
# reaches the `docker build` above instead, and a same-named image may have been
# built by hand with arbitrary --build-arg values.
scripts/release/verify-image.sh "$BUILDBASE_TAG" baseline

echo "==> [tarball] compiling statically-linked C++ runtime binaries"
mkdir -p "$STAGE/bin"
docker run --rm -v "$REPO_ROOT":/src -v "$STAGE/bin":/out "$BUILDBASE_TAG" bash -c "
  set -euo pipefail
  # --no-as-needed for -lm for the same reason the buildbase needs it: libhs's
  # fdr_compile needs pow and libhs.pc does not declare libm, so -lm precedes
  # -lhs and Debian's default --as-needed would drop it. CGO_LDFLAGS carries
  # only this — libstdc++/libgcc are forced static via -extldflags below, not
  # here — because CGO_LDFLAGS is appended once per cgo-importing package
  # (runtime/cgo, net, os/user, the matcher package all import cgo), so
  # whatever it names is repeated multiple times on the final host-link
  # command line, interleaved with each package's own #cgo directives.
  export CGO_LDFLAGS='-Wl,--no-as-needed -lm'
  # Static libstdc++/libgcc go through -extldflags, not CGO_LDFLAGS, and use
  # GNU ld's \"-l:<filename>\" colon syntax rather than
  # \"-lstdc++ -static-libstdc++\". Both choices are load-bearing, confirmed
  # empirically by building all four real binaries against the real libhs.a:
  #   1. Plain -lstdc++ is required at all — cgo links via the bare \`gcc\`
  #      driver (not \`g++\`), so nothing implies libstdc++ automatically;
  #      omitting it reproduces the buildbase's own documented failure mode
  #      (see docker/buildbase/Dockerfile) one level up — control-plane
  #      (which imports the vectorscan matcher, unlike nexus-hub) fails to
  #      link with 136 undefined libhs C++ symbols (__cxa_*, operator
  #      new/delete, std::__throw_*).
  #   2. \"-static-libstdc++ -lstdc++\" in CGO_LDFLAGS looked right and passed
  #      for ai-gateway and nexus-hub, but failed the exact same way for
  #      control-plane and compliance-proxy. Cause: CGO_LDFLAGS repeats once
  #      per cgo package (see above), and gcc's static substitution only
  #      converts the *first* \"-lstdc++\" it sees — repeats resolve as an
  #      ordinary dynamic \"-l\" flag, so whether the binary ends up clean
  #      depends on how many cgo packages happen to link after the matcher
  #      package for that specific binary's import graph. That is a property
  #      of unrelated packages, not something this script controls or should
  #      rely on.
  #   3. \"-l:libstdc++.a\" replaces gcc's substitution with GNU ld naming the
  #      static archive directly, so every repeat is static by construction —
  #      but it is still a static *archive*: ld only pulls a member to
  #      satisfy a symbol that is *already* undefined at the point the
  #      archive is scanned, so it must appear after -lhs on the command
  #      line. CGO_LDFLAGS is textually prepended before the matcher
  #      package's own \"-L/usr/local/lib -lhs\", i.e. the wrong side, for
  #      exactly the same reason -Wl,--no-as-needed is needed for -lm above.
  #      -extldflags sidesteps this the same way: cmd/link appends it once,
  #      after every object file and every package's cgo flags — always
  #      after -lhs, regardless of cgo package import order — so it is the
  #      only placement that does not depend on which package's cgo group
  #      happens to land last for a given binary.
  EXTLD='-static-libgcc -Wl,--no-as-needed -l:libstdc++.a -l:libgcc.a -lm'
  for svc in \
    nexus-hub:packages/nexus-hub/cmd/nexus-hub \
    control-plane:packages/control-plane/cmd/control-plane \
    ai-gateway:packages/ai-gateway/cmd/ai-gateway \
    compliance-proxy:packages/compliance-proxy/cmd/compliance-proxy; do
    name=\${svc%%:*}; path=\${svc#*:}
    go build -tags vectorscan -trimpath \
      -ldflags \"-s -w -X main.buildVersion=${VERSION}@${REVISION} -extldflags '\$EXTLD'\" \
      -o /out/\$name ./\$path/
  done
  # Same -extldflags as the real binaries: the self-test binary must link
  # against libhs the identical way to be a faithful proof that the shipped
  # binaries' link recipe actually scans, not just that some other recipe does.
  out=\"\$(go test -tags vectorscan -count=1 -v -run '$SELFTEST_PATTERN' \
    -ldflags \"-extldflags '\$EXTLD'\" \
    ./packages/shared/policy/hooks/matcher/ 2>&1)\" || { printf '%s\\n' \"\$out\"; exit 1; }
  printf '%s\\n' \"\$out\"
  passed=\"\$(printf '%s\\n' \"\$out\" | grep -c '^--- PASS: ' || true)\"
  if [ \"\$passed\" != $SELFTEST_COUNT ]; then
    echo \"ERROR: expected $SELFTEST_COUNT firing self-tests to pass, got \$passed\" >&2
    exit 1
  fi
"

echo "==> [tarball] verifying no dynamic libstdc++/libgcc dependency remains"
for bin in "$STAGE"/bin/*; do
  # `grep -c` exits 1 on zero matches; under pipefail that would abort here at
  # exactly the moment the check passes. Checks both libstdc++ and libgcc_s
  # since both are required to be statically linked (see CGO_LDFLAGS/-extldflags
  # above) and either one regressing to dynamic should fail this gate.
  hits="$(docker run --rm -v "$STAGE/bin":/b "$BUILDBASE_TAG" \
            bash -c "ldd /b/$(basename "$bin") 2>/dev/null | grep -c -E 'libstdc\+\+|libgcc_s' || true")"
  if [ "$hits" -ne 0 ]; then
    echo "ERROR: $(basename "$bin") still links libstdc++ or libgcc dynamically" >&2
    exit 1
  fi
done
echo "    all binaries are free of a dynamic libstdc++/libgcc dependency"

# glibc is NOT static — only libstdc++/libgcc are — so the tarball carries a
# minimum-distribution requirement that the archive itself cannot express. The
# operator doc states it; this measures it, so a base-image or toolchain bump
# that raises the floor fails here instead of surfacing as
# "GLIBC_2.xx not found" on an operator's host after they follow the install
# steps. Raising DOCUMENTED_GLIBC_FLOOR means updating the operator doc's
# "Linux tarball" prerequisites in the same commit.
DOCUMENTED_GLIBC_FLOOR=2.34
echo "==> [tarball] measuring the glibc floor (documented as ${DOCUMENTED_GLIBC_FLOOR})"
for bin in "$STAGE"/bin/*; do
  floor="$(docker run --rm -v "$STAGE/bin":/b "$BUILDBASE_TAG" \
            bash -c "objdump -T /b/$(basename "$bin") 2>/dev/null | grep -oE 'GLIBC_[0-9]+\.[0-9]+' | sed 's/GLIBC_//' | sort -uV | tail -1")"
  if [ -z "$floor" ]; then
    echo "ERROR: could not read versioned glibc symbols from $(basename "$bin") — the floor was not measured" >&2
    exit 1
  fi
  highest="$(printf '%s\n%s\n' "$DOCUMENTED_GLIBC_FLOOR" "$floor" | sort -V | tail -1)"
  if [ "$highest" != "$DOCUMENTED_GLIBC_FLOOR" ]; then
    echo "ERROR: $(basename "$bin") requires glibc ${floor}, above the documented floor ${DOCUMENTED_GLIBC_FLOOR}." >&2
    echo "       Every host running this tarball would need glibc ${floor} or newer. Update" >&2
    echo "       docs/operators/ops/container-deployment.md and this value together." >&2
    exit 1
  fi
done
echo "    glibc floor measured at or below ${DOCUMENTED_GLIBC_FLOOR}"

echo "==> [tarball] staging UI and configs"
make control-plane-ui-build
mkdir -p "$STAGE/ui" "$STAGE/systemd" "$STAGE/config"
cp -r packages/control-plane-ui/dist/. "$STAGE/ui/"
cp deploy/systemd/*.service "$STAGE/systemd/"
for svc in nexus-hub control-plane ai-gateway compliance-proxy; do
  cp "packages/$svc/$svc.config.yaml" "$STAGE/config/$svc.yaml"
done
cp .env.example "$STAGE/config/env.example"

mkdir -p "$OUT"
# -C "$STAGE_ROOT" ... "$name" (not -C "$STAGE" .) is what puts "$name/" as
# the top-level directory inside the archive rather than its contents at the
# archive root.
tar -C "$STAGE_ROOT" -czf "$OUT/${name}.tar.gz" "$name"
# Fresh write (>), not append (>>): this OUT directory is not recreated
# between local reruns (unlike a CI matrix leg's fresh checkout), so `>>`
# would accumulate one duplicate line per rerun for the same version/arch.
# CI's own flatten step (release.yml) already assumes a single fresh line
# per matrix leg's SHA256SUMS before concatenating them; a fresh write here
# is what actually makes that assumption true instead of merely documented.
( cd "$OUT" && sha256sum "${name}.tar.gz" > SHA256SUMS )

echo "==> [tarball] $OUT/${name}.tar.gz"
