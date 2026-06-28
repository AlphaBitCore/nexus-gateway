//go:build windows

// wfp_policy.go — marshalling for the IOCTL_NEXUS_WFP_PUSH_POLICY body.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §7
// SDD: docs/developers/specs/e59-s2-usermode-go-integration.md §T4
//
// Wire format (little-endian, packed):
//
//   NexusPolicyHeader {
//     u32 version    == NEXUS_WFP_PROTOCOL_VERSION (2)
//     u32 generation
//     u8  killSwitch
//     u8[3] reserved
//     u32 processBypassCount   ≤ NEXUS_MAX_PROCESS_BYPASS (256)
//     u32 destBypassCount      ≤ NEXUS_MAX_DEST_BYPASS    (1024)
//     u32 quicFallbackCount    ≤ NEXUS_MAX_QUIC_FALLBACK  (64)   // v2+
//   }
//   u32 processBypass[processBypassCount]
//   NexusCidr destBypass[destBypassCount] {
//     u8 family       AF_INET (2) | AF_INET6 (23)
//     u8 prefixLen
//     u8[2] reserved
//     u8[16] addr     IPv4 in first 4 bytes
//   }
//   NexusQuicImage quicFallback[quicFallbackCount] {            // v2+
//     u16 len                  WCHARs used in name[] (≤ 64)
//     u16[64] name             image basename, lowercase, UTF-16LE
//   }

package windows

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"unicode/utf16"
)

const (
	protocolVersion       uint32 = 2
	maxProcessBypassCount        = 256
	maxDestBypassCount           = 1024
	maxQuicFallbackCount         = 64
	quicImageMaxChars            = 64

	policyHeaderSize = 4 /*version*/ + 4 /*gen*/ + 1 /*ks*/ + 3 /*rsv*/ +
		4 /*pCnt*/ + 4 /*dCnt*/ + 4 /*qCnt*/ // = 24
	cidrEntrySize = 1 + 1 + 2 + 16          // = 20
	quicImageSize = 2 + quicImageMaxChars*2 // = 130 (u16 len + 64 UTF-16 chars)

	afInet  uint8 = 2
	afInet6 uint8 = 23
)

var (
	errPolicyTooManyPIDs   = errors.New("wfp: bypass PID count exceeds NEXUS_MAX_PROCESS_BYPASS")
	errPolicyTooManyCIDRs  = errors.New("wfp: bypass CIDR count exceeds NEXUS_MAX_DEST_BYPASS")
	errPolicyTooManyImages = errors.New("wfp: QUIC-fallback image count exceeds NEXUS_MAX_QUIC_FALLBACK")
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
	if len(p.QUICFallbackImages) > maxQuicFallbackCount {
		return nil, errPolicyTooManyImages
	}

	total := policyHeaderSize +
		len(p.BypassPIDs)*4 +
		len(p.BypassCIDRs)*cidrEntrySize +
		len(p.QUICFallbackImages)*quicImageSize
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
	binary.LittleEndian.PutUint32(buf[20:], uint32(len(p.QUICFallbackImages)))

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

	for _, img := range p.QUICFallbackImages {
		writeQuicImage(buf[off:off+quicImageSize], img)
		off += quicImageSize
	}

	return buf, nil
}

// writeQuicImage encodes one NexusQuicImage record into dst (length
// quicImageSize): a u16 WCHAR-count followed by the lowercase basename as
// UTF-16LE, zero-padded to quicImageMaxChars chars. Names longer than the
// cap are truncated (defensive; admins use short image names like
// "chrome.exe").
func writeQuicImage(dst []byte, name string) {
	u := utf16.Encode([]rune(quicImageBasename(name)))
	if len(u) > quicImageMaxChars {
		u = u[:quicImageMaxChars]
	}
	binary.LittleEndian.PutUint16(dst[0:], uint16(len(u)))
	for i, c := range u {
		binary.LittleEndian.PutUint16(dst[2+i*2:], c)
	}
}

// quicImageBasename lowercases and strips any directory prefix, so an admin
// may enter either "chrome.exe" or a full path; the kernel matches on the
// basename of the connecting process's ALE_APP_ID device path.
func quicImageBasename(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	return name
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
	qCnt := binary.LittleEndian.Uint32(buf[20:])

	if pCnt > maxProcessBypassCount || dCnt > maxDestBypassCount || qCnt > maxQuicFallbackCount {
		return Policy{}, errors.New("wfp: policy counts exceed limits")
	}
	expected := policyHeaderSize + int(pCnt)*4 + int(dCnt)*cidrEntrySize + int(qCnt)*quicImageSize
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

	images := make([]string, 0, qCnt)
	for i := uint32(0); i < qCnt; i++ {
		n := binary.LittleEndian.Uint16(buf[off:])
		if int(n) > quicImageMaxChars {
			return Policy{}, errors.New("wfp: QUIC image name length exceeds cap")
		}
		u := make([]uint16, n)
		for j := 0; j < int(n); j++ {
			u[j] = binary.LittleEndian.Uint16(buf[off+2+j*2:])
		}
		images = append(images, string(utf16.Decode(u)))
		off += quicImageSize
	}

	return Policy{
		Generation:         gen,
		KillSwitch:         ks,
		BypassPIDs:         pids,
		BypassCIDRs:        cidrs,
		QUICFallbackImages: images,
	}, nil
}
