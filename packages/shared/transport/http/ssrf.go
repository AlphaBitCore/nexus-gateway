package http

import (
	"fmt"
	"net"
	"syscall"
)

// IsDisallowedIP reports whether ip is in a range that an outbound fetch to an
// operator-supplied / attacker-influenceable URL (SSO discovery, SIEM webhook,
// generic admin-configured callback) must never reach. Returning true for an IP
// means "refuse to dial it".
//
// The blocked set is every range that can pivot into the deployment's own
// internal surface or a cloud metadata service:
//   - loopback              127.0.0.0/8, ::1
//   - RFC-1918 private       10/8, 172.16/12, 192.168/16  (net.IP.IsPrivate)
//   - RFC-4193 ULA           fc00::/7                       (net.IP.IsPrivate)
//   - link-local unicast     169.254/16 (incl. 169.254.169.254 cloud metadata),
//     fe80::/10
//   - link-local multicast, multicast, and the unspecified address.
//
// Public unicast addresses return false. This is the single source of truth for
// the SSRF deny-list; do not re-implement it per call site (a divergent copy is
// itself an SSRF hole). See [BlockPrivateDialControl] for the dialer hook.
func IsDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsPrivate()
}

// IsMetadataOrLinkLocalIP reports whether ip is a cloud instance-metadata or
// link-local address: the IPv4 169.254.0.0/16 IMDS surface (AWS/GCP/Azure
// 169.254.169.254), the IPv6 fe80::/10 link-local block, AND the fixed AWS
// IMDS-over-IPv6 ULA endpoint fd00:ec2::254 (which is NOT link-local, so it is
// matched explicitly). v4-mapped IPv6 forms (::ffff:169.254.169.254) and the
// unspecified address are caught too because net.IP collapses v4-mapped
// addresses to their v4 semantics before the link-local test.
//
// This is the MINIMUM deny-set every admin-configured-URL egress must apply: a
// metadata endpoint is never a legitimate provider base URL or webhook target,
// so reaching one is always an SSRF credential-theft pivot. It deliberately does
// NOT block RFC-1918 / loopback so an on-prem self-hosted provider (vLLM/Ollama
// on 10.x or 127.0.0.1) can still be probed — that broader block lives in
// [IsDisallowedIP] / [BlockPrivateDialControl] for the webhook/SIEM egress paths.
func IsMetadataOrLinkLocalIP(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// AWS exposes IMDS over IPv6 at the fixed ULA fd00:ec2::254. ULAs are not
	// link-local, and we deliberately permit ULA/RFC-1918 here for on-prem
	// providers, so this specific metadata endpoint must be blocked explicitly.
	return ip.Equal(awsIPv6MetadataIP)
}

// awsIPv6MetadataIP is the fixed AWS IMDS-over-IPv6 endpoint (fd00:ec2::254).
var awsIPv6MetadataIP = net.ParseIP("fd00:ec2::254")

// BlockPrivateDialControl is a [net.Dialer.Control] hook that refuses to connect
// to a non-public IP. It runs AFTER name resolution with the concrete address
// the socket would actually dial, so it catches both a directly-numeric internal
// host and a hostname that resolves (or DNS-rebinds at dial time) to an internal
// IP. A parse failure or a disallowed range returns an error, aborting the dial.
//
// Use this for fully-external admin-configured-URL egress (webhook senders,
// SIEM/audit sinks) where a private/loopback target is never legitimate. For the
// provider-connectivity probe — which legitimately reaches on-prem RFC-1918 /
// loopback providers — use [BlockMetadataDialControl] instead, which still blocks
// the metadata range. Prefer the named constructors in [AdminEgressGuard].
//
// Install it on the specific egress path via Config.DialControl (per-client,
// scoped):
//
//	client := nexushttp.New(nexushttp.Config{DialControl: nexushttp.BlockPrivateDialControl})
//
// Do NOT install it process-wide via SetGlobalDialControl in a service that also
// dials its own internal dependencies (Postgres, Redis, NATS, the Hub) over
// private addresses — that would fail those connections closed. The guard is for
// outbound-to-untrusted-URL paths only.
func BlockPrivateDialControl(_, address string, _ syscall.RawConn) error {
	return dialGuard(address, IsDisallowedIP)
}

// BlockMetadataDialControl is a [net.Dialer.Control] hook that refuses to connect
// ONLY to a cloud-metadata / link-local address (see [IsMetadataOrLinkLocalIP]),
// while permitting RFC-1918 / loopback so on-prem self-hosted provider base URLs
// (vLLM/Ollama on 10.x or 127.0.0.1) remain reachable. It runs AFTER name
// resolution on the concrete dial address, so it defeats a hostname that
// DNS-rebinds to 169.254.169.254 at dial time.
//
// This is the correct guard for the provider-connectivity probe: it closes the
// metadata-exfiltration pivot without breaking the on-prem-provider use case.
func BlockMetadataDialControl(_, address string, _ syscall.RawConn) error {
	return dialGuard(address, IsMetadataOrLinkLocalIP)
}

// dialGuard parses the resolved dial address and refuses the connection when the
// concrete IP matches deny. Shared by both dial controls so the parse/refuse
// logic exists in exactly one place.
func dialGuard(address string, deny func(net.IP) bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("ssrf-guard: cannot parse dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("ssrf-guard: dial address %q is not an IP", host)
	}
	if deny(ip) {
		return fmt.Errorf("ssrf-guard: refusing to connect to non-public address %s", ip)
	}
	return nil
}

// AdminEgressKind names the SSRF policy an admin-configured-URL egress path needs.
// It is the single decision point every such call site routes through, so the
// per-path policy lives in one table instead of being re-picked (and drifting)
// at each dialer construction.
type AdminEgressKind int

const (
	// AdminEgressExternalOnly blocks every non-public address (loopback,
	// RFC-1918, ULA, link-local, metadata). Use for egress whose target is
	// external by nature and never legitimately internal: webhook senders
	// (alert / Slack / policy-hook) and SIEM / audit sinks.
	AdminEgressExternalOnly AdminEgressKind = iota
	// AdminEgressAllowPrivate blocks only the cloud-metadata / link-local
	// range, permitting RFC-1918 / loopback. Use for the provider-connectivity
	// probe, which legitimately reaches on-prem self-hosted providers on
	// private addresses but must never be steered at an instance-metadata
	// endpoint.
	AdminEgressAllowPrivate
)

// AdminEgressDialControl returns the [net.Dialer.Control] guard for the given
// admin-configured-URL egress policy. This is the one chokepoint: every site
// that dials an operator-supplied URL obtains its dial guard here rather than
// referencing BlockPrivateDialControl / BlockMetadataDialControl directly, so
// the "which ranges does THIS egress class block" decision is centralized and a
// new call site cannot silently pick the wrong (or no) guard.
func AdminEgressDialControl(kind AdminEgressKind) func(network, address string, c syscall.RawConn) error {
	switch kind {
	case AdminEgressAllowPrivate:
		return BlockMetadataDialControl
	default:
		return BlockPrivateDialControl
	}
}
