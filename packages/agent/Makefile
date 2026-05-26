.PHONY: build test lint bench vulncheck clean

BINARY := bin/nexus-agent
GO := go
GOFLAGS := -trimpath

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./cmd/agent

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
