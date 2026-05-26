// Package aggregators implements the concrete Aggregator types registered
// with the alerteval Engine. Each rule maps to one file in this package
// (e.g. hook_reject_rate.go, vk_traffic_spike.go) and plugs into one of
// the four reusable evaluation helpers in helpers.go.
//
// Spec: alerteval-streaming-engine-design §5.5.
package aggregators

// intParam reads a numeric param from the rule.Params map. JSON numbers
// unmarshal into Go as float64; this helper accepts int / int64 / float64
// and falls back to def on any other type or missing key.
func intParam(p map[string]any, key string, def int) int {
	if p == nil {
		return def
	}
	switch v := p[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return def
	}
}

// floatParam reads a floating-point param from rule.Params, with similar
// type tolerance to intParam.
func floatParam(p map[string]any, key string, def float64) float64 {
	if p == nil {
		return def
	}
	switch v := p[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return def
	}
}

// stringParam reads a string param from rule.Params, falling back to def
// on missing key or wrong type.
func stringParam(p map[string]any, key, def string) string {
	if p == nil {
		return def
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	return def
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}

func derefFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func stringInSlice(s string, ss []string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
