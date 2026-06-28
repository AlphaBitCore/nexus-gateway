// Package typology is the single canonical home for the three orthogonal
// axes that describe an HTTP traffic event in Nexus:
//
//   - EndpointKind — the semantic category of the request (chat,
//     embedding, tts, image_generation, …). Used by hook pipeline filtering,
//     routing rule matching, cost formula dispatch, traffic_event persistence,
//     and Prometheus labels.
//   - WireShape — the request body / response body wire format
//     (openai-chat, anthropic-messages, gemini-generate-content,
//     bedrock-converse, …). Used by codec selection in AI Gateway and
//     body extraction in Compliance Proxy + Agent.
//   - IngressPath (AIGW-internal, not exported from this package) — the
//     literal HTTP path used purely for HTTP handler dispatch.
//
// The three axes are independent. One request has exactly one value on
// each axis; the same EndpointKind can be served over different
// WireShapes (chat over openai-chat or anthropic-messages); the same
// WireShape can ride over different IngressPaths (openai-chat over
// /v1/chat/completions or /openai/deployments/.../chat/completions on
// Azure).
//
// Single canonical mapping function: [ClassifyPath]. Every callsite —
// AI Gateway dispatch, Compliance Proxy forward handler, Agent intercept
// handler, hook pipeline filter, audit persistence, routing rule matcher
// — calls this one function and reads the two typed values off the
// result. There is no second EndpointTypeFromPath function in the tree.
//
// See docs/developers/specs/e87-endpoint-typology-unification.md for the
// design of this package.
package typology
