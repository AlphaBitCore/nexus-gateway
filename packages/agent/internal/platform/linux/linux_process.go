//go:build linux

package linux

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/pidcache"
)

// procMetaCache collapses repeated /proc reads for the same PID: a browser
// opening many connections from one process resolves its metadata once per
// TTL instead of per connection.
var procMetaCache = pidcache.New()

// ProcessInfo resolves process metadata from /proc on Linux, cached by PID.
func (p *LinuxPlatform) ProcessInfo(pid int) (api.ProcessMeta, error) {
	return procMetaCache.Get(pid, processInfoUncached)
}

// processInfoUncached does the raw /proc read for one PID.
func processInfoUncached(pid int) (api.ProcessMeta, error) {
	meta := api.ProcessMeta{PID: pid}
	pidStr := strconv.Itoa(pid)

	// Executable path: /proc/[pid]/exe symlink
	exePath, err := os.Readlink(filepath.Join("/proc", pidStr, "exe"))
	if err == nil {
		meta.Path = exePath
		meta.Name = filepath.Base(exePath)
	}

	// Short name fallback: /proc/[pid]/comm
	if meta.Name == "" {
		if data, err := os.ReadFile(filepath.Join("/proc", pidStr, "comm")); err == nil {
			meta.Name = strings.TrimSpace(string(data))
		}
	}

	// User from UID in /proc/[pid]/status
	if data, err := os.ReadFile(filepath.Join("/proc", pidStr, "status")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if u, err := user.LookupId(fields[1]); err == nil {
						meta.User = u.Username
					}
				}
				break
			}
		}
	}

	if meta.Path == "" {
		return meta, fmt.Errorf("cannot resolve exe for pid %d", pid)
	}
	return meta, nil
}

// findPIDBySocket looks up the owning PID of a TCP socket by parsing
// /proc/net/tcp[6] and matching against /proc/[pid]/fd symlinks.
//
// There is no inode-keyed cache here on purpose: every new connection has a
// unique socket inode, so an inode→PID cache can never hit on this path (it
// is called once per connection). The repeated-work win lives one level
// down, in the PID-keyed procMetaCache that ProcessInfo uses — a browser's
// many connections share the same PID even though each has a fresh inode.
func findPIDBySocket(srcIP string, srcPort int, dstIP string, dstPort int) int {
	inode := findSocketInode(srcIP, srcPort, dstIP, dstPort)
	if inode == "" {
		return 0
	}
	return findPIDByInode(inode)
}

// findSocketInode parses /proc/net/tcp and /proc/net/tcp6 to find the inode
// of a socket matching the given local (and, when known, remote) address.
// Both address families are scanned because a v4 connection can appear in
// tcp6 as a v4-mapped address and a genuine IPv6 connection only appears in
// tcp6 — scanning only IPv4 would never resolve a PID for those.
func findSocketInode(localIP string, localPort int, remoteIP string, remotePort int) string {
	localHex := ipPortToHex(localIP, localPort)
	remoteHex := ipPortToHex(remoteIP, remotePort)
	if localHex == "" {
		return ""
	}
	for _, procFile := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if inode := scanProcNetTCP(procFile, localHex, remoteHex); inode != "" {
			return inode
		}
	}
	return ""
}

// scanProcNetTCP scans one /proc/net/tcp{,6} file for a row whose local
// (and optionally remote) hex address matches. The localHex/remoteHex are
// the v4 8-hex-digit forms; for tcp6 the v4-mapped address occupies the
// low 32 bits, so we suffix-match the 32-hex-digit v6 column.
func scanProcNetTCP(procFile, localHex, remoteHex string) string {
	f, err := os.Open(procFile)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		if !addrHexMatch(fields[1], localHex) {
			continue
		}
		// Match remote too when known, to avoid TOCTOU collisions with
		// TIME_WAIT sockets sharing the same local address.
		if remoteHex == "" || addrHexMatch(fields[2], remoteHex) {
			return fields[9] // inode
		}
	}
	return ""
}

// addrHexMatch compares a /proc/net/tcp{,6} "ADDR:PORT" hex column against
// the v4 "IIIIIIII:PPPP" form. For tcp (v4) it is an exact match; for tcp6
// the address column is 32 hex digits and a v4 / v4-mapped connection's
// v4 octets sit in the final 8, so the port must match exactly and the v4
// address hex must be the suffix of the v6 address hex.
func addrHexMatch(column, v4HexAddrPort string) bool {
	colAddr, colPort, ok := splitHexAddrPort(column)
	if !ok {
		return false
	}
	wantAddr, wantPort, ok := splitHexAddrPort(v4HexAddrPort)
	if !ok {
		return false
	}
	if !strings.EqualFold(colPort, wantPort) {
		return false
	}
	if strings.EqualFold(colAddr, wantAddr) {
		return true // exact (tcp v4)
	}
	// tcp6: v4-mapped address in the low 32 bits → suffix match.
	return len(colAddr) == 32 && strings.EqualFold(colAddr[24:], wantAddr)
}

// splitHexAddrPort splits "ADDR:PORT" into its hex address and port halves.
func splitHexAddrPort(s string) (addr, port string, ok bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// findPIDByInode searches /proc/[pid]/fd/ for a socket inode.
func findPIDByInode(inode string) int {
	target := "socket:[" + inode + "]"
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && link == target {
				return pid
			}
		}
	}
	return 0
}

// ipPortToHex converts an IP:port pair to the /proc/net/tcp{,6} hex format.
//   - IPv4 → 8 hex digits (one little-endian uint32). Example:
//     "127.0.0.1:443" → "0100007F:01BB".
//   - IPv6 → 32 hex digits (four little-endian uint32 words), matching the
//     /proc/net/tcp6 layout, so genuine IPv6 connections resolve too.
//
// A v4 address returns the 8-digit form; addrHexMatch handles matching it
// against the v4-mapped suffix in tcp6.
func ipPortToHex(ip string, port int) string {
	hexPort := fmt.Sprintf("%04X", port)
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		return fmt.Sprintf("%02X%02X%02X%02X:%s", v4[3], v4[2], v4[1], v4[0], hexPort)
	}
	v6 := parsed.To16()
	if v6 == nil {
		return ""
	}
	var b strings.Builder
	for w := range 4 { // four little-endian 32-bit words
		base := w * 4
		fmt.Fprintf(&b, "%02X%02X%02X%02X", v6[base+3], v6[base+2], v6[base+1], v6[base])
	}
	return b.String() + ":" + hexPort
}
