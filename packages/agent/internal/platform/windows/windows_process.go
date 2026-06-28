//go:build windows

package windows

import (
	"fmt"
	"net"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/pidcache"
)

// procMetaCache collapses repeated Win32 process lookups for the same PID:
// a browser opening many connections from one process resolves its image
// name + token user (the latter can RPC to a domain controller in AD
// environments) once per TTL instead of per connection.
var procMetaCache = pidcache.New()

// iphlpapi.dll functions for TCP table queries
var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")

	kernel32DLL                    = windows.NewLazySystemDLL("kernel32.dll")
	procQueryFullProcessImageNameW = kernel32DLL.NewProc("QueryFullProcessImageNameW")
)

// TCP_TABLE_OWNER_PID_CONNECTIONS = 4 requests connections with owning PID.
const (
	tcpTableOwnerPidConnections = 4
	afINET                      = 2 // AF_INET
)

// mibTCPRowOwnerPID maps to MIB_TCPROW_OWNER_PID.
type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  [4]byte
	LocalPort  uint32 // network byte order, only low 16 bits used
	RemoteAddr [4]byte
	RemotePort uint32 // network byte order, only low 16 bits used
	OwningPid  uint32
}

// ProcessInfo resolves process metadata using Win32 APIs, cached by PID.
func (p *WindowsPlatform) ProcessInfo(pid int) (api.ProcessMeta, error) {
	return procMetaCache.Get(pid, processInfoUncached)
}

// processInfoUncached does the raw Win32 lookup for one PID.
func processInfoUncached(pid int) (api.ProcessMeta, error) {
	meta := api.ProcessMeta{PID: pid}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return meta, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)

	// Parent PID off the same handle (no system-wide process snapshot).
	// Resolved here (not in the kernel driver) so the kernel
	// NexusFlowAuditEntry.parentPid field can stay reserved.
	meta.ParentPID = queryParentPID(h)

	// Executable path via QueryFullProcessImageNameW
	meta.Path = queryProcessImageName(h)
	if meta.Path != "" {
		meta.Name = filepath.Base(meta.Path)
	}

	// Process owner via OpenProcessToken + GetTokenUser
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err == nil {
		defer token.Close()
		if tokenUser, err := token.GetTokenUser(); err == nil {
			account, domain, _, err := tokenUser.User.Sid.LookupAccount("")
			if err == nil {
				meta.User = domain + `\` + account
			}
		}
	}

	if meta.Path == "" {
		return meta, fmt.Errorf("cannot resolve image name for pid %d", pid)
	}
	return meta, nil
}

func queryProcessImageName(h windows.Handle) string {
	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(
		uintptr(h),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// queryParentPID returns the parent process ID for the process referenced by
// h via NtQueryInformationProcess(ProcessBasicInformation) — a single query
// against the open handle, not a system-wide process-table walk. Returns 0 if
// it cannot be resolved.
func queryParentPID(h windows.Handle) int {
	var pbi windows.PROCESS_BASIC_INFORMATION
	var retLen uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation,
		unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &retLen); err != nil {
		return 0
	}
	return int(pbi.InheritedFromUniqueProcessId)
}

// findOwnerPID looks up the PID owning a local TCP endpoint via
// GetExtendedTcpTable (iphlpapi.dll).
func findOwnerPID(localIP net.IP, localPort int) int {
	ip4 := localIP.To4()
	if ip4 == nil {
		return 0
	}

	// First call to determine buffer size
	var size uint32
	procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 1,
		uintptr(afINET), uintptr(tcpTableOwnerPidConnections), 0)
	if size == 0 {
		return 0
	}

	buf := make([]byte, size)
	r, _, _ := procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		1,
		uintptr(afINET),
		uintptr(tcpTableOwnerPidConnections),
		0,
	)
	if r != 0 {
		return 0
	}

	numEntries := *(*uint32)(unsafe.Pointer(&buf[0]))
	const rowSize = unsafe.Sizeof(mibTCPRowOwnerPID{})
	offset := unsafe.Sizeof(uint32(0)) // skip dwNumEntries
	bufLen := uintptr(len(buf))

	for i := uint32(0); i < numEntries; i++ {
		if offset+rowSize > bufLen {
			break // guard against TOCTOU: table grew between sizing and data calls
		}
		row := (*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[offset]))
		offset += rowSize

		// Compare local address and port (port stored in network byte order)
		rowPort := int(row.LocalPort>>8 | (row.LocalPort&0xFF)<<8)
		if rowPort == localPort &&
			row.LocalAddr[0] == ip4[0] &&
			row.LocalAddr[1] == ip4[1] &&
			row.LocalAddr[2] == ip4[2] &&
			row.LocalAddr[3] == ip4[3] {
			return int(row.OwningPid)
		}
	}
	return 0
}
