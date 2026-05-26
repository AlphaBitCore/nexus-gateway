package canonicalbridge

import (
	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/canonicalext"
)

// GetExt returns the JSON value at nexus.ext.<provider>.<key> in body, or an
// empty [gjson.Result] when absent. Thin re-export of [canonicalext.Get] so
// the public bridge surface keeps a single import path for callers that do
// not want to depend on the internal codec helper package.
func GetExt(body []byte, provider, key string) gjson.Result {
	return canonicalext.Get(body, provider, key)
}

// SetExt writes value under nexus.ext.<provider>.<key> in body and returns
// the new body. Thin re-export of [canonicalext.Set]; codec packages call
// [canonicalext.Set] directly to avoid a cycle through this package.
func SetExt(body []byte, provider, key string, value any) ([]byte, error) {
	return canonicalext.Set(body, provider, key, value)
}
