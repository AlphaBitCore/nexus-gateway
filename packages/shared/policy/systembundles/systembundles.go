// Package systembundles is the single source of truth for the macOS system
// networking/push/continuity daemons that MUST NEVER have their UDP closed by
// the agent's QUIC-fallback kill-list (CLAUDE.md NE rule 5).
//
// The kill-list (`forceQUICFallbackBundles`) is admin-controlled: an entry E
// makes the NE force any flow whose bundle id equals E — OR has prefix `E + "."`
// (helper/child match, QUICFallbackBundles.swift) — onto TCP, which for a UDP
// flow means closing it. So a single over-broad entry like "com.apple" closes
// UDP for every com.apple.* daemon, and a bare system daemon id closes that
// daemon directly. Either takes down DNS / DHCP / Push / Continuity / time /
// Kerberos for the whole enrolled fleet from a routine settings permission.
//
// This package is consumed on BOTH the Control Plane write path (reject the
// request — defends against the low-priv admin persona) and the agent daemon
// write path (strip before the file is written — defends against a hostile node
// that pushes the agent_settings shadow directly, bypassing the CP handler).
// Keeping the list in one Go package is the only way those two Go gates can
// agree, so this is the SSOT for the protected set on the Go side.
//
// The NE Swift provider's copy of this floor is GENERATED from ProtectedBundles
// below — it is no longer hand-synced and can no longer lag. The generator
// (internal/gen) emits SystemBundles.generated.swift into the NexusAgentExtension
// directory; the generated Swift reproduces the normalize()/related() matching
// semantics EXACTLY (lowercase-normalized equal / ancestor / descendant), and
// a golden test in this package asserts the committed file is up to date so CI
// catches a stale checkout. This closes the two drift findings that were open
// while the Swift set was hand-maintained: F-0392 (codegen the Swift set from
// this SSOT) and F-0368 (the Swift set lagged — missing identityservicesd,
// rapportd, networkserviceproxy, the com.apple.kerberos family). Both are now
// correct-by-construction: the Swift floor IS this set, rendered.
//
// The floor stays a HARDCODED compile-time constant on BOTH sides — it is
// intentionally NOT shadow-controllable. That immutability is the SEC-M8-01
// A3-defense: an attacker who can push the agent_settings shadow cannot widen
// a kill-list to close UDP for a system daemon, because the floor is compiled
// in, not config.
//
// Code generated consumer: SystemBundles.generated.swift (via internal/gen).
package systembundles

//go:generate go run ./internal/gen

import "strings"

// ProtectedBundles is the protected set: critical macOS system networking,
// push, continuity, time, location, and authentication daemons. Entries are
// lowercase for case-insensitive matching. This is intentionally a SUPERSET of
// the NE's historical 13-entry fast-decline list — that set was incomplete
// (it omitted iMessage/FaceTime signalling, Continuity/AirDrop,
// the network-service proxy, and the Kerberos family), so those are included
// here. `com.apple.kerberos` is listed as a namespace so every `*.Kerberos.*`
// child daemon is covered by the descendant rule in Covers.
var ProtectedBundles = []string{
	"com.apple.mdnsresponder",     // multicast DNS / unicast DNS resolution
	"com.apple.configd",           // SystemConfiguration (network state)
	"com.apple.dhcpcd",            // DHCP client
	"com.apple.bootpd",            // BOOTP/DHCP
	"com.apple.apsd",              // Apple Push (APNs)
	"com.apple.nsurlsessiond",     // background URLSession / push-kit transfers
	"com.apple.identityservicesd", // iMessage / FaceTime signalling
	"com.apple.rapportd",          // Continuity / Handoff / AirDrop
	"com.apple.networkserviceproxy",
	"com.apple.kerberos", // Kerberos.* family (kdc, digest, …) — namespace
	"com.apple.kdc",
	"com.apple.timed",     // time sync
	"com.apple.locationd", // location (Wi-Fi/cell assist)
	"com.apple.symptomsd", // network symptom/connectivity
	"ntpd",                // bare process names the NE set also carries
	"mdnsresponder",
	"launchd",
}

// normalize lowercases and trims surrounding whitespace plus any trailing dots
// (a "com.apple." typo is inert in the NE matcher but normalizing it to
// "com.apple" lets the ancestor rule catch the over-broad intent).
func normalize(s string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(s)), ".")
}

// related reports whether kill-entry e and protected bundle p sit in an
// equal / ancestor / descendant relationship in the dotted-namespace tree —
// i.e. whether honoring e as a kill-list entry would close UDP for p:
//
//	e == p                  exact system daemon (e.g. "com.apple.apsd")
//	p starts with e + "."   e is an over-broad ancestor (e="com.apple" ⊃ p)
//	e starts with p + "."   e is a child of a protected namespace
//	                        (e="com.apple.kerberos.kdc" under p="com.apple.kerberos")
func related(e, p string) bool {
	return e == p || strings.HasPrefix(p, e+".") || strings.HasPrefix(e, p+".")
}

// Covers reports whether the kill-list entry would close UDP for a protected
// system bundle, and returns that bundle for a precise operator error. ok is
// false for a normal app bundle (e.g. "com.google.Chrome", "com.apple.Safari"),
// which is a legitimate QUIC-fallback target and must be allowed.
func Covers(entry string) (protected string, ok bool) {
	e := normalize(entry)
	if e == "" {
		return "", false
	}
	for _, p := range ProtectedBundles {
		if related(e, p) {
			return p, true
		}
	}
	return "", false
}

// Filter splits entries into the safe set to honor and the protected entries
// that were dropped. Used by the agent daemon write path as defense-in-depth
// when a shadow write reaches it without passing the CP reject gate.
func Filter(entries []string) (clean, stripped []string) {
	for _, e := range entries {
		if _, bad := Covers(e); bad {
			stripped = append(stripped, e)
			continue
		}
		clean = append(clean, e)
	}
	return clean, stripped
}
