module github.com/AlphaBitCore/nexus-gateway/packages/httpclient

go 1.26.0

// The whole point of this module is that its dependency closure is stdlib
// plus x/net. Anything more and the modules that adopted it for that reason
// — nexus-agent-core, the scenario suites — are back where they started.
require golang.org/x/net v0.57.0

require golang.org/x/text v0.40.0 // indirect
