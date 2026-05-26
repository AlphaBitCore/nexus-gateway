// Package canonicalext exposes the nexus.ext.<provider>.<key> passthrough
// helpers shared by every per-provider SchemaCodec. Lives below the codecs
// so spec_anthropic / spec_gemini / canonicalbridge can all import it
// without creating a cycle (canonicalbridge already imports spec_anthropic
// and spec_gemini for hub_ingress wiring).
package canonicalext

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Get returns the JSON value at nexus.ext.<provider>.<key> in body, or an
// empty [gjson.Result] when absent. The result is unparsed; callers
// interpret .Str, .Int, .Raw, etc.
func Get(body []byte, provider, key string) gjson.Result {
	return gjson.GetBytes(body, "nexus.ext."+provider+"."+key)
}

// Set writes value under nexus.ext.<provider>.<key> in body and returns the
// new body. value must be JSON-marshalable per sjson rules.
func Set(body []byte, provider, key string, value any) ([]byte, error) {
	return sjson.SetBytes(body, "nexus.ext."+provider+"."+key, value)
}
