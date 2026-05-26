// Package opsmetrics provides the runtime + business metric sampler used by
// every Nexus Thing (services and agents) for Hub-collected operational
// telemetry.
package registry

import "time"

// MetricKind enumerates the three supported metric shapes.
type MetricKind string

const (
	KindGauge     MetricKind = "gauge"
	KindCounter   MetricKind = "counter"
	KindHistogram MetricKind = "histogram"
)

// Sample is a single observation of one metric.
//
// For gauges and counters, Value is the cumulative observed value at the
// sample time. For histograms, Value is left at zero and the bucket counts
// live in Metadata under the key "buckets" as a six-element int array (the
// canonical histogram layout from spec §6.4).
type Sample struct {
	Name         string         `json:"name"`
	Kind         MetricKind     `json:"kind"`
	DimensionKey string         `json:"dim"`
	Value        float64        `json:"value,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// SampleBatch is the WS message payload for `metrics_sample`.
type SampleBatch struct {
	ThingID   string    `json:"thingId"`
	SampledAt time.Time `json:"sampledAt"`
	Samples   []Sample  `json:"samples"`
}

// Diag levels.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"
)

// Diag event types.
const (
	EventTypeError     = "error"
	EventTypeCrash     = "crash"
	EventTypeWatchdog  = "watchdog"
	EventTypeLifecycle = "lifecycle"
)

// DiagEvent is the WS / HTTP payload for `diag_event`.
//
// TraceID is a first-class typed field that carries the cross-service
// correlation id (the X-Nexus-Request-Id header value) so a query for
// "every diag row that belongs to trace X" hits a real column with a
// btree index instead of probing the JSONB Attrs map. The SlogSink
// auto-extracts the `trace_id` slog attr into this field at emit time;
// the Hub diag writer + drain handler persist it into thing_diag_event.trace_id.
type DiagEvent struct {
	ThingID      string         `json:"thingId"`
	OccurredAt   time.Time      `json:"occurredAt"`
	Level        string         `json:"level"`
	EventType    string         `json:"eventType"`
	Source       string         `json:"source"`
	Message      string         `json:"message"`
	MessageHash  string         `json:"messageHash"`
	TraceID      string         `json:"traceId,omitempty"`
	Attrs        map[string]any `json:"attrs,omitempty"`
	StackTrace   string         `json:"stackTrace,omitempty"`
	RepeatCount  int            `json:"repeatCount"`
	AgentVersion string         `json:"agentVersion,omitempty"`
	OSInfo       map[string]any `json:"osInfo,omitempty"`
}

// StaticInfo is the L2 static-identity payload that Things write to
// thing.metadata.staticInfo via thingclient.UpdateStaticInfo (spec §5.6).
type StaticInfo struct {
	Hostname  string `json:"hostname"`
	PrimaryIP string `json:"primaryIp"`
	OS        string `json:"os"`
	// OSName is the human-friendly OS name (macOS / Linux / Windows),
	// derived from runtime.GOOS. The CP admin UI's Device Detail →
	// System tab renders this in the "OS Name" row alongside
	// OSVersion; `OS` itself stays as the protocol-level GOOS value.
	OSName        string `json:"osName,omitempty"`
	OSVersion     string `json:"osVersion"`
	KernelVersion string `json:"kernelVersion"`
	// MachineID is the OS-managed machine identifier — IOPlatformUUID
	// on macOS, /etc/machine-id on Linux, MachineGuid on Windows.
	// Stable across reboots; resets only via rare admin actions
	// (nvram -c / systemd-machine-id-setup / sysprep /generalize).
	MachineID string `json:"machineId,omitempty"`
	// CPUModel is the friendly CPU brand string (e.g. "Apple M2 Pro",
	// "Intel(R) Xeon(R) Platinum 8275CL"). Empty when the sysctl /
	// /proc/cpuinfo lookup is unavailable.
	CPUModel      string `json:"cpuModel,omitempty"`
	CPUCores      int    `json:"cpuCores"`
	TotalRAMBytes uint64 `json:"totalRamBytes"`
	// TotalMemMB mirrors TotalRAMBytes / (1024 * 1024) and exists for
	// the admin UI which displays the value in MB. Kept as a separate
	// field so the UI binding doesn't need to do the division client-side.
	TotalMemMB uint64 `json:"totalMemMB,omitempty"`
	// SerialNumber is the chassis-embossed device serial — IOPlatformSerialNumber
	// on macOS, /sys/class/dmi/id/product_serial on Linux (root-only on most
	// distros), BIOS serial on Windows. Empty when the lookup is unavailable.
	SerialNumber string `json:"serialNumber,omitempty"`
	// ModelName is the marketing product name — "MacBookPro18,2" on macOS,
	// /sys/class/dmi/id/product_name on Linux, computersystem.Model on
	// Windows. Empty when unavailable.
	ModelName            string `json:"modelName,omitempty"`
	ServiceVersion       string `json:"serviceVersion"`
	BuildSHA             string `json:"buildSha"`
	BuildTime            string `json:"buildTime"`
	StartTime            string `json:"startTime"`
	ConfigVersionApplied int64  `json:"configVersionApplied"`
	// PublicURL is the externally-reachable base URL that clients of
	// this service use (scheme + host[:port], no trailing slash).
	// Populated by server-side Things (Hub, Control Plane, AI Gateway,
	// Compliance Proxy) from their YAML config. Empty for client Things
	// like Agent (clients don't accept inbound connections, no external
	// URL to advertise).
	//
	// Consumers: Control Plane's admin API surfaces a thing_type →
	// publicURL map so the UI can render real-environment URLs in
	// install / enrollment / sign-in guidance without hardcoding any
	// hostname.
	PublicURL string `json:"publicUrl,omitempty"`

	// DeviceFingerprint is a SHA-256 truncated to 128 bits (32 hex chars)
	// of hardware-stable signals — IOPlatformUUID + IOPlatformSerial +
	// primary NIC MAC + CPU model on macOS; /etc/machine-id + MAC + CPU
	// on Linux; registry MachineGuid + MAC + CPU on Windows.
	//
	// Purpose: a stable identifier for the underlying physical (or VM)
	// device that survives OS reinstall (machineUUID / machine-id /
	// MachineGuid all persist) so the fleet can:
	//   - detect duplicate enrollments from the same hardware under
	//     different SSO accounts (anti-fraud)
	//   - reconcile a re-enrolled Thing back to its prior thing_id
	//   - count distinct physical machines for licensing
	//
	// Privacy contract: the raw signals are intentionally NOT surfaced
	// here; only the hash leaves the host. The hash is one-way — a
	// reader cannot recover the underlying serial number / MAC / UUID.
	// Empty when none of the signals are available (e.g. a sandboxed
	// runtime that blocks ioreg / can't read /etc/machine-id).
	DeviceFingerprint string `json:"deviceFingerprint,omitempty"`
}
