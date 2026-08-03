#!/usr/bin/env bash
# init-secrets.sh — generate the secrets the stack needs and write .env.
#
# Five values must be identical across services, so they are generated once here
# rather than per-service. Nothing is defaulted: a public image shipping a known
# credential is found by scanners within hours.
#
# Idempotent: generates whatever is missing (the CA and/or .env), leaves
# whatever already exists untouched, and exits 0 either way — a rerun that
# only needs to backfill a missing CA (for example, an operator upgrading
# from before compliance-proxy's CA pair existed) must succeed, not fail on
# an unrelated ".env already exists" guard. Non-clobber discipline is still
# absolute: neither the CA nor .env is ever overwritten by a plain rerun.
# There are exactly two error cases, both of which refuse rather than
# overwrite: --force against an existing .env (see below), which surfaces as an
# explicit error rather than a silent no-op because the caller asked for
# regeneration by name; and a compliance-ca/ directory holding only one half of
# the CA pair, where generating would replace the surviving half.
set -euo pipefail

# Create .env unreadable to anyone else from the outset. Writing it first and
# chmod-ing afterwards leaves a window in which the freshly generated secrets
# are world-readable.
umask 077

cd "$(dirname "$0")"

FORCE=false
while [ $# -gt 0 ]; do
  case "$1" in
    --force) FORCE=true; shift ;;
    *) echo "ERROR: unknown flag $1" >&2; exit 1 ;;
  esac
done

command -v openssl >/dev/null 2>&1 || { echo "ERROR: openssl is required" >&2; exit 1; }

# compliance-proxy's MITM CA. Generated as its own idempotent step, before
# the .env handling below, so an operator who already has .env from before
# this pair existed can rerun this script and backfill just the CA — and,
# with the .env handling below now also idempotent, that rerun succeeds
# cleanly (exit 0) instead of the CA backfill being followed by an unrelated
# error. Same non-clobber discipline as .env: only generate if the pair is
# absent, so a rerun never invalidates a CA an operator's agents/clients
# already trust. Self-signed EC P-256, matching
# nexus-ami/scripts/first-boot-ca.sh's appliance CA — the maintainer's choice
# is to generate a usable CA out of the box; an operator who wants their own
# CA replaces compliance-ca/ca.{crt,key} before `up`.
CA_DIR="compliance-ca"
ca_generated=false
# compliance-proxy reads this pair as uid 65532 (docker/services/Dockerfile's
# distroless :nonroot stage), and on Linux a bind mount preserves host
# ownership — so the umask 077 above, which is right for .env, makes the
# compliance profile crash-loop on EACCES before it ever serves a request
# (restart: unless-stopped turns the startup failure into a loop). Permissions
# are therefore normalised explicitly, and normalised on EVERY run, not only on
# generation: an operator whose CA already exists with the old host-only modes
# needs the documented "rerun ./init-secrets.sh" to actually repair them.
normalise_ca_perms() {
  # The directory holds no secret of its own and must be traversable by the
  # container's uid, or the per-file modes below are irrelevant.
  chmod 755 "$CA_DIR"
  chmod 644 "$CA_DIR/ca.crt"
  # ca.key is the one that matters. Prefer handing it to the container's group
  # so it stays unreadable to other local users; if that needs privileges we do
  # not have, widen it but SAY SO — silently leaving it unreadable would ship a
  # crash-looping profile, and silently widening it would hide a private key
  # becoming world-readable.
  if chown 0:65532 "$CA_DIR/ca.key" 2>/dev/null; then
    chmod 640 "$CA_DIR/ca.key"
  else
    chmod 644 "$CA_DIR/ca.key"
    echo "  WARNING: could not give ca.key to gid 65532 (needs root), so it is mode 0644 —"
    echo "           readable by every local user on this host. compliance-proxy runs as"
    echo "           uid 65532 and must be able to read it. To harden:"
    echo "             sudo chown 0:65532 $CA_DIR/ca.key && sudo chmod 640 $CA_DIR/ca.key"
  fi
}

# Exactly one half present is neither "already provisioned" nor "not yet
# provisioned", and the generate branch below would silently resolve it the
# destructive way: `openssl req -x509` writes both files unconditionally, so a
# surviving ca.crt — one an operator may already have imported into a fleet's
# trust stores — would be replaced by a certificate for a brand-new key, with no
# backup and no way back. Refuse and let the operator decide.
if { [ -f "$CA_DIR/ca.crt" ] && [ ! -f "$CA_DIR/ca.key" ]; } \
  || { [ ! -f "$CA_DIR/ca.crt" ] && [ -f "$CA_DIR/ca.key" ]; }; then
  echo "ERROR: $CA_DIR holds only half of the CA pair — refusing to touch it." >&2
  echo "       ca.crt: $([ -f "$CA_DIR/ca.crt" ] && echo present || echo missing); ca.key: $([ -f "$CA_DIR/ca.key" ] && echo present || echo missing)" >&2
  echo "       Restore the missing file from wherever you keep it, or move the" >&2
  echo "       remaining one aside and rerun to generate a fresh pair — which" >&2
  echo "       every client that trusts the old certificate will have to re-import." >&2
  exit 1
fi

if [ -f "$CA_DIR/ca.crt" ] && [ -f "$CA_DIR/ca.key" ]; then
  echo "compliance-ca/ already present; leaving the key material as-is."
  normalise_ca_perms
  echo "  Permissions normalised for the compliance-proxy container (uid 65532)."
else
  echo "Generating compliance-proxy MITM CA (compliance-ca/)..."
  mkdir -p "$CA_DIR"
  openssl ecparam -genkey -name prime256v1 -noout -out "$CA_DIR/ca.key"
  openssl req -x509 -new -nodes -key "$CA_DIR/ca.key" -sha256 -days 3650 \
    -subj "/CN=Nexus Compliance Proxy CA/O=Nexus Gateway" \
    -out "$CA_DIR/ca.crt"
  normalise_ca_perms
  echo "  Wrote $CA_DIR/ca.crt + $CA_DIR/ca.key"
  echo "  Import $CA_DIR/ca.crt into any client that must trust the compliance proxy's MITM."
  ca_generated=true
fi
echo

env_generated=false
if [ -f .env ]; then
  if [ "$FORCE" = true ]; then
    echo "ERROR: --force was given but .env already exists — refusing to regenerate it." >&2
    echo "       Rotating CREDENTIAL_ENCRYPTION_KEY makes existing stored provider credentials unreadable." >&2
    echo "       Remove .env deliberately, then rerun (with or without --force), if you intend to regenerate secrets." >&2
    exit 1
  fi
  echo ".env already present; leaving it as-is."
else
  gen() { openssl rand -hex 32; }

  # The seed fixes the administrator identity as admin@nexus.ai and the
  # rotation below updates that row, so the email is not configurable here.
  ADMIN_EMAIL="admin@nexus.ai"
  ADMIN_PASSWORD="${ADMIN_PASSWORD:-$(openssl rand -base64 18)}"

  # Generated BEFORE the heredoc, not inside it. A `$(gen)` that fails while the
  # heredoc is being expanded contributes an empty string and nothing notices —
  # `cat` still exits 0 — so .env would be written with blank secrets and the
  # stack would come up with an empty ADMIN_KEY_HMAC_SECRET and no CREDENTIAL
  # encryption key. Assigning first puts each one under `set -e`.
  PG_PASSWORD="$(gen)"
  INTERNAL_TOKEN="$(gen)"
  HUB_TOKEN="$(gen)"
  ADMIN_HMAC="$(gen)"
  CRED_KEY="$(gen)"
  PROXY_TOKEN="$(gen)"
  # The assistant VK is generated as its random half so the emptiness check
  # below can see it: the "nvk_" prefix would otherwise make the composed value
  # non-empty no matter what openssl did. A degenerate one is not an exposure —
  # this branch only runs when .env is absent, i.e. a first install, where
  # mint-assistant-vk.js rejects it and the migrator exits non-zero before any
  # service starts — but it is an install that wedges with a confusing error,
  # which is worth failing here instead.
  ASSISTANT_VK_RANDOM="$(openssl rand -hex 24)"
  ASSISTANT_VK="nvk_${ASSISTANT_VK_RANDOM}"
  for v in "$PG_PASSWORD" "$INTERNAL_TOKEN" "$HUB_TOKEN" "$ADMIN_HMAC" "$CRED_KEY" "$PROXY_TOKEN" "$ASSISTANT_VK_RANDOM" "$ADMIN_PASSWORD"; do
    if [ -z "$v" ]; then
      echo "ERROR: secret generation produced an empty value — refusing to write .env." >&2
      exit 1
    fi
  done

  cat > .env <<EOF
# Generated by init-secrets.sh. Not committed; treat as a credential file.

POSTGRES_USER=nexus
POSTGRES_PASSWORD=${PG_PASSWORD}
POSTGRES_DB=nexus_gateway

INTERNAL_SERVICE_TOKEN=${INTERNAL_TOKEN}
HUB_CONFIG_TOKEN=${HUB_TOKEN}
ADMIN_KEY_HMAC_SECRET=${ADMIN_HMAC}
CREDENTIAL_ENCRYPTION_KEY=${CRED_KEY}
COMPLIANCE_PROXY_API_TOKEN=${PROXY_TOKEN}

NEXUS_ADMIN_PASSWORD=${ADMIN_PASSWORD}

# The system-assistant Virtual Key ("Chat with Nexus"). The seed ships a
# deterministic plaintext that is public in the OSS repository; this replaces
# it. Generated here rather than by the migrator because Compose reads .env
# before any container starts, so the control plane must already hold the value
# the migrator is about to stamp into the database.
NEXUS_ASSISTANT_SYSTEM_VK=${ASSISTANT_VK}

NEXUS_REGISTRY=ghcr.io/alphabitcore
NEXUS_VERSION=latest
EOF

  chmod 600 .env

  echo "Wrote .env (mode 600)."
  echo
  echo "  Admin sign-in: ${ADMIN_EMAIL}"
  echo "  Password:      ${ADMIN_PASSWORD}"
  echo
  echo "This password is shown once. It is also stored in .env."
  env_generated=true
fi

echo
if [ "$ca_generated" = false ] && [ "$env_generated" = false ]; then
  echo "Nothing to do — compliance-ca/ and .env were both already present."
else
  echo "Done."
fi
echo "Next: docker compose up -d"
