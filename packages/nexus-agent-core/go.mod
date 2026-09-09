module github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core

go 1.26.0

toolchain go1.26.6

require (
	github.com/goccy/go-json v0.10.6
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

require github.com/AlphaBitCore/nexus-gateway/packages/httpclient v0.0.0

replace github.com/AlphaBitCore/nexus-gateway/packages/httpclient => ../httpclient
