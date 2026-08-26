#!/bin/bash
# lib-verify.sh — fail-closed supply-chain integrity helpers for the AMI build.
#
# the appliance image is the trust anchor that terminates and
# inspects all customer TLS, so every third-party artifact baked into it
# (Valkey, valkey-search, NATS, Node.js) MUST be verified against a pinned
# cryptographic digest recorded next to its version constant BEFORE it is
# compiled/installed. A build that trusts whatever bytes `curl`/`git clone`
# returns has no defense against a substituted dependency (moved tag, replaced
# release tarball, build-time on-path MITM).
#
# The pinned digests are committed in-repo and code-reviewed in the PR that
# bumps the version, so the build never trusts live upstream network: the
# digest is the trust root, not the connection. This is strictly stronger than
# a runtime GPG check against a keyring the build itself fetches.
#
# Sourced by install-valkey.sh / install-nats.sh / install-node-prisma.sh.
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

# nexus_sha256 <file>
#   Print the bare-hex sha256 of <file> to stdout, portably. The Linux AMI build
#   host ships GNU coreutils (`sha256sum`); a developer's macOS build host ships
#   only BSD `shasum`. Single source of truth for the hash tool so build.sh and
#   verify_sha256 never re-pick it inconsistently. Returns non-zero (and prints
#   nothing) if neither tool is present, so callers under `set -euo pipefail`
#   fail closed rather than emit an empty digest.
nexus_sha256() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    echo "ERROR: [nexus_sha256] no sha256 tool found (need sha256sum or shasum)" >&2
    return 1
  fi
}

# verify_sha256 <expected-hex> <file>
#   Abort (return 1) unless <file>'s sha256 matches <expected-hex>. Fail-closed:
#   an empty/blank expected digest, a missing file, or any mismatch terminates
#   the build (callers run under `set -euo pipefail`, so a non-zero return
#   aborts the whole install).
verify_sha256() {
  local expected="$1" file="$2"
  if [ -z "${expected// /}" ]; then
    echo "ERROR: [verify_sha256] no expected digest supplied for $file — refusing to install an unverified artifact" >&2
    return 1
  fi
  if [ ! -f "$file" ]; then
    echo "ERROR: [verify_sha256] file not found: $file" >&2
    return 1
  fi
  local actual
  actual=$(nexus_sha256 "$file") || return 1
  if [ "$actual" != "$expected" ]; then
    echo "ERROR: [verify_sha256] checksum MISMATCH for $file" >&2
    echo "  expected: $expected" >&2
    echo "  actual:   $actual" >&2
    return 1
  fi
  echo "==> [verify] sha256 OK: $file"
}

# verify_git_commit <dir> <expected-sha>
#   Abort (return 1) unless the git HEAD of <dir> equals <expected-sha>. Defeats
#   a moved upstream lightweight tag pointing at attacker code. The submodule
#   gitlinks (gRPC/Protobuf/Abseil for valkey-search) are part of the pinned
#   commit's tree, so verifying the top commit transitively pins every vendored
#   dependency to an immutable content hash.
verify_git_commit() {
  local dir="$1" expected="$2"
  if [ -z "${expected// /}" ]; then
    echo "ERROR: [verify_git_commit] no expected commit supplied for $dir — refusing to build an unpinned checkout" >&2
    return 1
  fi
  local actual
  if ! actual=$(git -C "$dir" rev-parse HEAD 2>/dev/null); then
    echo "ERROR: [verify_git_commit] not a git repository: $dir" >&2
    return 1
  fi
  if [ "$actual" != "$expected" ]; then
    echo "ERROR: [verify_git_commit] $dir resolved to HEAD=$actual, expected pinned $expected (tag moved / supply-chain drift)" >&2
    return 1
  fi
  echo "==> [verify] commit OK: $dir @ $expected"
}
