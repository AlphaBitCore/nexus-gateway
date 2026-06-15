.PHONY: build test lint bench vulncheck docker clean dev-certs run-dev

BINARY := bin/nexus-compliance-proxy
GO := go
GOFLAGS := -trimpath

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./cmd/compliance-proxy

test:
	$(GO) test ./... -v -race -count=1

bench:
	$(GO) test ./... -bench=. -benchmem -run=^$

lint:
	golangci-lint run ./...

vulncheck:
	govulncheck ./...

docker:
	docker build -t nexus-compliance-proxy .

clean:
	rm -rf bin/

# Generate a local ECDSA CA for compliance-proxy.dev.yaml (gitignored under dev-certs/).
# pathlen:0 — the proxy CA only ever signs leaf certs; the constraint stops a
# stolen CA key from minting an intermediate CA that devices would trust.
dev-certs:
	mkdir -p dev-certs
	openssl ecparam -name prime256v1 -genkey -noout -out dev-certs/ca.key
	openssl req -new -x509 -key dev-certs/ca.key -out dev-certs/ca.crt -days 365 \
		-subj "/O=Nexus Dev/CN=Nexus Compliance Proxy Dev CA" \
		-addext "basicConstraints=critical,CA:TRUE,pathlen:0"
	@echo "CA written to dev-certs/ca.crt and dev-certs/ca.key"

# Build and run with local dev config (from this directory).
run-dev: build
	./$(BINARY) -config compliance-proxy.dev.yaml
