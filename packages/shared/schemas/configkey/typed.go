package configkey

import (
	"github.com/goccy/go-json"
	"reflect"
)

// TypedRegistry maps Type A configKeys to the Go struct that backs
// their state JSON. Hub startup audit uses this to validate that
// schemas match what publishers/receivers expect.
//
// All entries currently use the raw json.RawMessage placeholder; typed
// struct mapping will be introduced per-key as receivers are migrated
// to shared typed structs in packages/shared/schemas/configtypes/.
var TypedRegistry = map[string]reflect.Type{
	// Killswitch: {enabled: bool} — wire schema is interception.Killswitch.
	Killswitch: reflect.TypeOf((*json.RawMessage)(nil)).Elem(), //nolint:prod-todos
	// LogLevel: {level: string}.
	LogLevel: reflect.TypeOf((*json.RawMessage)(nil)).Elem(), //nolint:prod-todos
	// Cache, AIGuard, GatewayPassthrough, AgentSettings, DiagMode, Onboarding,
	// PayloadCapture, Observability — all Type A with their own shapes.
	Cache:              reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	AIGuard:            reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	GatewayPassthrough: reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	AgentSettings:      reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	DiagMode:           reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	Onboarding:         reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	PayloadCapture:     reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	Observability:      reflect.TypeOf((*json.RawMessage)(nil)).Elem(),

	// Dual-tier response-cache keys.
	ResponseCacheTimeSensitivePatterns: reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	SemanticCacheConfig:                reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
	// Extract (L1 exact-match) cache fleet config singleton.
	ResponseCacheExtractConfig: reflect.TypeOf((*json.RawMessage)(nil)).Elem(),
}
