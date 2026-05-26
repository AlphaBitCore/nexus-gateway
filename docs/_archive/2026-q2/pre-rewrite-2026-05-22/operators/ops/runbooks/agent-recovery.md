# Agent Recovery Runbook

Operator guide for recovering a Nexus Agent installation that is misbehaving, has lost its network connectivity, or has left intercept rules in a broken state.

---

## macOS NE path recovery

**Symptom:** Wi-Fi is connected but nothing works (DNS, HTTPS, browser, etc.) after installing or upgrading the Nexus Agent on a machine using the NE legacy build (`interceptMode="ne"`).

**Root cause:** The `NETransparentProxyProvider` system extension has claimed OS-level flows but is not relaying them. Every outbound connection stalls. Recovery requires stopping the daemon so the NE extension is deregistered.

**Recovery steps:**

```bash
# 1. Unload the daemon — this deregisters the NE extension
sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist

# 2. Verify network is restored (ping / curl)
curl -s https://example.com | head -5

# 3. If network is still broken after step 1:
sudo reboot
```

If the issue reproduces after a fresh install, capture the diagnostic bundle before remediation:

```bash
# Collect logs for engineering
sudo log show --predicate 'subsystem == "com.nexus.agent"' --last 1h > /tmp/nexus-agent.log
```

**Reference:** `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` §3.

---

## macOS pf path recovery (E74)

**Symptom:** The Nexus Agent daemon crashed, was force-killed, or was removed without running its cleanup sequence, and the pf anchor `nexus-agent/transparent` was not flushed. Outbound TCP 443 flows on the machine are being redirected to `127.0.0.1:13443` where nothing is listening — connections fail with "Connection refused" or stall.

**Root cause:** pf `rdr` rules remain in the `nexus-agent/transparent` anchor after the daemon exited without cleanup. Unlike the NE path, only the flows matching the `rdr` rules are affected (not all traffic), so the impact is scoped to TCP 443 (and optionally 80 if admin had enabled HTTP capture).

**Diagnostic command:**

```bash
# Inspect current anchor contents — shows which rdr rules are active
sudo pfctl -a nexus-agent/transparent -s rules
```

If the above command returns `rdr` rules and the daemon is not running, proceed with the flush below.

**Recovery commands:**

```bash
# Flush the anchor — removes all rdr rules; normal routing resumes immediately
sudo pfctl -a nexus-agent/transparent -F all

# Verify the anchor is empty
sudo pfctl -a nexus-agent/transparent -s rules
# Expected output: (empty) or "No ALTQ support in kernel"
```

If `pfctl -a nexus-agent/transparent -F all` returns an error stating the anchor does not exist, the rules are already cleared — no further action required.

**If the daemon is still registered with launchd but failing:**

```bash
# Stop the daemon (triggers cleanup-on-restart via DEC-009 path B)
sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist

# Flush the anchor as a precaution
sudo pfctl -a nexus-agent/transparent -F all

# Restart the daemon (installs fresh pf rules on startup)
sudo launchctl load /Library/LaunchDaemons/com.nexus-gateway.agent.plist
```

**Reference:** `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` §7 "pf path recovery".

---

## Linux recovery

**Symptom:** iptables NAT REDIRECT rules are present but the daemon is not running; TCP connections to port 443 fail.

```bash
# Check current REDIRECT rules
sudo iptables -t nat -L OUTPUT -v -n | grep nexus

# Flush nexus-specific rules (the daemon registers its chain on start; flush if absent daemon)
sudo iptables -t nat -D OUTPUT -p tcp --dport 443 -j REDIRECT --to-port 19080 2>/dev/null || true

# Restart the daemon
sudo systemctl restart nexus-agent
```

---

## Windows recovery

**Symptom:** WinDivert filter is active but the daemon is not running; connections hang.

```powershell
# Stop and restart the service (cleanup runs on service start)
Stop-Service NexusAgentSvc -Force
Start-Service NexusAgentSvc
```

If the service fails to start, uninstall and reinstall the agent via the `.msi` package.

---

## Escalation

If the above steps do not restore connectivity, collect the following before escalating to engineering:

1. macOS: `sudo log show --predicate 'subsystem == "com.nexus.agent"' --last 2h > /tmp/nexus-agent.log`
2. macOS pf: `sudo pfctl -a nexus-agent/transparent -s rules > /tmp/nexus-pf-rules.txt`
3. macOS NE: `systemextensionsctl list > /tmp/nexus-sysext.txt`
4. Agent version: check the About page in the Agent UI or `nexus-agent --version`.

Attach the above files to the support ticket.
