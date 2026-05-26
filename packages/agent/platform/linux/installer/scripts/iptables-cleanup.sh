#!/bin/sh
# ExecStopPost belt — fires after the nexus-agent service stops to
# ensure the NEXUS_AGENT iptables / ip6tables chain and its OUTPUT
# hook are gone. Idempotent: every step is `|| true` so a missing
# chain is harmless.
#
# The daemon's own shutdown handler already does this on clean
# SIGTERM; this script covers SIGKILL / OOM / panic-before-handler
# cases where the daemon couldn't run its own teardown.
set -u

CHAIN="NEXUS_AGENT"

for tool in iptables ip6tables; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        continue
    fi
    "$tool" -t nat -D OUTPUT -j "$CHAIN" 2>/dev/null || true
    "$tool" -t nat -F "$CHAIN"            2>/dev/null || true
    "$tool" -t nat -X "$CHAIN"            2>/dev/null || true
done

exit 0
