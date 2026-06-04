// Package openapigen generates OpenAPI 3.1 specifications for the Nexus Control
// Plane admin API by statically analysing the control-plane Go source.
//
// It is a build/dev tool, not a runtime capability: it loads the control-plane
// packages with full type information (golang.org/x/tools/go/packages), walks
// the Echo route-registration tree reachable from a set of root registrars
// (default: RegisterAdminRoutes), and for every discovered route extracts the
// HTTP method, the URL path, the IAM action expression, the request body type
// (the argument to c.Bind), and the response body types (the arguments to
// c.JSON). Each Go type is rendered to a JSON Schema; routes are grouped by
// resource kind (the first path segment after the base prefix); and one
// OpenAPI 3.1 document is emitted per kind, plus an index catalog.
//
// The generator is deliberately structural, not semantic. Field names, types,
// optionality (pointer / omitempty), path/query parameters, response status
// codes and IAM tiers are all derivable from the AST and are emitted. Business
// constraints that the handlers enforce imperatively — enum value sets held in
// `validXxx` maps, "field is required" checks, cross-field rules — are NOT
// expressed in struct tags and therefore cannot be recovered structurally; they
// are left for the openapi-review skill to fill in by reading the handler logic.
// Anything the generator cannot resolve is recorded in the Report rather than
// silently dropped.
package openapigen
