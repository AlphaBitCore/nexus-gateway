package platform

import (
	"context"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// BuildInfo carries the static identity that callers (services / agents)
// inject into CaptureStaticInfo. ServiceVersion / BuildSHA / BuildTime
// are stamped at build time via -ldflags; StartTime is per-process;
// PublicURL is the deployment-time public base URL (from yaml config).
// None of these are derivable from runtime introspection alone.
type BuildInfo struct {
	ServiceVersion string
	BuildSHA       string
	BuildTime      string
	StartTime      string
	// PublicURL is the externally-reachable base URL clients use to
	// reach this service. Empty for client Things (Agent). Server
	// Things populate it from their `publicURL` yaml config field so
	// CP UI / API can render real-environment URLs (e.g. the Hub URL
	// shown in agent install instructions) without hardcoding host
	// names.
	PublicURL string
}

// CaptureStaticInfo gathers the L2 static-identity payload (spec §5.6) for the
// current process. Best-effort: any individual lookup that fails on the host
// platform contributes a zero value rather than failing the whole call.
func CaptureStaticInfo(b BuildInfo) registry.StaticInfo {
	hostname, _ := os.Hostname()
	ramBytes := totalRAMBytes()
	return registry.StaticInfo{
		Hostname:          hostname,
		PrimaryIP:         primaryOutboundIP(),
		OS:                runtime.GOOS,
		OSName:            friendlyOSName(runtime.GOOS),
		OSVersion:         osVersion(),
		KernelVersion:     kernelVersion(),
		MachineID:         hardwareUUID(),
		CPUModel:          cpuBrandString(),
		CPUCores:          runtime.NumCPU(),
		TotalRAMBytes:     ramBytes,
		TotalMemMB:        ramBytes / (1024 * 1024),
		SerialNumber:      hardwareSerial(),
		ModelName:         hardwareModel(),
		ServiceVersion:    b.ServiceVersion,
		BuildSHA:          b.BuildSHA,
		BuildTime:         b.BuildTime,
		StartTime:         b.StartTime,
		PublicURL:         b.PublicURL,
		DeviceFingerprint: computeDeviceFingerprint(),
	}
}

// friendlyOSName maps runtime.GOOS to the marketing OS name shown in the
// admin Device Detail → System tab. Unknown values fall through verbatim
// so future platforms don't get silently labelled "Other".
func friendlyOSName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return goos
	}
}

// dialContextFn is the dial function used by primaryOutboundIP.
// It is a package-level variable so tests can inject a stub connection to
// exercise both the error path and the non-UDPAddr local-address branch
// without needing real network access. Production code never reassigns
// this variable.
var dialContextFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, addr)
}

// primaryOutboundIP returns the IP the OS would use for an outbound TCP
// connection to a sentinel address — without actually opening one. UDP is
// connectionless, so the dial merely picks a route and source IP in the
// kernel. Returns empty on any error (sandbox, no route, etc.).
func primaryOutboundIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	conn, err := dialContextFn(ctx, "udp", "1.1.1.1:80")
	if err != nil {
		return ""
	}
	defer conn.Close() //nolint:errcheck
	if udp, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return udp.IP.String()
	}
	return ""
}
