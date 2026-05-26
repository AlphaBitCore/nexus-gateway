package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"runtime"
	"strings"
)

// computeDeviceFingerprint composes platform-specific hardware-stable
// signals into a single 128-bit (32 hex chars) SHA-256 truncated hash.
//
// Signal layout (joined by "|", missing pieces become empty strings):
//
//	hardwareUUID() | hardwareSerial() | primaryMAC() | cpuModel()
//
// Why each:
//   - hardwareUUID:  primary identity (IOPlatformUUID on darwin,
//     /etc/machine-id on linux, MachineGuid on windows).
//     Persists across OS reinstall on darwin; persists
//     across reboots elsewhere.
//   - hardwareSerial: secondary identity (darwin IOPlatformSerialNumber).
//     Adds entropy when the UUID source is missing.
//   - primaryMAC:    NIC hardware addr of the first non-loopback IPv4
//     interface. Less stable (replaced NIC, Wi-Fi
//     randomization) but useful as anti-clone signal.
//   - cpuModel:      Catch the case where two virtual machines share an
//     IOPlatformUUID seed (rare on prod, common in QA).
//
// Truncation: 32 hex chars = 128 bits = same order as thing_id; collision
// probability < 2^-64 across the global fleet, well below noise floor.
//
// Privacy contract: the raw signals NEVER leave the host. Only the hash is
// returned and copied into StaticInfo.DeviceFingerprint. A reader cannot
// recover the underlying serial / UUID / MAC from the hash.
//
// Empty return: if NONE of the signals can be collected (sandboxed
// runtime, restricted container), the hash is computed over "|||" which
// is well-known; we treat that as "no fingerprint available" and return
// "" instead of leaking a constant placeholder.
// ComputeDeviceFingerprint is the exported entry point used by callers
// outside opsmetrics (e.g. agent enrollment) that need the fingerprint
// before StaticInfo is built. The internal `computeDeviceFingerprint`
// stays lowercase for the existing CaptureStaticInfo call site.
func ComputeDeviceFingerprint() string { return computeDeviceFingerprint() }

// computeFingerprintSignalsFn gathers the four hardware signals used to
// compute the device fingerprint. It is a package-level variable so tests
// can inject all-empty results to exercise the "no fingerprint available"
// branch without needing a sandboxed runtime. Production code never
// reassigns this variable.
var computeFingerprintSignalsFn = func() (uuid, serial, mac, cpu string) {
	return hardwareUUID(), hardwareSerial(), primaryMAC(), cpuModel()
}

func computeDeviceFingerprint() string {
	uuid, serial, mac, cpu := computeFingerprintSignalsFn()

	if uuid == "" && serial == "" && mac == "" && cpu == "" {
		return ""
	}

	parts := []string{uuid, serial, mac, cpu}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:16]) // first 16 bytes = 32 hex chars
}

// netInterfacesFn is the function used to enumerate network interfaces.
// It is a package-level variable so tests can inject a stub to exercise
// the error and empty-result branches without depending on OS NIC state.
// Production code never reassigns this variable.
var netInterfacesFn = net.Interfaces

// primaryMAC returns the MAC address of the first non-loopback interface
// that has at least one IP. Stable across reboots on most systems; not
// stable across NIC swap or Wi-Fi MAC randomization. Empty on any error.
//
// This is shared across platforms — Go's net package abstracts the
// per-OS NIC enumeration so we don't need build-tagged variants.
func primaryMAC() string {
	ifaces, err := netInterfacesFn()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil || len(addrs) == 0 {
			continue
		}
		return iface.HardwareAddr.String()
	}
	return ""
}

// cpuModel returns a coarse CPU identifier (e.g. "Apple M2 Pro" or
// "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz"). Implementation is
// platform-specific; this helper centralizes the runtime.GOARCH prefix
// so the fingerprint differs between arm64 / amd64 builds on the same
// CPU family (which is a desirable property — running a different
// architecture binary on the same host SHOULD register as a different
// device fingerprint).
func cpuModel() string {
	return runtime.GOARCH + ":" + cpuBrandString()
}
