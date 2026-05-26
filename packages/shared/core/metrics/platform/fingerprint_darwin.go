//go:build darwin

package platform

import (
	"bytes"
)

// hardwareUUID returns IOPlatformUUID parsed out of ioreg output. This
// is the value Apple guarantees unique per physical Mac; it survives OS
// reinstall but is reset by `nvram -c` (rare). Empty on shell-out
// failure (sandboxed runtime, ioreg missing).
func hardwareUUID() string {
	out, err := runShellOnceBytes("ioreg", "-c", "IOPlatformExpertDevice", "-d", "2")
	if err != nil {
		return ""
	}
	return extractIORegStringValue(out, "IOPlatformUUID")
}

// hardwareSerial returns IOPlatformSerialNumber. Equivalent stability to
// the UUID, embossed on the device chassis.
func hardwareSerial() string {
	out, err := runShellOnceBytes("ioreg", "-c", "IOPlatformExpertDevice", "-d", "2")
	if err != nil {
		return ""
	}
	return extractIORegStringValue(out, "IOPlatformSerialNumber")
}

// hardwareModel returns the Apple model identifier from sysctl —
// e.g. "MacBookPro18,2" or "Mac15,3". Empty on shell-out failure.
func hardwareModel() string {
	out, err := runShellOnce("sysctl", "-n", "hw.model")
	if err != nil {
		return ""
	}
	return out
}

// cpuBrandString returns the friendly CPU model name from sysctl. On
// Apple Silicon this is something like "Apple M2 Pro"; on Intel Macs
// it includes the full SKU string. Empty on shell-out failure.
func cpuBrandString() string {
	out, err := runShellOnce("sysctl", "-n", "machdep.cpu.brand_string")
	if err != nil {
		return ""
	}
	return out
}

// runShellOnceBytes is the []byte-returning sibling of runShellOnce.
// ioreg output is multi-line key=value with quoted strings; bytes.Index
// scanning is materially simpler than line-by-line string parsing.
func runShellOnceBytes(name string, args ...string) ([]byte, error) {
	// Re-use the existing ctx + timeout from runShellOnce by going
	// through the same exec path — wrapped here so the parsing code
	// reads naturally as a single helper.
	s, err := runShellOnce(name, args...)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// extractIORegStringValue parses lines like:
//
//	"IOPlatformUUID" = "12345678-90AB-CDEF-1234-567890ABCDEF"
//
// Returns the inner quoted string. Empty when the key is not present or
// the line layout doesn't match.
func extractIORegStringValue(data []byte, key string) string {
	needle := []byte(`"` + key + `"`)
	idx := bytes.Index(data, needle)
	if idx < 0 {
		return ""
	}
	rest := data[idx+len(needle):]
	eqIdx := bytes.IndexByte(rest, '=')
	if eqIdx < 0 {
		return ""
	}
	rest = rest[eqIdx+1:]
	q1 := bytes.IndexByte(rest, '"')
	if q1 < 0 {
		return ""
	}
	rest = rest[q1+1:]
	q2 := bytes.IndexByte(rest, '"')
	if q2 < 0 {
		return ""
	}
	return string(rest[:q2])
}
