package core

// ToInt64 converts a JSON-decoded numeric value to int64.
// JSON numbers unmarshal to float64 by default when the target is any.
func ToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}
