package store

import "fmt"

// ParseDecimal converts a Prisma Decimal string (via pgx) to float64.
// Returns the parsed value and true on success, or 0 and false if s is nil
// or cannot be parsed.
func ParseDecimal(s *string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	var f float64
	if _, err := fmt.Sscanf(*s, "%f", &f); err != nil {
		return 0, false
	}
	return f, true
}

// NilIfEmpty returns nil when s is empty, otherwise a pointer to s.
// Useful for passing optional string parameters to pgx queries.
func NilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
