// E74-S7 gap-closure test harness.
// Standalone module — intentionally NOT in go.work so the pf tests
// don't affect `go build ./...` from repo root and can be run on macOS
// developer machines without Linux CI runners needing darwin build tags.
module github.com/AlphaBitCore/nexus-gateway/tests/agent/gap_closure

go 1.25.0

require github.com/jackc/pgx/v5 v5.7.2

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)
