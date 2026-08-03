#!/usr/bin/env bash
# Cross-compile the agent for every platform it ships to.
#
# Why this gate exists: the agent's platform shims are split by build tag, and
# cmd_run.go builds ONE struct literal that every variant must satisfy. A field
# added to wire_bridge_darwin.go's DarwinBridgeArgs without mirroring it into
# wire_bridge_other.go compiles perfectly on a macOS workstation and fails only
# under GOOS=linux — the platform the .deb and the container image ship.
#
# Not hypothetical: wire_bridge_other.go carries a comment stating the rule
# verbatim ("MUST stay field-for-field aligned ... or non-darwin cross-compiles
# break with 'unknown field ...' at cmd_run.go's struct literal site"), and the
# rule was broken anyway, because a comment is not a gate. The break survived
# every `go build ./...` run on the branch: that command builds for the HOST only.
#
# Scope, stated rather than implied:
#   linux   — CGO_ENABLED=0. Catches the type-level breakage this gate exists for.
#             The real image builds with cgo; a cross-cgo toolchain is not assumed
#             here, so linkage errors are the container build's job, not this one's.
#   darwin  — CGO_ENABLED=1, and only on a darwin host. The darwin agent genuinely
#             needs cgo (internal/platform/darwin/proc's Meta / ProcessInfo come
#             from a cgo file), so a CGO_ENABLED=0 darwin build fails for reasons
#             that say nothing about the shims.
#   windows — CGO_ENABLED=0, ENFORCED. It was warn-only while
#             packages/shared/core/profiling/pprof.go referenced syscall.SIGUSR1,
#             which does not exist there; the capture signal now lives behind a
#             build tag (dumpsignal_{unix,windows}.go) and the Windows build is
#             clean, so the platform is enforced rather than watched. A warn-only
#             platform is a platform nobody notices breaking twice.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
cd packages/agent || exit 1

fail=0

run_build() { # goos cgo label
  # The verdict is `go build`'s EXIT CODE, not whether it printed anything.
  # Deciding on stderr-emptiness fails both ways: GOFLAGS=-x, a cold module cache
  # ("go: downloading …") or any toolchain notice turns a passing build red, and a
  # build killed by a signal prints nothing at all and would have read as green.
  # Verified: with GOFLAGS=-x the old predicate reported "✗ FAILED GOOS=linux" for
  # a build that exits 0 on its own.
  local out rc
  out="$(GOOS="$1" GOARCH=amd64 CGO_ENABLED="$2" go build -o /dev/null ./cmd/agent 2>&1)"
  rc=$?
  if (( rc != 0 )); then
    printf '[check:agent-cross-build] %s GOOS=%s (CGO=%s) exit=%d:\n' "$3" "$1" "$2" "$rc"
    echo "$out" | sed 's/^/    /'
    return 1
  fi
  printf '[check:agent-cross-build] ✓ GOOS=%s (CGO=%s)\n' "$1" "$2"
  return 0
}

run_build linux 0 "✗ FAILED" || fail=1

if [[ "$(go env GOHOSTOS)" == "darwin" ]]; then
  run_build darwin 1 "✗ FAILED" || fail=1
else
  echo "[check:agent-cross-build] ~ GOOS=darwin skipped: needs cgo, so it is only checked on a darwin host"
fi

run_build windows 0 "✗ FAILED" || fail=1

if (( fail )); then
  echo "[check:agent-cross-build] FAILED — an enforced platform does not compile."
  exit 1
fi
echo "[check:agent-cross-build] OK — every enforced platform compiles."
