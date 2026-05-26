# E61 Valkey Migration Runbook

> Runbook version: 1.0  
> Epic: E61 (Smart Response Cache — Semantic L2)  
> SDD: `docs/developers/specs/e61-s3-valkey-switch-and-l2-write.md` §T1  
> Target: Single-EC2 production instance (taskforce10x.com)  
> Estimated downtime: ~30–60 seconds  
> Risk: Low — Valkey 8 is RDB-compatible with Redis 7; existing data migrates automatically

---

## Background

E61 introduces a semantic (vector) response cache backed by the
[valkey-search](https://github.com/valkey-io/valkey-search) module.  The
module exposes `FT.CREATE` / `FT.SEARCH` / `FT.DROPINDEX` commands (a
RediSearch-compatible API) for HNSW vector indexing.

The development environment was already switched in `docker-compose.yml` (E61-S3
2026-05-20).  This runbook covers the production cutover.

### Why valkey-bundle?

The official Valkey project publishes `valkey/valkey-bundle` on Docker Hub.  It
ships Valkey 8.x with the valkey-search module pre-installed in
`/usr/lib/valkey/libsearch.so`.  The container entrypoint auto-discovers every
`.so` in that directory and loads them via `--loadmodule` flags at startup.  No
custom Dockerfile or manual module path is required.

License: Valkey core + valkey-search are **BSD 3-Clause** — compatible with the
project's OSS-readiness requirement (C-7).  `redis-stack` (SSPL) was explicitly
rejected for this reason.

---

## 1. Preflight checks

Run these on the production host **before** scheduling the maintenance window.

```bash
# 1a. Confirm the running Redis version.
docker exec nexus-redis redis-cli INFO server | grep redis_version
# Expected: redis_version:7.x.y

# 1b. Check current key count (estimate restore size).
docker exec nexus-redis redis-cli DBSIZE
# Typical: <50 k keys for sessions, IAM cache, rate-limit counters.

# 1c. Check memory footprint.
docker exec nexus-redis redis-cli INFO memory | grep used_memory_human

# 1d. Estimate incoming traffic rate (requests/sec) from Prometheus.
#     Metric: nexus_aigw_requests_total (rate over last 5 minutes).
#     Plan maintenance during a low-traffic window (00:00–05:00 UTC recommended).

# 1e. Verify no active BGSAVE is running.
docker exec nexus-redis redis-cli LASTSAVE
# Note the timestamp; a second LASTSAVE call 10s later should match
# (no background save in progress).

# 1f. Check container names match the docker-compose config.
docker ps --format '{{.Names}}' | grep nexus
# Must include: nexus-redis, nexus-postgres, nexus-nats
```

---

## 2. Pre-cutover snapshot

Take a Redis RDB snapshot immediately before the cutover.  This is the rollback
data source.

```bash
# 2a. Trigger a foreground save (blocks ~1s for typical key counts).
docker exec nexus-redis redis-cli BGSAVE

# 2b. Wait for save to complete.
until [ "$(docker exec nexus-redis redis-cli LASTSAVE)" != "$(docker exec nexus-redis redis-cli LASTSAVE)" ]; do
  sleep 1
done
# Alternatively: docker exec nexus-redis redis-cli BGSAVE && sleep 5

# 2c. Copy the RDB file out of the container to the host.
SNAP_DIR="/opt/nexus/backups/redis-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$SNAP_DIR"
docker cp nexus-redis:/data/dump.rdb "$SNAP_DIR/dump.rdb"
echo "Snapshot saved to $SNAP_DIR/dump.rdb"
# Verify: ls -lh "$SNAP_DIR/dump.rdb"
```

> **Note on RDB compatibility**: Valkey 8 reads Redis 7 RDB files natively.
> The volume-mounted RDB is shared between the old and new containers, so
> no explicit import step is needed.  Valkey loads the file on startup.

---

## 3. Stop gateway services

Put the AI Gateway (and companion services) into a brief maintenance window so
no new writes occur while the store is being swapped.

```bash
# 3a. Stop in reverse-dependency order.
#     Keep nexus-hub running last — it holds the Hub shadow state.
#     Order: ai-gateway → compliance-proxy → control-plane → nexus-hub
systemctl stop nexus-aigw
systemctl stop nexus-compliance-proxy
systemctl stop nexus-control-plane
# Pause here: verify no active connections on port 3050 (AI Gateway).
ss -tlnp | grep 3050   # should show nothing
```

---

## 4. Stop the Redis container

```bash
docker stop nexus-redis
docker rm nexus-redis
```

The Redis volume (`/opt/nexus/data/redis/` or the Docker named volume `redisdata`)
is preserved.  Do NOT `docker volume rm`.

---

## 5. Start the Valkey container

Pull and start the `valkey/valkey-bundle:8-trixie` image.  Mount the **same
data volume** that Redis used so Valkey loads the existing RDB on startup.

```bash
# 5a. Pull the image (do this before the maintenance window to save time).
docker pull valkey/valkey-bundle:8-trixie

# 5b. Start Valkey using the same data volume.
#     Adjust the volume mount path if your setup differs from the example below.
docker run -d \
  --name nexus-valkey \
  --network nexus_default \
  -p 6437:6379 \
  -e TZ=UTC \
  -v nexus_redisdata:/data \
  --restart unless-stopped \
  valkey/valkey-bundle:8-trixie

# If using docker compose (recommended — matches the updated docker-compose.yml):
docker compose up -d valkey
```

---

## 6. Verify valkey-search is loaded

```bash
# 6a. Wait for Valkey to be ready.
until docker exec nexus-valkey valkey-cli ping 2>/dev/null | grep -q PONG; do
  echo "waiting for valkey..." && sleep 1
done

# 6b. Confirm the search module is loaded.
docker exec nexus-valkey valkey-cli MODULE LIST
# Expected output includes a line with name=search.
# Example:
#   1) 1) "name"
#      2) "search"
#      3) "ver"
#      4) 20200  (version varies)

# 6c. Verify existing data was loaded from RDB.
docker exec nexus-valkey valkey-cli DBSIZE
# Must match the value recorded in preflight step 1b.

# 6d. Spot-check a session key (optional).
docker exec nexus-valkey valkey-cli --scan --pattern 'nexus:session:*' | head -3
```

---

## 7. Update service ENV and restart services

If the container name changed (`nexus-redis` → `nexus-valkey`), verify that
service ENV variables still resolve to the correct host.  In the single-EC2
setup, services connect via `NEXUS_REDIS_HOST`.

```bash
# 7a. Check current REDIS host in systemd units.
grep NEXUS_REDIS_HOST /etc/systemd/system/nexus-*.service
# If the value is "nexus-redis" (Docker hostname), it must now be "nexus-valkey"
# OR the Docker network alias must be updated.

# 7b. Option A: Update the ENV variable in each systemd EnvironmentFile.
#     Edit /etc/nexus/nexus-aigw.env (and the other three services):
#       NEXUS_REDIS_HOST=nexus-valkey
#     Then reload and restart.
systemctl daemon-reload

# 7c. Option B: Add a Docker network alias so the old hostname still resolves.
#     (Only if changing ENV across 4 service files is higher risk.)
docker network connect nexus_default nexus-valkey --alias nexus-redis
# This lets services configured with NEXUS_REDIS_HOST=nexus-redis keep working.

# 7d. Restart services in dependency order.
systemctl start nexus-hub
sleep 3
systemctl start nexus-control-plane
sleep 3
systemctl start nexus-aigw
sleep 3
systemctl start nexus-compliance-proxy
```

---

## 8. Verification

### 8a. Run a smoke test

```bash
cd /opt/nexus/app
python3 tests/scripts/smoke-gateway.py \
  --target prod \
  --models gpt-4o \
  --no-all-ingress  # single-model, non-stream + 2-turn cache check
# Expected: PASS for L1 (extract) cache round-trip.
```

### 8b. Verify FT.CREATE / FT.SEARCH / FT.DROPINDEX manually

```bash
# Create a test index.
docker exec nexus-valkey valkey-cli \
  FT.CREATE testidx ON HASH PREFIX 1 testidx: \
  SCHEMA vec VECTOR HNSW 6 DIM 4 TYPE FLOAT32 DISTANCE_METRIC COSINE

# Confirm creation.
docker exec nexus-valkey valkey-cli FT.INFO testidx

# Clean up.
docker exec nexus-valkey valkey-cli FT.DROPINDEX testidx
# Expected: OK
```

### 8c. Verify the semantic cache index (if E61-S3 write path is deployed)

```bash
# The ai-gateway creates the fleet-wide semantic index on startup when
# semantic cache is enabled.  Check for it:
docker exec nexus-valkey valkey-cli \
  FT.INFO "nexus:semantic-cache:v1" 2>/dev/null | head -5
# If semantic cache is not yet enabled (E61 not fully deployed), this
# returns an error — that is expected at this stage.
```

### 8d. Operational signals (replacing fabricated Prometheus checks)

The ai-gateway does NOT emit dedicated `nexus_aigw_redis_*` series. Verify
cutover health via:
- **Service logs** — `tail -f packages/ai-gateway/logs/ai-gateway.log` for
  `redis` / `valkey` / `connection refused` lines. None expected.
- **Existing cache metrics** — `nexus_aigw_cache_lookups_total{result="hit"}`
  (extract cache) and `nexus_aigw_gemini_cache_*_total` should resume at
  pre-cutover rates within ~10 s.
- **Direct Valkey health** — `docker exec nexus-valkey valkey-cli PING`
  must return `PONG`.

---

## 9. Rollback procedure

If Valkey fails to start, shows module-load errors, or services cannot connect:

```bash
# 9a. Stop the Valkey container.
docker stop nexus-valkey && docker rm nexus-valkey

# 9b. Restore the Redis 7 container with the original data volume.
docker run -d \
  --name nexus-redis \
  --network nexus_default \
  -p 6437:6379 \
  -e TZ=UTC \
  -v nexus_redisdata:/data \
  --restart unless-stopped \
  redis:7-alpine

# 9c. Revert NEXUS_REDIS_HOST in EnvironmentFiles (if it was changed in step 7).

# 9d. Restart services.
systemctl daemon-reload
systemctl start nexus-hub nexus-control-plane nexus-aigw nexus-compliance-proxy

# 9e. Verify Redis is healthy.
docker exec nexus-redis redis-cli ping   # PONG
```

The RDB on disk was written by Redis 7 (step 2).  If any data was written to
Valkey between steps 5–9 before rollback, that delta is lost — acceptable given
the <60s maintenance window and the cache-only nature of the store (no
business-critical state, only sessions and cache entries that will regenerate).

---

## 10. Common pitfalls

| Pitfall | Detection | Fix |
|---|---|---|
| Services still pointing at `nexus-redis` hostname | `Connection refused` in aigw logs | Add Docker alias `--alias nexus-redis` or update ENV |
| valkey-search module not loaded | `MODULE LIST` returns empty | Confirm image is `valkey-bundle`, not plain `valkey/valkey` |
| NEXUS_REDIS_HOST set to `127.0.0.1` instead of container name | Aigw connects to host loopback, not container | Set host to container name or Docker bridge IP |
| Data volume not mounted | `DBSIZE` returns 0 after cutover | Verify `-v nexus_redisdata:/data` in run command |
| docker-compose version mismatch | Compose file uses `valkey:` service name but systemd start script uses `nexus-redis` | Update `docker-compose.yml` service name and aliases together |
| Module load order: MODULE LOAD vs startup flag | `MODULE LOAD` at runtime returns permission error on some images | Use `valkey-bundle` (loads at startup), never `MODULE LOAD` on a running server |

---

## 11. Operator alerts during cutover

Monitor the following during the ~60-second window. The ai-gateway does
not emit dedicated `nexus_aigw_redis_*` series; verify via the real
namespaces below + service logs + direct Valkey checks.

| Signal | Source | Threshold | Meaning |
|---|---|---|---|
| Service log Redis/Valkey errors | `tail -f packages/ai-gateway/logs/ai-gateway.log` | 0 after restart | Connection or command errors — check `NEXUS_REDIS_HOST` |
| `nexus_aigw_cache_lookups_total{result="hit"}` (rate) | Prometheus | Resumes at pre-cutover level within ~10 s | Extract cache is reachable |
| `nexus_aigw_gemini_cache_*_total` (rate) | Prometheus | Resumes at pre-cutover level within ~10 s | Gemini cache adapter is reachable |
| `process_start_time_seconds` for aigw | Prometheus | Bumps after `systemctl start` | Confirms process restarted |
| `go_goroutines` for aigw | Prometheus | Stabilizes within 30s | No goroutine leak from reconnect loop |
| `docker exec nexus-valkey valkey-cli PING` | Direct | `PONG` | Valkey is reachable; spike during reconnect is expected, must settle |
| `traffic_event` row count delta | `SELECT count(*) FROM traffic_event WHERE timestamp > now() - interval '1 minute'` | Returns to pre-cutover rate within ~30 s | End-to-end flow restored |

**MODULE LIST stale check (manual, post-cutover):**
Run once after services restart to confirm valkey-search module is still
present.  If it vanishes (e.g., after a container restart with the wrong
image), semantic-cache writes begin failing silently while L1 (extract)
keeps working.

```bash
docker exec nexus-valkey valkey-cli MODULE LIST | grep -i search
# Expected: "search" with a version number.
# If empty: the container restarted without the bundle image — pull and
#           restart nexus-valkey with valkey/valkey-bundle:8-trixie.
```

If service-log Redis/Valkey errors persist for >2 minutes after
`systemctl start nexus-aigw`, OR `traffic_event` row count fails to
recover, initiate rollback (§9).

---

## 12. Module load order note

The `valkey/valkey-bundle` container entrypoint auto-discovers modules at
startup from `/usr/lib/valkey/*.so`.  Do **not** attempt to load valkey-search
via `MODULE LOAD` on a running server — it requires `--enable-debug-command` and
may fail with a permission error on hardened images.  The correct and only
supported path is the bundle image with startup auto-discovery.

---

## 13. Revision history

| Date | Author | Change |
|---|---|---|
| 2026-05-20 | E61-S3a | Initial version — dev swap + prod runbook |
