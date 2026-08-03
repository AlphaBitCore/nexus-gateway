#!/usr/bin/env bash
set -euo pipefail

# ─── Nexus Gateway — Local Development Bootstrap ─────────────────────────────
# Checks prerequisites, starts Docker services, runs DB migrations, then
# optionally starts the Control Plane UI dev server.
#
# Usage:
#   ./scripts/dev-start.sh                       # bootstrap + start Control Plane UI (PRESERVES existing DB data)
#   ./scripts/dev-start.sh --no-dev              # bootstrap only; start services manually
#   ./scripts/dev-start.sh --force-reset         # DESTRUCTIVE: wipe DB + docker volumes, then bootstrap + start UI
#   ./scripts/dev-start.sh --force-reset --no-dev
#
# IMPORTANT: --force-reset wipes ALL local data including traffic_event,
# audit log, virtual keys, etc. Use it only when you genuinely want a
# clean slate (e.g. schema diverged beyond what `prisma db push` can
# reconcile without data loss). The default (no flag) runs `prisma db push`
# WITHOUT --force-reset, so additive schema changes apply but existing
# rows survive.

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${CYAN}[nexus]${NC} $1"; }
ok()   { echo -e "${GREEN}  ✔ $1${NC}"; }
warn() { echo -e "${YELLOW}  ⚠  $1${NC}"; }
err()  { echo -e "${RED}  ✖ $1${NC}"; exit 1; }

FORCE_RESET=false
NO_DEV=false
for arg in "$@"; do
  case "$arg" in
    --force-reset) FORCE_RESET=true ;;
    --reset)
      err "--reset has been renamed to --force-reset (the old name didn't make the destructive intent obvious). Re-run with --force-reset if you really want to wipe the local DB + docker volumes."
      ;;
    --no-dev) NO_DEV=true ;;
    *) err "Unknown argument: $arg" ;;
  esac
done

if $FORCE_RESET; then
  log "Force-reset mode: WILL WIPE the local Postgres / Valkey / NATS volumes + the entire nexus_gateway database before re-applying schema. All traffic_event, audit log, virtual keys, etc. will be lost."
fi
if $NO_DEV; then
  log "Bootstrap only (--no-dev): will not start dev servers automatically"
fi

# ─── 1. Check prerequisites ─────────────────────────────────────────────────

log "Checking prerequisites..."

command -v docker >/dev/null 2>&1 || err "Docker is not installed. Install it from https://docker.com"
command -v node >/dev/null 2>&1   || err "Node.js is not installed. Install v20+ from https://nodejs.org"
command -v npm >/dev/null 2>&1    || err "npm is not installed."
command -v go >/dev/null 2>&1     || err "Go is not installed. Install Go 1.25+ from https://go.dev/dl/"
command -v openssl >/dev/null 2>&1 || warn "openssl not found; the repo-root .env auto-bootstrap will fall back to a fixed dev encryption key"

NODE_VERSION=$(node -v | sed 's/v//' | cut -d. -f1)
if [[ "$NODE_VERSION" -lt 20 ]]; then
  err "Node.js v20+ required (found v$(node -v))"
fi

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
GO_MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
GO_MINOR=$(echo "$GO_VERSION" | cut -d. -f2)
if [[ "$GO_MAJOR" -lt 1 ]] || { [[ "$GO_MAJOR" -eq 1 ]] && [[ "$GO_MINOR" -lt 25 ]]; }; then
  warn "Go 1.25+ recommended for this repo (found go$GO_VERSION)"
fi

ok "Node.js $(node -v) | npm $(npm -v) | Go $(go version | awk '{print $3}') | Docker $(docker --version | awk '{print $3}' | tr -d ',')"

# ─── 1b. Bootstrap repo-root .env (service boot secrets) ────────────────────
# packages/shared/core/bootenv loads <repo-root>/.env into each Go binary at
# process start. Without it, Control Plane errors out at boot with
# "hubclient: INTERNAL_SERVICE_TOKEN is not set", and ai-gateway can't decrypt
# Credential rows. We copy .env.example over and substitute the CHANGE_ME_*
# placeholders with safe dev defaults so a fresh clone can `go run` every
# service without hand-editing secrets. Values chosen to match the
# corresponding dev fallbacks elsewhere in the repo:
#   ADMIN_KEY_HMAC_SECRET  → a per-developer random value (the CP
#                            fails closed on an empty secret and has NO committed
#                            fallback). Both the CP and the AI Gateway read the
#                            same env value, so VK lookups match.
#   INTERNAL_SERVICE_TOKEN → matches tests/.env.local.example NEXUS_HUB_SERVICE_TOKEN
#                            so the test harness and the services agree.
#   HUB_CONFIG_TOKEN       → the CP→Hub config-write / admin-alerts bearer
#                            A fixed dev value, [MUST MATCH] CP & Hub;
#                            distinct from INTERNAL_SERVICE_TOKEN so the dev stack
#                            exercises the split authority.
#   CREDENTIAL_ENCRYPTION_KEY → random 32-byte hex (openssl) or a fixed
#                               dev key if openssl is missing. Stable across
#                               restarts because it's persisted to .env.

cd "$ROOT_DIR"
log "Bootstrapping repo-root .env (service boot secrets)..."

if [[ -f .env ]]; then
  ok ".env exists (no changes — edit by hand if you need to rotate secrets)"
else
  if [[ ! -f .env.example ]]; then
    err ".env is missing and .env.example was not found at repo root"
  fi

  if command -v openssl >/dev/null 2>&1; then
    DEV_ENCRYPTION_KEY="$(openssl rand -hex 32)"
    # The HMAC secret must be a real per-developer random value, not a
    # committed constant — the CP now fails closed on an empty/missing secret
    # (ValidateHMACSecret), and a fixed string would let anyone recompute every
    # stored key_hash.
    DEV_HMAC_SECRET="$(openssl rand -hex 32)"
  else
    # openssl missing — derive real entropy from /dev/urandom (universally
    # present on macOS + Linux) rather than a committed constant.
    # the credential vault now rejects degenerate keys (<16 distinct bytes) at
    # boot, so a fixed example like 0123…cdef would fail the service closed.
    DEV_ENCRYPTION_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    DEV_HMAC_SECRET="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    warn "openssl missing — derived dev secrets from /dev/urandom (do NOT use in production)"
  fi

  # Single-pass sed (BSD + GNU compatible — no -i used; we write to a temp file
  # and atomically move it into place).
  sed \
    -e "s|^INTERNAL_SERVICE_TOKEN=.*|INTERNAL_SERVICE_TOKEN=dev-service-token|" \
    -e "s|^HUB_CONFIG_TOKEN=.*|HUB_CONFIG_TOKEN=dev-hub-config-token|" \
    -e "s|^ADMIN_KEY_HMAC_SECRET=.*|ADMIN_KEY_HMAC_SECRET=${DEV_HMAC_SECRET}|" \
    -e "s|^CREDENTIAL_ENCRYPTION_KEY=.*|CREDENTIAL_ENCRYPTION_KEY=${DEV_ENCRYPTION_KEY}|" \
    -e "s|^COMPLIANCE_PROXY_API_TOKEN=.*|COMPLIANCE_PROXY_API_TOKEN=dev-compliance-proxy-token|" \
    -e "s|^AI_GATEWAY_API_TOKEN=.*|AI_GATEWAY_API_TOKEN=dev-ai-gateway-token|" \
    -e "s|^NEXUS_ASSISTANT_SYSTEM_VK=.*|NEXUS_ASSISTANT_SYSTEM_VK=nvk_local_b0075000|" \
    .env.example > .env.tmp && mv .env.tmp .env
  # Wire the web assistant ("Chat with Nexus") to the seeded bootstrap
  # system-assistant virtual key so /assistant/chat works out of the box
  # (otherwise it returns 503). It routes via the enabled smart-auto-routing rule
  # once a provider key is set. The plaintext is the deterministic local default
  # from seed/bootstrap (assistantVkKey); the appliance mints a random one.
  ok "Created repo-root .env from .env.example with dev-default secrets"

  # Hard-fail if any CHANGE_ME_ placeholder survived on a real KEY=VALUE line
  # (comments mentioning CHANGE_ME_ in passing are fine). Catches a future
  # .env.example gaining a new placeholder we forgot to substitute here.
  if grep -E '^[A-Z][A-Z0-9_]*=CHANGE_ME_' .env >/dev/null 2>&1; then
    err ".env still contains CHANGE_ME_* placeholders after substitution — update scripts/dev-start.sh to cover the new variable"
  fi
fi

# ─── 2. Start Docker services ───────────────────────────────────────────────

log "Starting Docker services (PostgreSQL + Valkey + NATS)..."

cd "$ROOT_DIR"

if $FORCE_RESET; then
  docker compose down -v 2>/dev/null || true
fi

docker compose up -d

# Wait for PostgreSQL
log "Waiting for PostgreSQL..."
RETRIES=30
until docker compose exec -T postgres pg_isready -U postgres >/dev/null 2>&1; do
  RETRIES=$((RETRIES - 1))
  if [[ $RETRIES -le 0 ]]; then
    err "PostgreSQL failed to become ready after 30 seconds"
  fi
  sleep 1
done
ok "PostgreSQL ready (localhost:55532)"

# Wait for Valkey (Redis-wire-compatible).
# The docker-compose service is named `valkey` and the in-container CLI is
# `valkey-cli`; both still speak the Redis protocol so go-redis/v9 clients
# work unchanged. If you have an old container named `nexus-redis` left over
# from before the swap, `docker compose down -v` once to clean it up.
log "Waiting for Valkey (Redis-wire-compatible)..."
RETRIES=15
until docker compose exec -T valkey valkey-cli ping >/dev/null 2>&1; do
  RETRIES=$((RETRIES - 1))
  if [[ $RETRIES -le 0 ]]; then
    err "Valkey failed to become ready after 15 seconds"
  fi
  sleep 1
done
ok "Valkey ready (localhost:6437; speaks Redis protocol)"

# Wait for NATS — probe from inside the container (same pattern as postgres
# and redis above). The host-side `wget http://localhost:8222` path is flaky
# under Docker Desktop on macOS: the published port can accept TCP but never
# answer HTTP for tens of seconds, leaving wget (which has no timeout flag
# set here) hanging indefinitely. The container-internal probe is what the
# compose healthcheck already uses.
log "Waiting for NATS..."
RETRIES=15
until docker compose exec -T nats wget -q --spider http://localhost:8222/healthz >/dev/null 2>&1; do
  RETRIES=$((RETRIES - 1))
  if [[ $RETRIES -le 0 ]]; then
    warn "NATS failed to become ready (Hub consumers will not function without it)"
    break
  fi
  sleep 1
done
if [[ $RETRIES -gt 0 ]]; then
  ok "NATS JetStream ready (localhost:4222)"
fi

# ─── 3. Install npm dependencies ────────────────────────────────────────────

log "Installing npm dependencies..."
cd "$ROOT_DIR"
npm install --silent
ok "npm dependencies installed"

# ─── 4. Run Prisma migrations ───────────────────────────────────────────────

log "Running database migrations (tools/db-migrate)..."
cd "$ROOT_DIR/tools/db-migrate"

# Bootstrap .env from .env.example on first run. Prisma loads DATABASE_URL
# from .env via dotenv (see prisma.config.ts); without this, fresh clones
# fail with "DATABASE_URL is not defined". Also propagate the
# CREDENTIAL_ENCRYPTION_KEY from the repo-root .env so the seed script can
# redact provider credentials with the same key the runtime uses — seed.ts
# only reads tools/db-migrate/.env (it runs from this dir via tsx) so the
# repo-root .env is not visible without this step.
if [[ ! -f .env ]]; then
  if [[ -f .env.example ]]; then
    cp .env.example .env
    ok "Created tools/db-migrate/.env from .env.example (override locally if needed)"
  else
    err "tools/db-migrate/.env is missing and .env.example was not found"
  fi
fi

# Mirror the seed's required secrets from the repo-root .env into this
# directory's .env. The two-tier seed (tools/db-migrate/seed/seed.ts) re-stamps
# the demo tenant's credentials under the local dev keys:
#   CREDENTIAL_ENCRYPTION_KEY — AES-256 key to re-encrypt demo Credential ciphertext.
#   ADMIN_KEY_HMAC_SECRET     — HMAC secret to hash demo virtual-key / admin-key values.
# Both MUST match the running services' values. seed.ts runs from this dir via
# tsx and only reads tools/db-migrate/.env, so the repo-root .env is not visible
# without this step; without them the demo tier (Tier B) aborts fast.
ROOT_ENV="$ROOT_DIR/.env"
if [[ -f "$ROOT_ENV" ]]; then
  for KEY_NAME in CREDENTIAL_ENCRYPTION_KEY ADMIN_KEY_HMAC_SECRET; do
    ROOT_VAL=$(grep -E "^${KEY_NAME}=" "$ROOT_ENV" | head -1 | cut -d= -f2-)
    if [[ -n "$ROOT_VAL" ]]; then
      if grep -q "^${KEY_NAME}=" .env; then
        sed -e "s|^${KEY_NAME}=.*|${KEY_NAME}=${ROOT_VAL}|" .env > .env.tmp && mv .env.tmp .env
      else
        printf '%s=%s\n' "$KEY_NAME" "$ROOT_VAL" >> .env
      fi
      ok "Propagated ${KEY_NAME} from repo-root .env into tools/db-migrate/.env"
    fi
  done
fi

if $FORCE_RESET; then
  npx prisma db push --force-reset
  ok "Database wiped + schema re-applied (--force-reset)"
else
  # Default: apply schema additively. Existing rows (traffic_event,
  # virtual keys, etc.) survive. Run with --force-reset only when a
  # destructive reset is the explicit intent.
  npx prisma db push
  ok "Database schema pushed (data preserved — use --force-reset to wipe)"
fi

# ─── 4a-bis. Apply post-push schema extras `prisma db push` cannot express ────
# db push is declarative: it reconciles the DB to schema.prisma's model graph.
# PostgreSQL-native features (RANGE partitioning) have no Prisma representation
# and live in hand-written SQL at tools/db-migrate/schema-extras.sql, which db
# push ignores. Without re-applying it, metric_ops_raw stays a plain table and
# the Hub `ops-raw-partition` job fails every cycle with "metric_ops_raw is not
# partitioned (SQLSTATE 42P17)". The file is re-runnable and dev telemetry is
# disposable (dev-phase policy), so apply it unconditionally after every push.
#
# Applied through `prisma db execute`, which resolves the target from
# DATABASE_URL exactly like the `db push` above and like the container migrator
# (docker/db-migrator/entrypoint.sh). The previous `docker exec -i
# nexus-postgres psql -d nexus_gateway` form hardcoded a container name, a role
# and a database name, so it could not reach a database that was not the local
# compose container — and it discarded both streams and only warned, which is
# how a real abort stayed invisible. Never soften this back to a warning: the
# file is applied as one script and stops at its FIRST failing statement, so the
# correctness-bearing objects declared further down are simply never created
# while the bring-up still looks green. Observed once: a duplicate
# (type='agent', physical_id) pair blocked thing_type_physical_id_uniq and every
# statement after it, costing 29 indexes — see
# tools/db-migrate/manual-scripts/clear_duplicate_thing_physical_id_2026_08_03.sql.
EXTRAS_SQL="$ROOT_DIR/tools/db-migrate/schema-extras.sql"
if [[ ! -f "$EXTRAS_SQL" ]]; then
  err "schema-extras.sql not found at $EXTRAS_SQL — the PostgreSQL-native objects (metric_ops_raw partitioning, the partial unique indexes that keep agent identity and virtual-key names unique, the cache view + function) cannot be applied"
fi
if ! npx prisma db execute --file schema-extras.sql; then
  err "Applying schema-extras.sql failed — the error above names the first statement that aborted, and EVERY statement after it was skipped. Fix that statement's cause (a data conflict blocking a unique index is the usual one; see tools/db-migrate/manual-scripts/) and re-run, because the database is now missing PostgreSQL-native objects the services depend on"
fi
ok "Applied schema-extras.sql (metric_ops_raw → RANGE-partitioned, partial/expression indexes, cache view + function)"

# ─── 4b. Seed database ─────────────────────────────────────────────────────

log "Seeding database..."
npx prisma db seed
ok "Database seeded"

# ─── 4c. Compliance Proxy dev CA (TLS-bump cert issuer) ─────────────────────
# compliance-proxy.dev.yaml points the cert issuer at ./dev-certs/ca.{crt,key}
# (paths are relative to the package dir, so the binary expects to run from
# packages/compliance-proxy). Without this CA the proxy aborts at boot with
# "cert issuer: cert: read CA cert ./dev-certs/ca.crt: no such file or
# directory" — the same files served by `make dev-certs` in that package.

cd "$ROOT_DIR/packages/compliance-proxy"
if [[ -f dev-certs/ca.crt && -f dev-certs/ca.key ]]; then
  ok "Compliance Proxy dev CA already present (packages/compliance-proxy/dev-certs/)"
elif command -v openssl >/dev/null 2>&1; then
  mkdir -p dev-certs
  openssl ecparam -name prime256v1 -genkey -noout -out dev-certs/ca.key 2>/dev/null
  # pathlen:0 — the proxy CA only ever signs leaf certs; the constraint stops
  # a stolen CA key from minting an intermediate CA that devices would trust.
  # keyUsage is REQUIRED alongside a pathlen constraint: RFC 5280 says a CA that
  # asserts pathLenConstraint must assert keyCertSign, and strict verifiers
  # (OpenSSL 3, Python ssl) reject the cert outright with
  # "Path length given without key usage keyCertSign". Without it the CA works
  # in some clients and fails in others, which makes proxy testing look like a
  # code problem.
  openssl req -new -x509 -key dev-certs/ca.key -out dev-certs/ca.crt -days 365 \
    -subj "/O=Nexus Dev/CN=Nexus Compliance Proxy Dev CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
  ok "Generated Compliance Proxy dev CA (packages/compliance-proxy/dev-certs/{ca.crt,ca.key})"
else
  warn "openssl missing — skipping Compliance Proxy dev CA. Run 'make dev-certs' in packages/compliance-proxy/ before starting the proxy."
fi

# ─── 5. Startup guide ───────────────────────────────────────────────────────

cd "$ROOT_DIR"
echo ""
echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Bootstrap complete — start each service in a separate terminal:${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "  ${CYAN}Nexus Hub${NC} (port 3060):"
echo -e "    cd packages/nexus-hub && go run ./cmd/nexus-hub/ -config nexus-hub.dev.yaml"
echo ""
echo -e "  ${CYAN}Control Plane${NC} (port 3001):"
echo -e "    cd packages/control-plane && go run ./cmd/control-plane/ -config control-plane.dev.yaml"
echo ""
echo -e "  ${CYAN}Control Plane UI${NC} (port 3000):"
echo -e "    npm run dev:control-plane-ui"
echo ""
echo -e "  ${CYAN}AI Gateway${NC} (port 3050):"
echo -e "    cd packages/ai-gateway && go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml"
echo ""
echo -e "  ${CYAN}Compliance Proxy${NC} (proxy :3128, runtime API :3040):"
echo -e "    cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml"
echo ""
echo -e "  ${YELLOW}Tip:${NC} the default config flag is the prod-shape yaml; without"
echo -e "       ${YELLOW}-config <svc>.dev.yaml${NC} the binary fails fast on required dev fields."
echo ""
echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  Once the services are up — try it (seeded demo):${NC}"
echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
echo -e "  ${CYAN}Console:${NC} http://localhost:3000  —  log in: ${CYAN}admin@nexus.ai${NC} / ${CYAN}nexus-demo${NC}"
echo -e "  ${CYAN}First request:${NC} see examples/01-hello-world (demo VK ${CYAN}nvk_demo_0c101489${NC})."
echo ""
echo -e "  ${YELLOW}Add a provider API key${NC} before any request will succeed: the seed"
echo -e "       ships placeholder credentials, so calls return ${YELLOW}no available provider${NC}"
echo -e "       until you set a real key in the console — ${CYAN}Settings → Providers → <provider> → Add credential${NC}."
echo ""
echo -e "  ${YELLOW}Chat with Nexus${NC} (the in-console web assistant) is pre-wired to the"
echo -e "       system-assistant VK (NEXUS_ASSISTANT_SYSTEM_VK in .env); once a provider key is set it"
echo -e "       answers via smart-auto-routing. Production deployments must also set"
echo -e "       ${CYAN}NEXUS_ASSISTANT_PROD=1${NC} (hardened posture) — it is intentionally off in dev."
echo ""
echo -e "  ${YELLOW}Stop Docker:${NC} docker compose down"
echo ""

if $NO_DEV; then
  ok "Bootstrap complete. Start services manually using the commands above."
  exit 0
fi

# ─── 6. Start Control Plane UI ──────────────────────────────────────────────

log "Starting Control Plane UI (Ctrl+C to stop)..."
npm run dev:control-plane-ui
