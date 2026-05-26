// Scenario-driven business-flow tests. See tests/scenarios/00-catalog.md
// for the API-endpoint → scenario coverage map, and
// docs/_archive/2026-q2/programs/test-scenarios-program-plan.md for the program plan.
//
// Kept as a standalone module (not in go.work) for the same reasons as
// tests/integration-go/: a broken scenario must not break repo-wide
// `go build`, and the test module pins its own pgx/SDK versions
// independent of the production services. Sibling helpers from
// integration-go/ are pulled in via a workspace-sibling replace.
module github.com/AlphaBitCore/nexus-gateway/tests/scenarios

go 1.25.0

require (
	github.com/AlphaBitCore/nexus-gateway/tests/integration-go v0.0.0
	github.com/jackc/pgx/v5 v5.9.1
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/AlphaBitCore/nexus-gateway/tests/integration-go => ../integration-go
