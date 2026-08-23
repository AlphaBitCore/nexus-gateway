package configkey

import "strings"

// The `system_metadata` table has no schema and no owner. A row is a key and a
// JSON blob, written by whichever service reached for it, and nothing has ever
// checked that a key is one somebody still reads.
//
// That is how `audit.payload` survived three months after the code stopped
// reading it — and it was worse than dead weight, because its value was the
// OPPOSITE of the setting actually in force and its timestamp kept moving.
// Anyone asking "is body capture on?" by the obvious name read `false` and
// stopped investigating.
//
// So: a boot-time WARN naming every key nobody claims. Not fatal — an unknown
// key does not break a running system, and refusing to start over a stale row
// would turn a bookkeeping problem into an outage. It needs a person to look,
// which is exactly what nothing prompted before.

// SystemMetadataKeys is every fixed key a service reads or writes.
//
// A key here is a claim that some code path uses it. Deleting the code without
// deleting the entry is the drift this list exists to surface, so the reverse
// check — an entry no code mentions — belongs in a test, not at runtime.
var SystemMetadataKeys = []string{
	"agent.config.version",
	"agent.settings",
	"audit_chain.acked_orphans",
	"device.auth.mode",
	"gateway.credential_reliability.config",
	"gateway.settings",
	"hub.spill_upload_secret",
	"observability.config",
	"payload_capture.config",
	"siem.config",
	"streaming_compliance.config",
}

// systemMetadataKeyFamilies are the DYNAMIC keys, built at runtime from a thing
// type and a config key rather than written down.
//
// They are the reason a plain set is the wrong shape. Production carries twelve
// `propagation_ledger:` rows and one enumerated set would have to be regenerated
// every time a config key or thing type is added — so the check would either go
// stale or fire on legitimate rows every boot. A warning that cries wolf twelve
// times a day is a warning nobody reads, which is the failure this whole
// mechanism exists to prevent.
var systemMetadataKeyFamilies = []string{
	"propagation_ledger:",
}

// UnknownSystemMetadataKeys returns the subset of `present` that no code claims.
//
// Given the keys a database actually holds, it names the ones to look at. The
// caller logs them; this decides nothing on its own, because "unknown" is a
// prompt to investigate and not a verdict — a key may belong to a service that
// has not been deployed yet, or to one being retired in the other order.
func UnknownSystemMetadataKeys(present []string) []string {
	known := make(map[string]struct{}, len(SystemMetadataKeys))
	for _, k := range SystemMetadataKeys {
		known[k] = struct{}{}
	}
	var out []string
	for _, k := range present {
		if _, ok := known[k]; ok {
			continue
		}
		claimed := false
		for _, prefix := range systemMetadataKeyFamilies {
			if strings.HasPrefix(k, prefix) {
				claimed = true
				break
			}
		}
		if !claimed {
			out = append(out, k)
		}
	}
	return out
}
