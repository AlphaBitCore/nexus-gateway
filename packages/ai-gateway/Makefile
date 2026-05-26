.PHONY: build test lint bench vulncheck clean

BINARY := bin/nexus-ai-gateway
GO := go
GOFLAGS := -trimpath

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./cmd/ai-gateway

test:
	$(GO) test ./... -v -race -count=1

bench:
	$(GO) test ./... -bench=. -benchmem -run=^$$

lint:
	golangci-lint run ./...

vulncheck:
	govulncheck ./...

clean:
	rm -rf bin/
