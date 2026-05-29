#!/bin/bash
# install-postgres.sh — install PostgreSQL 16 from AL2023's dnf and initialise
# an empty cluster. The data directory is populated by first-boot-db.sh.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

echo "==> [install-postgres] installing postgresql16-server..."
dnf install -y postgresql16-server postgresql16-contrib

echo "==> [install-postgres] enabling postgresql.service..."
systemctl enable postgresql

# IMPORTANT: postgres `initdb` is NOT run here. It happens at first-boot
# (see first-boot-db.sh). Reason: harden.sh wipes /var/lib/pgsql/data/*
# before the AMI snapshot — if we initdb'd at build time those files would
# be removed and postgresql.service would refuse to start on the launched
# instance with "data directory not initialized". Deferring initdb to
# first-boot avoids that whole class of bug AND keeps every launched
# instance's cluster identifier unique.

echo "==> [install-postgres] complete (initdb deferred to first-boot)."
