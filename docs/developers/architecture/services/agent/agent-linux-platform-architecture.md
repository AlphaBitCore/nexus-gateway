# Agent Linux platform

On Linux the agent captures outbound traffic with a transparent proxy: an
iptables `REDIRECT` rule in the `nat` table sends outbound TCP to a local
listener (default `127.0.0.1:19080`, with a sibling `[::1]:19080` listener for
IPv6) inside the Go daemon. The daemon recovers each connection's real
destination with `SO_ORIGINAL_DST`, attributes the originating process through
`/proc`, asks the policy engine for a decision, and inspects, passes through, or
denies. Interception runs in `ModeIPTables`. Both address families are handled:
IPv4 flows land on the `127.0.0.1` listener, IPv6 flows on the `[::1]` listener
(best-effort — a host with IPv6 disabled runs IPv4-only).

Two pieces make the transparent redirect safe and durable: an `SO_MARK` on the
agent's own sockets keeps its egress out of its own redirect (no self-loop), and
a reconciler keeps the iptables chain healed against anything else on the host
that rewrites netfilter (firewalld, ufw, a manual flush).

## The NEXUS_AGENT chain

All redirect logic lives in a dedicated `nat`-table chain, `NEXUS_AGENT`, hooked
from `OUTPUT` by a single `-j NEXUS_AGENT` jump. The chain holds three rules in
order:

1. `-m mark --mark 0x4e58 -j RETURN` — traffic the agent itself originated
   (stamped with `SO_MARK`) returns immediately, never redirected.
2. `-d 127.0.0.0/8 -j RETURN` (IPv4) or `-d ::1/128 -j RETURN` (IPv6) — loopback
   is left alone.
3. `-p tcp -j REDIRECT --to-ports <proxy port>` — everything else is redirected
   to the local listener.

The chain is maintained for both families: `iptables` for IPv4 and `ip6tables`
for IPv6, which are present even on nft-only distributions as the `iptables-nft`
compatibility shim.

On hosts whose `nat` table already holds nft-native rules (libvirt, docker,
kube-proxy — i.e. most real servers), the `iptables-nft` shim reports a
not-yet-created chain with `chain '<name>' in table 'nat' is incompatible, use
'nft' tool` rather than the plain `No chain/target/match by that name`. Both
mean the chain is simply absent, so the reconciler treats either wording as
"absent → install"; `iptables-restore` then creates and manages the chain on
those hosts without trouble, and once it exists `-S <chain>` reads it back
normally. Treating the nft wording as a hard error would otherwise disable
interception on every such host.

## The reconciler

`Reconciler` keeps kernel netfilter state matching that canonical rule set. Its
`Start` performs the first install synchronously — so a successful return means
capture is live — and a failed first install is fatal to the daemon's startup,
because without the chain no traffic reaches the proxy. It then runs a 5-second
drift loop: each tick dumps the live chain (`iptables -t nat -S`), compares it
line-by-line against the canonical rules, and on drift (or first install)
re-applies the whole chain through `iptables-restore --noflush` — a single
atomic transaction that touches only `NEXUS_AGENT` and leaves firewalld's,
Docker's, and the user's chains intact. The `OUTPUT` hook is re-asserted
idempotently every tick. First install logs at info; later drift logs at warn
(a signal that something on the host interfered).

`Stop` tears the chain down idempotently in both families — unhook from
`OUTPUT`, flush, delete — best-effort so a partial or already-removed state does
not error. The systemd unit carries an `ExecStopPost` cleanup as a safety net
for the case where the daemon dies before its own teardown runs.

## Health reporting

The platform reports capture-layer health to the status collector, which turns a
degraded capture layer into a yellow tray icon and an "out of sync" node in the
Control Plane. The signal is driven entirely by the reconciler's ability to keep
the chain installed — **not** by flow counts. This is the key difference from the
macOS Network Extension, where zero IPC attaches means the user never approved
the proxy dialog and nothing is being captured. On Linux the redirect chain is
live the instant the reconciler installs it, so an enrolled host sitting idle
with zero flows is healthy, not degraded.

To express that, the Linux health snapshot sets a *self-reported* flag: the
status collector trusts the platform's own verdict instead of applying the
generic "zero connections means not connected" heuristic (which would mis-flag
every idle host). The reconciler tracks each tick's outcome — installed-yet,
consecutive-failure count, last error — and the platform maps it to a verdict:

- **not installed** (no successful tick yet) → degraded, "iptables redirect
  chain not installed".
- **persistently failing** (consecutive failed reconciles reach the threshold,
  ~10 s of sustained failure — lost `CAP_NET_ADMIN`, a broken iptables binary)
  → degraded, "iptables redirect chain repair failing", with the count and error.
- **otherwise** → healthy.

A routine firewalld/ufw flush is **not** degraded: the next tick re-applies the
chain successfully and resets the counter, so self-healing reloads never surface
as yellow. A single transient failure (xtables-lock contention) is likewise
tolerated below the threshold. Flow counters (cumulative connections, active
sessions, last-flow time) are still tracked and surfaced for the diagnostics
dashboard — they just do not gate the health verdict.

## SO_MARK loop avoidance

Every socket the agent opens for its own egress — the MITM upstream dialer and
all the agent's outbound HTTP clients (enrollment, relay, updater, the
thingclient HTTP fallback) — is stamped with `SO_MARK` `0x4e58` ("NX") via the
dialer's `Control` callback, before `connect`. The agent's HTTP transports pick
this up through a global dial-control hook the Linux build installs at startup.
The `NEXUS_AGENT` chain's first rule returns marked traffic, so the proxy's own
upstream connections are never caught by the redirect that would otherwise loop
them back into the proxy. The mark is identification, not a secret — it carries
no security property, only loop avoidance.

## Per-connection handling

Each redirected connection is accepted on a bounded worker pool (a 512-slot
semaphore backpressures the accept loop) and handled as follows:

- **Original destination.** `getsockopt(SO_ORIGINAL_DST)` on the redirected
  socket recovers the IP and port the client actually dialed (the listener only
  sees the redirect target otherwise). The handler branches on the accepted
  socket's family — `SOL_IP`/`SO_ORIGINAL_DST` for IPv4, and
  `SOL_IPV6`/`IP6T_SO_ORIGINAL_DST` for IPv6 flows that arrived on the `[::1]`
  listener.
- **Hostname.** `proxy.PeekSNI` peeks the TLS ClientHello to upgrade the
  destination from an IP literal to the SNI hostname and to capture the
  handshake bytes for replay; a flow with no SNI keeps the IP.
- **Process.** The owning PID is found by matching the socket's local/remote
  address in `/proc/net/tcp` (IPv4) or `/proc/net/tcp6` (IPv6) to a socket
  inode, then scanning `/proc/<pid>/fd` for that inode. That scan runs on every
  connection — a socket inode is unique per connection, so caching the inode→PID
  result can never hit and is deliberately not done. `/proc/<pid>/{exe,comm,status}`
  then give the executable path, name, and owning user; this per-PID metadata read
  is collapsed by a PID-keyed cache (`platform/pidcache`, 30s TTL), so a browser
  opening many connections from one process re-reads `/proc` once per process
  rather than once per connection.

The destination and process attribution form the `InterceptedConn` the policy
engine decides on.

## Applying the decision

- **deny** — the client socket is closed hard (`SetLinger(0)`).
- **passthrough** — dial the real upstream through the `SO_MARK`-stamped dialer
  (so the upstream connection escapes the redirect), replay the peeked
  ClientHello bytes, then relay both directions.
- **inspect** — hand the connection to `proxy.BumpFlow`, the shared TLS-bump
  engine the macOS and Windows paths, the compliance proxy, and the AI gateway
  all use (see
  [agent-forwarder-architecture.md](agent-forwarder-architecture.md)). It
  terminates TLS, runs the hook pipeline, and emits one audit row per HTTP
  request. If inspection is not possible — the device CA never loaded so the
  bridge dependencies are unwired, or the ClientHello peek failed on a
  non-TLS / server-speaks-first flow — the handler falls open to a plain relay
  and stamps `BUMP_FAILED_PASSTHROUGH`.

Passthrough and deny flows write one transport-level audit row (destination,
process, decision, byte counts, duration, and the agent's own intercept
overhead). Inspect flows write nothing here because `BumpFlow` already recorded
per-request rows.

## Egress proxy

By default the agent dials the real provider directly from the host. When
`upstreamProxy` is set in `agent.yaml` (`socks5://host:port`, `socks5h://`, or
`http://`), the MITM upstream's uTLS dial is tunnelled through that proxy
instead: the agent opens the connection to the proxy — still `SO_MARK`-stamped,
so the hop to the proxy escapes the `NEXUS_AGENT` redirect — issues a SOCKS5 or
HTTP `CONNECT` to the provider's host:port, and runs the uTLS handshake over the
tunnel. The proxy therefore only ever sees the `CONNECT` target host:port and
the re-encrypted TLS; the MITM-decrypted plaintext is re-encrypted to the
provider before the proxy hop, so the proxy is a trusted egress, not part of the
agent's security boundary. This lets a Linux agent forward intercepted AI
traffic out through a local egress / circumvention proxy where the host cannot
reach the provider directly, while still inspecting every flow.

`upstreamProxy` is a per-host boot setting (changing it needs a restart): the
working endpoint varies by host and network, so it lives in L1 yaml rather than
a Hub-pushed shadow key — fleet-wide uniform egress is out of scope here, and a
shadow-key form is the upgrade path if that demand appears. Empty / unset means
direct egress. An invalid value fails open to direct egress with an error log
rather than taking the host's AI traffic down; that log line is the operator's
signal that egress is misconfigured.

## Device CA and trust

The platform loads the device CA from the state directory and builds the TLS
engine that signs per-host leaf certificates for inspect flows. On a packaged
install the post-install script runs `nexus-agent install-ca` as root, which
generates and persists the CA under the state directory and installs it into the
OS trust store, so host clients trust the intercepted TLS. The trust-store step
auto-detects the distro layout rather than assuming Debian — it writes the cert
into the right anchor directory and runs that distro's refresh tool:
`/usr/local/share/ca-certificates` + `update-ca-certificates` on Debian/Ubuntu,
`/etc/pki/ca-trust/source/anchors` + `update-ca-trust` on RHEL/Fedora/Amazon
Linux, and the Arch / Alpine equivalents. (A `--skip-update` flag generates the
CA on disk without touching the trust store, for passthrough-only containers or
dry runs.) When the on-disk CA path is unreadable or unwritable — an
unprivileged dev run, or an install where the post-install step did not run as
root — the engine falls back to an ephemeral in-memory CA, and intercepted TLS
is untrusted until the CA is installed properly.

## Shutdown

`Stop` removes the iptables chain before closing the listener, so the kernel
stops redirecting new connections while in-flight ones still drain through the
open listener, then waits for the worker pool to finish under a timeout.

## References

- `packages/agent/internal/platform/linux/linux_linux.go` — the platform shim: listener, per-connection handler, `SO_ORIGINAL_DST`, `/proc` process resolution, decision application, device CA
- `packages/agent/internal/platform/linux/reconciler_linux.go` — the self-healing chain reconciler (canonical rules, drift detection, teardown, health snapshot)
- `packages/agent/internal/sync/status/status_health.go` — the status collector's self-reported-health branch that consumes the platform verdict
- `packages/agent/internal/platform/linux/iptables_linux.go` — the `iptables` / `ip6tables` wrappers (`iptables-restore`, dump, hook, remove)
- `packages/agent/internal/platform/linux/marker_linux.go` — `SO_MARK` stamping (`markControl`, `MarkedDialer`, `MarkedTransport`)
- `packages/agent/cmd/agent/platformshim/install_ca_linux.go` — the root-time `install-ca` command (load/generate CA, delegate trust-store install to catrust)
- `packages/agent/internal/platform/catrust/catrust_linux.go` — distro-aware OS trust-store install (Debian/RHEL/Arch/Alpine anchor dirs + refresh commands)
- `packages/agent/internal/network/proxy/proxy.go` — `PeekSNI` and `Relay`
- `packages/agent/internal/network/proxy/bridge.go` — `BumpFlow`, the entry into the shared TLS-bump pipeline
- `packages/httpclient` — the global dial-control hook the agent's HTTP clients consult for `SO_MARK`
