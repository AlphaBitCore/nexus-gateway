# Operator documentation

Documentation for running Nexus Gateway in production.

## Contents

- [`ops/`](./ops/) — deployment and operations guides:
  - [`deployment.md`](./ops/deployment.md) — bring-up and topologies.
  - [`ami-build.md`](./ops/ami-build.md) — build the single-instance appliance AMI (AWS Marketplace).
  - [`ec2-single-node.md`](./ops/ec2-single-node.md) — a single-node deployment.
  - [`install-test-env.md`](./ops/install-test-env.md) — a single-host test or staging install.
  - [`backup-dr.md`](./ops/backup-dr.md) — backup and disaster recovery.
  - [`pki-and-certs.md`](./ops/pki-and-certs.md) — PKI and certificates.
  - [`monitoring.md`](./ops/monitoring.md) — monitoring.
  - [`compliance.md`](./ops/compliance.md) — compliance operations.
  - [`redis-setup.md`](./ops/redis-setup.md) — bringing up the cache.
- [`ops/runbooks/`](./ops/runbooks/) — step-by-step operational runbooks, including air-gapped deployment, agent recovery, and post-deploy smoke tests.

For the full documentation map, see [`docs/README.md`](../README.md).
