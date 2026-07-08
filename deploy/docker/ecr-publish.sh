#!/usr/bin/env bash
# Build + publish the Nexus images to PRIVATE Amazon ECR so the two arena Nexus
# boxes can pull them the same way every other gateway box pulls its image
# (decision of record: ECR/Option 1; public Docker Hub is separate + James-gated).
#
# This mirrors .github/workflows/docker-publish.yml's build recipe exactly (same
# 5 images, same Dockerfiles, same vectorscan gates) — the only difference is the
# registry (private ECR) and an immutable git-sha tag for run provenance.
#
# Gates before a push (same intent as the workflow):
#   Gate B — vectorscan runtime self-check (hs-selfcheck) on the two scanning
#            images. Proves hs_compile/hs_alloc_scratch/hs_scan work in the exact
#            shipped image; a FAT_RUNTIME=ON libhs that silently never scans
#            (PII passthrough) fails here. The two scanning images are NOT pushed
#            unless this passes.
#
# Requires: awscli v2, docker (buildx), an AWS identity that can create/push to
# ECR in the target account. Run AFTER the build test is green (CLAUDE-CODE
# TASK 1) — a red scan path must never be published.
#
# Usage:
#   AWS_ACCOUNT=511092106101 AWS_REGION=us-east-1 ./deploy/docker/ecr-publish.sh
#   (TAG defaults to the current git sha; override with TAG=v1.2.3)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

: "${AWS_ACCOUNT:?set AWS_ACCOUNT (the arena AWS account id)}"
AWS_REGION="${AWS_REGION:-us-east-1}"
TAG="${TAG:-$(git rev-parse --short=12 HEAD)}"
REGISTRY="${AWS_ACCOUNT}.dkr.ecr.${AWS_REGION}.amazonaws.com"
DIGESTS_FILE="${DIGESTS_FILE:-/tmp/nexus-ecr-digests-${TAG}.txt}"

# image | dockerfile | vectorscan(true|false)   — identical to docker-publish.yml
IMAGES=(
  "nexus-hub|packages/nexus-hub/Dockerfile|false"
  "nexus-console|deploy/docker/console/Dockerfile|false"
  "nexus-ai-gateway|packages/ai-gateway/Dockerfile|true"
  "nexus-compliance-proxy|packages/compliance-proxy/Dockerfile|true"
  "nexus-db-migrate|tools/db-migrate/Dockerfile|false"
)

echo "→ registry=$REGISTRY  tag=$TAG"

# 1) ECR repos — idempotent, IMMUTABLE tags (a tag never silently changes what a
#    box pulled; provenance stays honest).
for spec in "${IMAGES[@]}"; do
  name="${spec%%|*}"
  aws ecr describe-repositories --region "$AWS_REGION" --repository-names "$name" >/dev/null 2>&1 \
    || aws ecr create-repository --region "$AWS_REGION" --repository-name "$name" \
         --image-tag-mutability IMMUTABLE --image-scanning-configuration scanOnPush=true >/dev/null
done

# 2) login
aws ecr get-login-password --region "$AWS_REGION" | docker login --username AWS --password-stdin "$REGISTRY"

: > "$DIGESTS_FILE"
for spec in "${IMAGES[@]}"; do
  IFS='|' read -r name dockerfile vectorscan <<<"$spec"
  local_ref="$REGISTRY/$name:$TAG"
  echo "→ building $name ($dockerfile) vectorscan=$vectorscan"
  docker build --platform linux/amd64 -f "$dockerfile" -t "$local_ref" .

  # Gate B: the two scanning images must prove the scan engine works at runtime
  # BEFORE they are pushed. hs-selfcheck prints scanRC=0 matches=1 on success.
  if [[ "$vectorscan" == "true" ]]; then
    echo "  Gate B: hs-selfcheck (runtime scan proof) …"
    if ! docker run --rm --entrypoint hs-selfcheck "$local_ref"; then
      echo "  ✗ $name failed the vectorscan runtime self-check — NOT pushing." >&2
      echo "    (a red scan engine ships PII unredacted; fix before publishing)" >&2
      exit 1
    fi
  fi

  echo "  pushing $local_ref"
  docker push "$local_ref"
  digest="$(aws ecr describe-images --region "$AWS_REGION" --repository-name "$name" \
    --image-ids imageTag="$TAG" --query 'imageDetails[0].imageDigest' --output text)"
  echo "$name  $REGISTRY/$name@$digest" | tee -a "$DIGESTS_FILE"
done

echo ""
echo "✓ published ${#IMAGES[@]} images to $REGISTRY at tag $TAG"
echo "  digests (record these in run provenance): $DIGESTS_FILE"
echo ""
echo "Arena Nexus boxes pull with:"
echo "  aws ecr get-login-password --region $AWS_REGION | docker login --username AWS --password-stdin $REGISTRY"
echo "  docker pull <one of the digests above>"
echo "The pulling box needs the instance-role policy in deploy/docker/ecr-pull-policy.json."
