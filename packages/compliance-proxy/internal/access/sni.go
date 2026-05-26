package access

import "strings"

// CheckSNI verifies that the TLS ClientHello SNI matches the CONNECT target host.
// Comparison is case-insensitive with trailing dot normalization.
func CheckSNI(sni, connectHost string) error {
	if normalize(sni) != normalize(connectHost) {
		return ErrSNIMismatch
	}
	return nil
}

// normalize lowercases and strips a trailing dot (FQDN form).
func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimRight(s, ".")
	return s
}
