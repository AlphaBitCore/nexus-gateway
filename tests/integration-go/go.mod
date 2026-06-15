// Phase 2 of the Nexus Gateway test program — Go integration tests that
// drive the running services over HTTP and verify state through pgx +
// Prometheus scrapes. Part of the go.work workspace; kept buildable standalone so
// these tests can be run / vendored independently of the production
// modules and so a broken integration test never breaks `go build ./...`
// from repo root.
module github.com/AlphaBitCore/nexus-gateway/tests/integration-go

go 1.25.0

require github.com/jackc/pgx/v5 v5.10.0

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.38.0 // indirect
)
