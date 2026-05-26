//go:build windows

// wfp_policy.go — marshalling for the IOCTL_NEXUS_WFP_PUSH_POLICY body.
//
// SKELETON. See wfp_windows.go header for build-tag context.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §7
// SDD: docs/developers/specs/e59-s2-usermode-go-integration.md §T4
//
// Wire format (little-endian, packed):
//
//   NexusPolicyHeader {
//     u32 version    == NEXUS_WFP_PROTOCOL_VERSION
//     u32 generation
//     u8  killSwitch
//     u8[3] reserved
//     u32 processBypassCount   ≤ NEXUS_MAX_PROCESS_BYPASS (256)
//     u32 destBypassCount      ≤ NEXUS_MAX_DEST_BYPASS    (1024)
//   }
//   u32 processBypass[processBypassCount]
//   NexusCidr destBypass[destBypassCount] {
//     u8 family       AF_INET (2) | AF_INET6 (23)
//     u8 prefixLen
//     u8[2] reserved
//     u8[16] addr     IPv4 in first 4 bytes
//   }

package windows

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

const (
	protocolVersion       uint32 = 1
	maxProcessBypassCount        = 256
	maxDestBypassCount           = 1024

	policyHeaderSize = 4 /*version*/ + 4 /*gen*/ + 1 /*ks*/ + 3 /*rsv*/ +
		4 /*pCnt*/ + 4 /*dCnt*/ // = 20
	cidrEntrySize = 1 + 1 + 2 + 16 // = 20

	afInet  uint8 = 2
	afInet6 uint8 = 23
)

var (
	errPolicyTooManyPIDs  = errors.New("wfp: bypass PID count exceeds NEXUS_MAX_PROCESS_BYPASS")
	errPolicyTooManyCIDRs = errors.New("wfp: bypass CIDR count exceeds NEXUS_MAX_DEST_BYPASS")
)

// MarshalPolicy serialises a Policy to the on-wire byte layout
// expected by the driver. Returns a freshly-allocated buffer that
// the caller passes verbatim to IOCTL_NEXUS_WFP_PUSH_POLICY.
func MarshalPolicy(p Policy) ([]byte, error) {
	if len(p.BypassPIDs) > maxProcessBypassCount {
		return nil, errPolicyTooManyPIDs
	}
	if len(p.BypassCIDRs) > maxDestBypassCount {
		return nil, errPolicyTooManyCIDRs
	}

	total := policyHeaderSize +
		len(p.BypassPIDs)*4 +
		len(p.BypassCIDRs)*cidrEntrySize
	buf := make([]byte, total)

	// Header.
	binary.LittleEndian.PutUint32(buf[0:], protocolVersion)
	binary.LittleEndian.PutUint32(buf[4:], p.Generation)
	if p.KillSwitch {
		buf[8] = 1
	}
	// 3 reserved bytes already zero
	binary.LittleEndian.PutUint32(buf[12:], uint32(len(p.BypassPIDs)))
	binary.LittleEndian.PutUint32(buf[16:], uint32(len(p.BypassCIDRs)))

	off := policyHeaderSize
	for _, pid := range p.BypassPIDs {
		binary.LittleEndian.PutUint32(buf[off:], pid)
		off += 4
	}

	for _, cidr := range p.BypassCIDRs {
		addr := cidr.Addr()
		switch {
		case addr.Is4():
			buf[off] = afInet
			a4 := addr.As4()
			copy(buf[off+4:off+8], a4[:])
		case addr.Is6():
			buf[off] = afInet6
			a16 := addr.As16()
			copy(buf[off+4:off+20], a16[:])
		default:
			return nil, errors.New("wfp: bypass CIDR has unknown family")
		}
		buf[off+1] = uint8(cidr.Bits())
		// 2 reserved bytes already zero
		off += cidrEntrySize
	}

	return buf, nil
}

// UnmarshalPolicy is for round-trip testing only — the driver never
// sends a policy back to user-mode.
func UnmarshalPolicy(buf []byte) (Policy, error) {
	if len(buf) < policyHeaderSize {
		return Policy{}, errors.New("wfp: policy buffer too short")
	}
	ver := binary.LittleEndian.Uint32(buf[0:])
	if ver != protocolVersion {
		return Policy{}, ErrVersionMismatch
	}
	gen := binary.LittleEndian.Uint32(buf[4:])
	ks := buf[8] != 0
	pCnt := binary.LittleEndian.Uint32(buf[12:])
	dCnt := binary.LittleEndian.Uint32(buf[16:])

	if pCnt > maxProcessBypassCount || dCnt > maxDestBypassCount {
		return Policy{}, errors.New("wfp: policy counts exceed limits")
	}
	expected := policyHeaderSize + int(pCnt)*4 + int(dCnt)*cidrEntrySize
	if len(buf) < expected {
		return Policy{}, errors.New("wfp: policy buffer shorter than header counts")
	}

	pids := make([]uint32, 0, pCnt)
	off := policyHeaderSize
	for i := uint32(0); i < pCnt; i++ {
		pids = append(pids, binary.LittleEndian.Uint32(buf[off:]))
		off += 4
	}

	cidrs := make([]netip.Prefix, 0, dCnt)
	for i := uint32(0); i < dCnt; i++ {
		family := buf[off]
		prefixLen := int(buf[off+1])
		var addr netip.Addr
		switch family {
		case afInet:
			// Wire layout: IPv4 lives in the first 4 bytes of the
			// 16-byte addr slot, remainder zeroed by MarshalPolicy.
			// Use AddrFrom4 so the resulting netip.Addr round-trips
			// equal to the input (AddrFrom16 of the same bytes
			// would yield ::a.b.c.d, which compares unequal to the
			// caller's a.b.c.d).
			var a4 [4]byte
			copy(a4[:], buf[off+4:off+8])
			addr = netip.AddrFrom4(a4)
		case afInet6:
			var a16 [16]byte
			copy(a16[:], buf[off+4:off+20])
			addr = netip.AddrFrom16(a16)
		default:
			return Policy{}, errors.New("wfp: unknown CIDR family")
		}
		cidrs = append(cidrs, netip.PrefixFrom(addr, prefixLen))
		off += cidrEntrySize
	}

	return Policy{
		Generation:  gen,
		KillSwitch:  ks,
		BypassPIDs:  pids,
		BypassCIDRs: cidrs,
	}, nil
}
