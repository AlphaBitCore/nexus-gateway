package resource

// The generic resource tools embed the generated CP OpenAPI catalog so the
// distributed CLI needs no repo on disk. Keep the embedded copy under
// openapi/control-plane/ in sync with the source of truth at
// docs/users/api/openapi/control-plane/ by running `go generate ./capabilities/...`
// after regenerating the specs (go run ./cmd/openapi-gen … + the openapi-review
// enrichment). The TestResourceEmbedConsistent test guards _index ↔ per-kind
// file consistency within the embed.
//
//go:generate sh -c "rm -f openapi/control-plane/*.yaml && cp ../../../../docs/users/api/openapi/control-plane/*.yaml openapi/control-plane/"
