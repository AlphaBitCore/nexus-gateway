# Backup & Disaster Recovery

## Overview

Nexus Gateway relies on three stateful systems: PostgreSQL, the Agent CA key pair, and NATS JetStream. This document defines backup strategies, RPO/RTO targets, and recovery procedures.

> **Operational target.** The repository ships **no automated backup
> scripts**, `archive_command` wiring, or cron jobs for the procedures
> below. This document describes the **contract the operations team is
> expected to fulfill** out-of-band (via S3 lifecycle policy + an external
> scheduler such as cron / systemd timer / EventBridge). The current
> production cadence is **manual `pg_dump` per release**, executed as part
> of the deploy runbook in
> `docs/operators/ops/runbooks/prod-deploy-data-changes.md` §0
> ("Pre-deploy backup"). Treat the RPO / pg_dump / WAL archiving sections
> below as the **future-state target**, not a description of what runs
> today. Operators implementing this runbook own choosing and wiring the
> scheduler.

---

## RPO / RTO Targets

| Component | RPO (Data Loss) | RTO (Recovery) | Justification |
|-----------|----------------|----------------|---------------|
| PostgreSQL | < 5 minutes | < 30 minutes | All config, audit, IAM data |
| Agent CA key | Zero (cannot regenerate) | < 15 minutes | Agents trust this CA; loss requires re-enrollment |
| Redis | N/A (pure cache) | < 5 minutes | Rebuilt on restart from DB |
| NATS JetStream | < 5 minutes | < 10 minutes | In-flight events; consumers replay from checkpoint |

---

## PostgreSQL Backup

### Continuous WAL Archiving (Recommended)

```bash
# postgresql.conf
archive_mode = on
archive_command = 'aws s3 cp %p s3://nexus-backups/wal/%f'
wal_level = replica
```

### Periodic pg_dump (Minimum)

```bash
# Daily logical backup
pg_dump -Fc -U nexus_app nexus_gateway | \
  aws s3 cp - s3://nexus-backups/daily/nexus-$(date +%Y%m%d).dump

# Retention: 30 days
aws s3 ls s3://nexus-backups/daily/ | \
  awk '{print $4}' | sort | head -n -30 | \
  xargs -I{} aws s3 rm s3://nexus-backups/daily/{}
```

### Point-in-Time Recovery

```bash
# 1. Stop all services
# 2. Restore base backup
pg_restore -d nexus_gateway /path/to/base.dump
# 3. Apply WAL logs up to target time
# 4. Restart services
```

---

## Agent CA Key Backup

The Agent CA key (`ca-key.pem`) is **critical** — loss requires re-enrollment of all agents.

### Backup Procedure

```bash
# Encrypt and store CA key pair
tar czf - .agent-ca/ | \
  gpg --symmetric --cipher-algo AES256 | \
  aws s3 cp - s3://nexus-backups/ca/agent-ca-$(date +%Y%m%d).tar.gz.gpg
```

### Kubernetes Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: nexus-agent-ca
  namespace: nexus
type: Opaque
data:
  ca-cert.pem: <base64>
  ca-key.pem: <base64>
```

Restrict access with RBAC:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-ca-reader
  namespace: nexus
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["nexus-agent-ca"]
  verbs: ["get"]
```

### HSM (Production Recommendation)

For production, store the CA private key in an HSM or cloud KMS:
- **AWS**: CloudHSM or KMS with custom key store
- **Azure**: Managed HSM or Key Vault Premium
- **GCP**: Cloud HSM
- **HashiCorp Vault**: Transit secrets engine with `sign` capability

---

## Credential Encryption Key Backup

The `CREDENTIAL_ENCRYPTION_KEY` encrypts provider API keys in the database. Loss means all stored credentials become unrecoverable.

- Store in a secrets manager (AWS Secrets Manager, Vault, etc.)
- Never commit to source control
- Rotation: use the `/api/admin/credentials/rotate-key` endpoint to re-encrypt

---

## NATS JetStream

JetStream stores in-flight events on disk. In a failure:

1. Events already consumed and written to PostgreSQL are safe
2. Unconsumed events in the stream are replayed automatically on reconnect
3. If NATS data is lost, consumers resume from their last DB-persisted checkpoint

### Backup

```bash
# NATS data directory backup (typically /data in containers)
tar czf nats-backup-$(date +%Y%m%d).tar.gz /data/jetstream/
```

### Recovery

Replace the NATS data directory and restart. Consumers will resume from their stored offsets.

---

## Redis

Redis is a **pure cache** — no backup needed. All cached data is rebuilt from PostgreSQL on service restart:
- Sessions: users re-login
- IAM cache: rebuilt from DB policies
- Rate limit counters: reset (brief window of relaxed limits)
- Desired state cache: rebuilt from Thing shadow table

---

## Disaster Recovery Runbook

### Scenario 1: PostgreSQL Data Loss

1. Provision new PostgreSQL instance
2. Restore from latest backup (`pg_restore` or WAL replay)
3. Run `npx prisma migrate deploy` to ensure schema is current
4. Restart all services (they reconnect automatically)
5. Verify: `curl http://hub:3060/readyz`

### Scenario 2: Agent CA Key Loss

1. Generate new CA: Hub auto-generates on startup if files missing
2. **All agents must re-enroll**: old device tokens become invalid
3. Re-create enrollment tokens via admin UI
4. Trigger agent re-enrollment (requires agent update or manual intervention)

### Scenario 3: Complete Infrastructure Loss

1. Provision PostgreSQL, Redis, NATS
2. Restore PostgreSQL from backup
3. Restore Agent CA key pair from encrypted backup
4. Set all environment variables (encryption keys, HMAC secrets, tokens)
5. Start services in order: Hub → CP → AG → Proxy → UI
6. Verify health endpoints
7. Agents auto-reconnect via WebSocket (if Hub URL unchanged)

---

## Backup Schedule Summary

| Item | Method | Frequency | Retention |
|------|--------|-----------|-----------|
| PostgreSQL | WAL archiving + pg_dump | Continuous + daily | 30 days |
| Agent CA key | Encrypted archive | On creation + after rotation | Permanent |
| Credential encryption key | Secrets manager | On creation + after rotation | Permanent |
| NATS JetStream | Filesystem backup | Daily | 7 days |
| Redis | None (pure cache) | — | — |
