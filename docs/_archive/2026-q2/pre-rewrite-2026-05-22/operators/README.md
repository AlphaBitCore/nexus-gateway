# Operators

Documentation for SREs and platform operators running the Nexus Gateway in
production.

## Sections

| Folder | What's there |
|---|---|
| [`ops/`](./ops/) | Deployment, runbooks, monitoring, PKI, Redis ops, backup/DR |

## Common starting points

- **First-time deployment** → `ops/deployment-*.md`
- **Incident** → `ops/runbooks/` (incident playbooks indexed by symptom)
- **Health monitoring** → `ops/monitoring*.md`
- **Backup / DR planning** → `ops/backup-dr*.md`

If you're touching infrastructure in code (deploy scripts, systemd units,
Docker), also check [`../developers/architecture/`](../developers/architecture/README.md)
for the architectural invariants behind it.
