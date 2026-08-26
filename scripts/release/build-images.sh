#!/usr/bin/env bash
# build-images.sh — build the Nexus service images for one architecture and one
# instruction-set configuration.
#
# CI calls this; so can a maintainer. Nothing about the build lives in the
# workflow YAML, so a CI result is reproducible on a laptop of the same
# architecture.
#
# Usage:
#   build-images.sh --config baseline --version 1.3.0
#   build-images.sh --config avx2 --version 1.3.0 --push --registry ghcr.io/alphabitcore
set -euo pipefail

CONFIG=baseline
VERSION=dev
REGISTRY=local
PUSH=false

while [ $# -gt 0 ]; do
  case "$1" in
    --config)   CONFIG="$2"; shift 2 ;;
    --version)  VERSION="$2"; shift 2 ;;
    --registry) REGISTRY="$2"; shift 2 ;;
    --push)     PUSH=true; shift ;;
    -h|--help)  sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "ERROR: unknown flag $1" >&2; exit 1 ;;
  esac
done

case "$CONFIG" in
  baseline) EXTRA_CMAKE_FLAGS="" ;;
  avx2)     EXTRA_CMAKE_FLAGS="-DBUILD_AVX2=ON" ;;
  *) echo "ERROR: --config must be 'baseline' or 'avx2', got '$CONFIG'" >&2; exit 1 ;;
esac

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

ARCH="$(docker version --format '{{.Server.Arch}}')"
if [ "$CONFIG" = "avx2" ] && [ "$ARCH" != "amd64" ]; then
  echo "ERROR: the avx2 configuration is amd64-only; this host is $ARCH" >&2
  exit 1
fi

REVISION="$(git rev-parse HEAD)"
BUILD_VERSION="${VERSION}@${REVISION}"
BUILDBASE_TAG="nexus-buildbase:${VERSION}-${CONFIG}-${ARCH}"

echo "==> [build-images] buildbase ($CONFIG, $ARCH)"
docker build \
  --build-arg "EXTRA_CMAKE_FLAGS=${EXTRA_CMAKE_FLAGS}" \
  -f docker/buildbase/Dockerfile \
  -t "$BUILDBASE_TAG" \
  docker/buildbase/

# The instruction-set gate runs here, once per configuration, because the
# buildbase is where libhs's instruction set is fixed. Every service image built
# below inherits it, so verifying each of them separately would re-measure the
# same archive four times — and, on the linked binaries, would measure Go's
# runtime-gated crypto assembly instead.
scripts/release/verify-image.sh "$BUILDBASE_TAG" "$CONFIG"

for svc in nexus-hub control-plane ai-gateway compliance-proxy; do
  tag="${REGISTRY}/${svc}:${VERSION}-${CONFIG}-${ARCH}"
  echo "==> [build-images] $svc -> $tag"
  docker build \
    --target "$svc" \
    --build-arg "BUILDBASE=${BUILDBASE_TAG}" \
    --build-arg "BUILD_VERSION=${BUILD_VERSION}" \
    --label "org.opencontainers.image.revision=${REVISION}" \
    --label "org.opencontainers.image.version=${VERSION}" \
    -f docker/services/Dockerfile \
    -t "$tag" \
    .
  if [ "$PUSH" = true ]; then
    docker push "$tag"
  else
    # Locally, also apply a tag deploy/docker-compose.yml can actually consume:
    # it interpolates one NEXUS_VERSION into all six image references, so images
    # left only under "<version>-<config>-<arch>" cannot be run by the compose
    # stack at all — the local workflow relied on hand-issued `docker tag`
    # commands that were easy to forget, and a stale one silently tests a
    # previous build. In the release path the equivalent tag is a multi-arch
    # manifest composed from both architectures by the publish job, so it is
    # deliberately not created here.
    #
    # The suffix mirrors the published tag contract instead of letting both
    # configurations claim the same name. Running the documented pair on an
    # amd64 host — baseline first, then avx2 — would otherwise leave
    # "<registry>/<svc>:<version>" pointing at the AVX2 build, so a compose
    # stack the operator believes is the portable one would SIGILL on a pre-AVX2
    # CPU, with both ISA gates having passed.
    case "$CONFIG" in
      baseline) local_tag="${REGISTRY}/${svc}:${VERSION}" ;;
      *)        local_tag="${REGISTRY}/${svc}:${VERSION}-${CONFIG}" ;;
    esac
    docker tag "$tag" "$local_tag"
  fi
done

# The two Node images. Neither links libhs, so neither has an instruction-set
# variant: they are built once, on the baseline leg, and the AVX2 leg leaves
# them alone. Both take the repository root as their build context — the UI
# builds against the packages/ui-shared workspace, the migrator carries the
# whole tools/db-migrate tree — which is why neither can be built from inside
# its own directory.
if [ "$CONFIG" = baseline ]; then
  for svc in control-plane-ui db-migrator; do
    tag="${REGISTRY}/${svc}:${VERSION}-${CONFIG}-${ARCH}"
    echo "==> [build-images] $svc -> $tag"
    docker build \
      --label "org.opencontainers.image.revision=${REVISION}" \
      --label "org.opencontainers.image.version=${VERSION}" \
      -f "docker/${svc}/Dockerfile" \
      -t "$tag" \
      .
    if [ "$PUSH" = true ]; then
      docker push "$tag"
    else
      # Same reasoning as the service loop above: give the compose file a tag
      # it can actually interpolate a single NEXUS_VERSION into.
      docker tag "$tag" "${REGISTRY}/${svc}:${VERSION}"
    fi
  done
fi

echo "==> [build-images] done ($CONFIG, $ARCH, $VERSION)"
