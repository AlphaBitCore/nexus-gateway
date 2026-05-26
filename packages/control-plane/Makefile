.PHONY: build test lint bench vulncheck clean

BINARY := bin/nexus-control-plane
GO := go
GOFLAGS := -trimpath

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./cmd/control-plane

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
