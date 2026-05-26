# Installing The Desktop Agent

The Desktop Agent intercepts AI traffic at the OS level — capturing requests from Cursor, Claude Code, browser AI plugins, and any other AI tool running on the endpoint without requiring proxy configuration per application. On macOS it uses a Network Extension (NETransparentProxyProvider) to claim outbound flows transparently. This page covers the macOS installation path, the enrollment ceremony, verification, and platform availability status.

---

## Platform status

| Platform | Status |
|---|---|
| **macOS (Apple Silicon + Intel)** | Primary. `.pkg` installer, Network Extension via `NETransparentProxyProvider`. |
| **Linux (x86_64, arm64)** | Experimental. systemd unit, iptables-based capture. |
| **Windows** | Roadmap. WFP-based capture is planned; no installer available yet. |

## macOS installation

### Prerequisites

- macOS 13 Ventura or later (Network Extension API requires it).
- Admin rights on the machine (`.pkg` install requires authorization).
- The Nexus Hub must be reachable from the machine (locally: `http://localhost:3060`).

### Download and install

1. Download the latest installer package from the canonical distribution URL: `https://nexus.taskforce10x.com/downloads/NexusAgent-latest.pkg`. For self-hosted deployments, the pkg is served from the same NGINX instance as the Control Plane; the exact URL depends on the deployment.

2. Double-click the downloaded `.pkg` file. The macOS installer wizard opens. Click through the license and destination screens, then click **Install**. Enter admin credentials when prompted.

3. The installer deploys the agent daemon to `/Library/Application Support/NexusAgent/` and installs a launchd plist at `/Library/LaunchDaemons/com.alphabitcore.nexus-agent.plist`. The daemon starts automatically after installation.

4. On macOS 13+, the system prompts the user to approve the Network Extension in **System Settings → Privacy & Security**. This approval is **required** for traffic interception. Without it the agent runs but cannot capture any flows. The system prompt appears automatically after the daemon starts; if it does not appear, open System Settings and look for the notification manually.

## Enrollment

The agent is installed but not yet enrolled with Hub. Enrollment binds the device to a specific org, issues an mTLS device cert, and activates config sync.

### Admin issues an enrollment token

1. In the Control Plane UI at `http://localhost:3000`, navigate to **Devices → Device Auth**.
2. Click **Issue Enrollment Token**.
3. Select the target organization, default role, and optionally constrain to `mac` OS.
4. Click **Issue**. The token plaintext is shown **once** — copy it immediately. Subsequent views show only metadata.

### End-user redeems the token

The agent's onboarding UI (accessible from the macOS menu bar icon) shows an **Enroll** button on first launch. Clicking it opens a dialog where the user pastes the token. The agent then:

1. Generates an ECDSA P-256 keypair locally and constructs a Certificate Signing Request.
2. POSTs the CSR and token to Hub at `POST /api/internal/things/enroll`.
3. Hub validates the token (single-use, expiry, OS constraint), signs the device cert with the Hub CA, inserts the `thing` row, and returns the signed cert plus a device token.
4. The agent stores the cert and device token, establishes an mTLS WebSocket to Hub, and begins pulling config (hook configs, interception domains, kill-switch state).

The enrollment token is single-use: a `redeemed` or `expired` token returns a 409 or 410. The admin re-issues from the same Devices surface.

## Verifying the agent is running

Check that the launchd job is loaded and running:

```bash
launchctl print system/com.alphabitcore.nexus-agent
```

The output should show `state = running`. If it shows `not found`, the package install may not have loaded the plist:

```bash
sudo launchctl load /Library/LaunchDaemons/com.alphabitcore.nexus-agent.plist
```

Read the agent log:

```bash
tail -f /var/log/nexus-agent/nexus-agent.log
```

A healthy agent produces lines like:

```
level=INFO msg="Hub WS established" thing_id=agt_... mTLS=true
level=INFO msg="config pulled" keys=17
level=INFO msg="NE proxy started" capture=true
```

## Verifying config sync

After enrollment the agent pulls its configuration from Hub's device shadow. Confirm the `thing` row is online and config is applied:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway \
  -c "SELECT type, status, display_name FROM thing WHERE type='Agent' ORDER BY created_at DESC LIMIT 5;"
```

In the Control Plane UI, **Infrastructure → Config Sync** shows the desired vs applied config for every node. A newly enrolled agent should show no drift within a few seconds of enrollment.

## Agent UI sign-in (optional)

The Desktop Agent ships a local UI (accessible from the menu bar). After enrollment the end-user can click **Sign in** to associate their identity with the agent. The sign-in triggers an OAuth + PKCE flow through the Control Plane — the same auth path as the admin UI. After sign-in, the Traffic page in the agent UI shows per-user activity.

## Fail-open guarantee

The macOS Network Extension is in the host's outbound packet path. A hang or crash in the NE would take down the machine's network. The NE is designed to fail-open: unknown flows are passed through without inspection, async daemon callbacks time out to passthrough (2 s default), and no hardcoded enforcement lists exist in the NE code. See [`agent-ne-fail-open-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md) for the full invariant set.

---

## Canonical docs

- [`agent-enrollment-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-enrollment-architecture.md) — cryptographic enrollment ceremony, dual-cert split, Hub CA
- [`agent-enrollment.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/agent-enrollment.md) — user-facing flow, failure modes, verification steps

**Adjacent wiki pages**: [Quickstart](Quickstart) · [Agent Overview](Agent-Overview) · [Agent Enrollment Attestation](Agent-Enrollment-Attestation) · [Agent macOS NE Architecture](Agent-macOS-NE-Architecture)
