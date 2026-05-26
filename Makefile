.PHONY: build test build-all test-all dev lint clean \
       nexus-hub-build nexus-hub-test \
       control-plane-build control-plane-test \
       control-plane-ui-build control-plane-ui-test control-plane-ui-dev \
       ai-gateway-build ai-gateway-test \
       compliance-proxy-build compliance-proxy-test \
       agent-build agent-test \
       agent-build-macos agent-package-macos agent-clean-macos \
       agent-build-windows agent-package-windows agent-clean-windows

# ── Build output convention ────────────────────────────────────────────
# All Go service binaries land in dist/bin/<service>/<binary> so they
# never pollute packages/<service>/ source trees. Platform-specific
# agent packages (.app, .pkg, .msi) still live under dist/{macos,windows}/
# — these are kept separate because they are produced by the platform
# build scripts, not by the plain `go build` targets here.

DIST_BIN := dist/bin

# ── Aggregate targets ──────────────────────────────────────────────────

# `make build` / `make test` are short aliases for the full sweep. Prefer
# these over `go build ./cmd/<svc>` from a package dir — those land the
# binary in the cwd and pollute the working tree (see .gitignore patterns
# for `packages/<svc>/<svc>`).
build: build-all

test: test-all

build-all: nexus-hub-build control-plane-build control-plane-ui-build ai-gateway-build compliance-proxy-build agent-build

test-all: nexus-hub-test control-plane-test control-plane-ui-test ai-gateway-test compliance-proxy-test agent-test

dev:
	npm run dev

lint: control-plane-ui-lint

clean:
	rm -rf $(DIST_BIN) packages/control-plane-ui/dist

# ── Nexus Hub (Go) ────────────────────────────────────────────────────

nexus-hub-build:
	cd packages/nexus-hub && go build -o ../../$(DIST_BIN)/nexus-hub/nexus-hub ./cmd/nexus-hub/

nexus-hub-test:
	cd packages/nexus-hub && go test -race -count=1 ./...

# ── Control Plane (Go + Echo) ────────────────────────────────────────

control-plane-build:
	cd packages/control-plane && go build -o ../../$(DIST_BIN)/control-plane/control-plane ./cmd/control-plane/

control-plane-test:
	cd packages/control-plane && go test -race -count=1 ./...

# ── Control Plane UI (React + Vite) ─────────────────────────────────

control-plane-ui-build:
	cd packages/control-plane-ui && npx tsc --noEmit && npx vite build

control-plane-ui-test:
	cd packages/control-plane-ui && npx vitest run

control-plane-ui-dev:
	npm run dev:control-plane-ui

control-plane-ui-lint:
	cd packages/control-plane-ui && npx eslint src/

# ── AI Gateway (Go) ─────────────────────────────────────────────────

ai-gateway-build:
	cd packages/ai-gateway && go build -o ../../$(DIST_BIN)/ai-gateway/ai-gateway ./cmd/ai-gateway/

ai-gateway-test:
	cd packages/ai-gateway && go test -race -count=1 ./...

# ── Compliance Proxy (Go) ───────────────────────────────────────────

compliance-proxy-build:
	cd packages/compliance-proxy && go build -o ../../$(DIST_BIN)/compliance-proxy/compliance-proxy ./cmd/compliance-proxy/

compliance-proxy-test:
	cd packages/compliance-proxy && go test -race -count=1 ./...

# ── Agent (Go) ────────────────────────────────────────────────────────
# Builds the CLI agent + the tray helper. The tray helper is Linux/
# Windows-only (cmd/agent-tray carries `//go:build linux || windows`);
# on macOS the menu-bar surface is provided by the native Swift app
# built via `agent-build-macos`, so we skip agent-tray on darwin.
# The .app / .pkg / .msi packaging targets below produce richer
# artifacts under dist/macos/ and dist/windows/.

agent-build:
	cd packages/agent && go build -o ../../$(DIST_BIN)/agent/agent ./cmd/agent/
	@if [ "$$(uname)" != "Darwin" ]; then \
	  cd packages/agent && go build -o ../../$(DIST_BIN)/agent/agent-tray ./cmd/agent-tray/; \
	else \
	  echo "  (skipping agent-tray: linux/windows only; macOS uses native NexusAgent.app)"; \
	fi

agent-test:
	cd packages/agent && go test -race -count=1 ./...

# ── macOS Agent Distribution (E23) ────────────────────────────────────

agent-build-macos:
	bash packages/agent/platform/darwin/Scripts/build.sh

agent-package-macos: agent-build-macos
	bash packages/agent/platform/darwin/Scripts/sign.sh
	bash packages/agent/platform/darwin/Scripts/package.sh
	bash packages/agent/platform/darwin/Scripts/notarize.sh

agent-clean-macos:
	rm -rf dist/macos

# ── Windows Agent Distribution (E23W) ─────────────────────────────────
# Requires PowerShell Core (`pwsh`). On Windows it's preinstalled; on
# macOS/Linux dev hosts install via Homebrew (`brew install powershell`)
# or apt. Real MSI compilation only works on a Windows host (or the
# windows-latest GitHub Actions runner) — see .github/workflows/ci.yml.

agent-build-windows:
	pwsh -NoProfile -File packages/agent/platform/windows/scripts/build.ps1

agent-package-windows: agent-build-windows
	pwsh -NoProfile -File packages/agent/platform/windows/scripts/sign.ps1
	pwsh -NoProfile -File packages/agent/platform/windows/scripts/package.ps1

agent-clean-windows:
	rm -rf dist/windows
