# syntax=docker/dockerfile:1
# nexus-db-migrate — deploy-utility image (NOT one of the four product images).
# Wraps tools/db-migrate so the schema push + prod seed can run as a Helm
# pre-install/pre-upgrade hook Job (Kubernetes can't volume-mount host source
# the way docker-compose.full.yml does). Mirrors the AMI first-boot-db step.
# Build context: repository root.
#   docker build -f tools/db-migrate/Dockerfile -t alphabitcore/nexus-db-migrate:test .

FROM node:20-alpine
WORKDIR /work
RUN apk add --no-cache ca-certificates
# Install deps against the tool's own manifest (it is a workspace member but
# self-contained for install: prisma + tsx + pg).
COPY tools/db-migrate/package.json ./package.json
RUN npm install --omit=dev --no-audit --no-fund
COPY tools/db-migrate/ ./
# Generate the Prisma client at build time so the hook Job only does
# push + seed at runtime. prisma.config.ts resolves env('DATABASE_URL') at
# load; generate does not connect, so a placeholder satisfies it.
RUN DATABASE_URL="postgresql://build:build@localhost:5432/build" npx prisma generate
# Default: schema push then production seed (no demo data). The Helm hook
# overrides command; this CMD makes `docker run` do the right thing too.
CMD ["sh", "-c", "npm run db:push -- --accept-data-loss && npm run seed:prod"]
