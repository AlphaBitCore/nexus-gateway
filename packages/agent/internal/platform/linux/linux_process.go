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
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
)

// ProcessInfo resolves process metadata from /proc on Linux.
func (p *LinuxPlatform) ProcessInfo(pid int) (api.ProcessMeta, error) {
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

// inodePIDCache caches inode→PID lookups for 2 seconds to avoid the expensive
// O(processes×fds) /proc scan on every intercepted connection.
var (
	inodeCacheMu  sync.RWMutex
	inodePIDCache = make(map[string]inodeCacheEntry)
	inodeCacheTTL = 2 * time.Second
)

type inodeCacheEntry struct {
	pid       int
	expiresAt time.Time
}

// findPIDBySocket looks up the owning PID of a TCP socket by parsing
// /proc/net/tcp and matching against /proc/[pid]/fd symlinks.
func findPIDBySocket(srcIP string, srcPort int, dstIP string, dstPort int) int {
	inode := findSocketInode(srcIP, srcPort, dstIP, dstPort)
	if inode == "" {
		return 0
	}

	// Check cache first.
	now := time.Now()
	inodeCacheMu.RLock()
	if entry, ok := inodePIDCache[inode]; ok && now.Before(entry.expiresAt) {
		inodeCacheMu.RUnlock()
		return entry.pid
	}
	inodeCacheMu.RUnlock()

	pid := findPIDByInode(inode)

	inodeCacheMu.Lock()
	inodePIDCache[inode] = inodeCacheEntry{pid: pid, expiresAt: now.Add(inodeCacheTTL)}
	// Evict expired entries periodically (when cache grows past 1024).
	if len(inodePIDCache) > 1024 {
		for k, v := range inodePIDCache {
			if now.After(v.expiresAt) {
				delete(inodePIDCache, k)
			}
		}
	}
	inodeCacheMu.Unlock()

	return pid
}

// findSocketInode parses /proc/net/tcp to find the inode of a socket matching
// the given local address.
func findSocketInode(localIP string, localPort int, remoteIP string, remotePort int) string {
	localHex := ipPortToHex(localIP, localPort)
	remoteHex := ipPortToHex(remoteIP, remotePort)

	f, err := os.Open("/proc/net/tcp")
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
		// Match both local and remote addresses to avoid TOCTOU collisions
		// with TIME_WAIT sockets sharing the same local address.
		if strings.EqualFold(fields[1], localHex) &&
			(remoteHex == "" || strings.EqualFold(fields[2], remoteHex)) {
			return fields[9] // inode
		}
	}
	return ""
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

// ipPortToHex converts an IP:port pair to the /proc/net/tcp hex format.
// Example: "127.0.0.1:443" → "0100007F:01BB"
func ipPortToHex(ip string, port int) string {
	parsed := net.ParseIP(ip).To4()
	if parsed == nil {
		return ""
	}
	// /proc/net/tcp stores IPs in little-endian uint32 hex on x86
	hexIP := fmt.Sprintf("%02X%02X%02X%02X", parsed[3], parsed[2], parsed[1], parsed[0])
	hexPort := fmt.Sprintf("%04X", port)
	return hexIP + ":" + hexPort
}
