#!/usr/bin/env bash
# tests/lib/env.sh — back-compat shim. The real loader is loadenv.sh.
#
# Older test scripts source this file unconditionally as their first line.
# It now delegates to loadenv.sh — target is taken from $NEXUS_TEST_TARGET
# (or defaults to "local" on TTY runs per the loadenv contract).
#
# New code should source tests/lib/loadenv.sh directly so the target choice
# is obvious in the caller.

_env_sh_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Pass an empty positional argument so loadenv.sh does NOT inherit the caller's
# `$1`. Otherwise scripts that source env.sh and accept their own flags
# (e.g. `run-all.sh --core`) would have their flag interpreted as a target
# string, failing with "unknown target '--core'". env.sh's contract is to
# resolve target from `$NEXUS_TEST_TARGET` (or default to local on TTY).
# shellcheck disable=SC1091
source "$_env_sh_dir/loadenv.sh" ""
unset _env_sh_dir
