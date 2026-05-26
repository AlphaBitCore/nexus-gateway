# Operations Backup Restore

Nexus Gateway has three stateful systems that require backup: PostgreSQL (all config, IAM policies, audit data, and traffic events), the Agent CA key pair (loss requires re-enrolling every managed device), and NATS JetStream (in-flight events). Valkey/Redis is a pure cache and requires no backup — it rebuilds from PostgreSQL on restart. The canonical procedures are in [`backup-dr.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/backup-dr.md); this page summarizes the RPO/RTO targets, backup procedures, and restore runbooks.

---

## Recovery targets

| Component | RPO (data loss tolerance) | RTO (recovery time target) | Notes |
|---|---|---|---|
| PostgreSQL | < 5 minutes | < 30 minutes | Config, audit, IAM, traffic events |
| Agent CA key | Zero | < 15 minutes | Loss forces re-enrollment of all agents |
| NATS JetStream | < 5 minutes | < 10 minutes | In-flight events; consumers replay from checkpoint |
| Valkey / Redis | N/A | < 5 minutes | Pure cache; rebuilt from DB on restart |

---

## PostgreSQL backup

### Continuous WAL archiving (recommended for production)

Configure PostgreSQL to archive WAL segments to S3 continuously. This achieves the < 5-minute RPO target.

```bash
# postgresql.conf additions
archive_mode = on
archive_command = 'aws s3 cp %p s3://nexus-backups/wal/%f'
wal_level = replica
```

### Periodic pg_dump (minimum baseline)

A daily logical backup provides a restore point even without WAL archiving:

```bash
# Daily backup — run from a cron job on the DB host
pg_dump -Fc -U nexus_app nexus_gateway | \
  aws s3 cp - s3://nexus-backups/daily/nexus-$(date +%Y%m%d).dump

# 30-day retention — prune older files
aws s3 ls s3://nexus-backups/daily/ | \
  awk '{print $4}' | sort | head -n -30 | \
  xargs -I{} aws s3 rm s3://nexus-backups/daily/{}
```

### Restore from pg_dump

```bash
# 1. Stop all Nexus Gateway services
systemctl stop nexus-compliance-proxy nexus-aigw nexus-control-plane nexus-hub

# 2. Restore the logical backup
pg_restore -d nexus_gateway /path/to/nexus-YYYYMMDD.dump

# 3. Apply any pending Prisma migrations
cd tools/db-migrate && npx prisma migrate deploy

# 4. Start services in dependency order
systemctl start nexus-hub
sleep 3
systemctl start nexus-control-plane nexus-aigw nexus-compliance-proxy

# 5. Verify all services registered
curl -s http://<hub-host>:3060/readyz
```

### Point-in-time recovery (with WAL archiving)

```bash
# 1. Restore the base backup
pg_restore -d nexus_gateway /path/to/base.dump

# 2. Configure recovery.conf or postgresql.conf recovery parameters
#    pointing at the WAL archive and the target time

# 3. Start PostgreSQL; it replays WAL up to the target time

# 4. Resume services as above (step 4 of pg_dump restore)
```

---

## Agent CA key backup

The Agent CA key is critical. Hub auto-generates a new CA on startup if the key files are missing, but all existing agents trust the original CA — losing it forces re-enrollment of every managed device.

### Backup procedure

```bash
# Encrypt the CA key pair with GPG and upload to S3
tar czf - .agent-ca/ | \
  gpg --symmetric --cipher-algo AES256 | \
  aws s3 cp - s3://nexus-backups/ca/agent-ca-$(date +%Y%m%d).tar.gz.gpg
```

Run this backup on CA creation and after every CA rotation. Store the GPG passphrase in a secrets manager (not in the same S3 bucket).

### Production recommendation: hardware security module

For production, store the CA private key in an HSM or cloud KMS:

- AWS CloudHSM or KMS with custom key store
- Azure Managed HSM or Key Vault Premium
- GCP Cloud HSM
- HashiCorp Vault with the Transit secrets engine (`sign` capability)

### Restore after CA loss

```bash
# 1. Download and decrypt the latest CA backup
aws s3 cp s3://nexus-backups/ca/agent-ca-YYYYMMDD.tar.gz.gpg /tmp/ca-backup.tar.gz.gpg
gpg --decrypt /tmp/ca-backup.tar.gz.gpg | tar xzf - -C /opt/nexus/

# 2. Restart Hub — it picks up the restored key on startup
systemctl restart nexus-hub

# 3. Verify agents reconnect automatically via WebSocket
# (If Hub URL is unchanged, agents reconnect within their next heartbeat cycle.)
```

If the CA is unrecoverable, Hub generates a new one and every device must re-enroll via the admin UI. Issue new enrollment tokens from the Control Plane UI at `/agents/enroll`.

---

## Credential encryption key backup

`CREDENTIAL_ENCRYPTION_KEY` encrypts all provider API keys stored in the `Credential` table. Loss makes every stored credential unrecoverable — the admin must re-enter every provider key.

Store `CREDENTIAL_ENCRYPTION_KEY` in a secrets manager (AWS Secrets Manager, HashiCorp Vault, or equivalent). Never commit it to source control or YAML.

See [Deployment Environment Variables](Deployment-Environment-Variables) for the full `[MUST MATCH]` contract — this key must be identical across the AI Gateway and Control Plane.

---

## NATS JetStream backup

JetStream stores in-flight events on disk. Events already consumed and written to PostgreSQL are durable; unconsumed events replay automatically on reconnect.

### Backup

```bash
# Back up the JetStream data directory (typically /data in the container)
tar czf nats-backup-$(date +%Y%m%d).tar.gz /data/jetstream/
```

### Restore

Replace the NATS data directory with the backup and restart NATS. Consumers resume from their stored offsets automatically. If the NATS data directory is lost entirely, consumers start from their last DB-persisted checkpoint — there may be a short gap in event history.

---

## Valkey / Redis

Valkey is a pure cache. No backup is required.

After a complete Valkey loss and restart, the following rebuild automatically:

- **Sessions**: users re-login through the OAuth+PKCE flow.
- **IAM cache**: rebuilt from the `iam_policy` table in PostgreSQL.
- **Rate-limit counters**: reset; there is a brief window of relaxed limits.
- **Desired-state cache**: rebuilt from the Hub shadow table.

---

## Full infrastructure restore (disaster recovery)

For a complete loss of the deployment host, follow this sequence:

1. Provision new infrastructure: PostgreSQL, Valkey, NATS, compute.
2. Restore PostgreSQL from the most recent backup (WAL replay to the RPO window, or pg_dump restore).
3. Run `npx prisma migrate deploy` from `tools/db-migrate/` to ensure schema is current.
4. Restore the Agent CA key pair from the encrypted backup.
5. Set all environment variables from `.env.example` — pay special attention to `[MUST MATCH]` pairs: `CREDENTIAL_ENCRYPTION_KEY`, `INTERNAL_SERVICE_TOKEN`, `ADMIN_KEY_HMAC_SECRET`.
6. Start services in order: Hub → Control Plane → AI Gateway → Compliance Proxy.
7. Verify health endpoints:

```bash
curl -s http://<hub-host>:3060/readyz
curl -s http://<cp-host>:3001/healthz
curl -s http://<aigw-host>:3050/healthz
curl -s http://<proxy-host>:3040/healthz
```

8. Agents auto-reconnect via WebSocket if the Hub URL is unchanged.

---

## Backup schedule summary

| Item | Method | Frequency | Retention |
|---|---|---|---|
| PostgreSQL | WAL archiving | Continuous | Rolling 30 days |
| PostgreSQL | `pg_dump` | Daily | 30 days |
| Agent CA key | GPG-encrypted archive | On creation + after rotation | Permanent |
| `CREDENTIAL_ENCRYPTION_KEY` | Secrets manager | On creation + after rotation | Permanent |
| NATS JetStream | Filesystem archive | Daily | 7 days |
| Valkey | None (pure cache) | — | — |

---

## Canonical docs

- [`backup-dr.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/backup-dr.md) — canonical backup and disaster recovery procedures
- [`credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — credential encryption key management details

**Adjacent wiki pages**: [Operations Runbook Index](Operations-Runbook-Index) · [Operations Credential Rotation](Operations-Credential-Rotation) · [Operations Migrations On Prod](Operations-Migrations-On-Prod) · [Deployment Environment Variables](Deployment-Environment-Variables) · [Credentials Storage](Credentials-Storage)
