//go:build windows

// wfp_ioctl.go — low-level DeviceIoControl wrappers for the five
// NexusWFP IOCTLs.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §6
// SDD: docs/developers/specs/e59-s2-usermode-go-integration.md §T1
//
// CTL_CODE(FileDeviceNetwork=0x12, Function, Method, Access). All
// match the C macros in nexus-wfp-driver/Common.h.

package windows

import (
	"encoding/binary"
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fileDeviceNetwork uint32 = 0x12
	fileAnyAccess     uint32 = 0
	methodBuffered    uint32 = 0
	methodOutDirect   uint32 = 2
)

// ctlCode mirrors the Windows CTL_CODE macro.
func ctlCode(deviceType, function, method, access uint32) uint32 {
	return (deviceType << 16) | (access << 14) | (function << 2) | method
}

var (
	ioctlNexusWfpHello        = ctlCode(fileDeviceNetwork, 0x800, methodBuffered, fileAnyAccess)
	ioctlNexusWfpSetProxyPort = ctlCode(fileDeviceNetwork, 0x801, methodBuffered, fileAnyAccess)
	ioctlNexusWfpPushPolicy   = ctlCode(fileDeviceNetwork, 0x802, methodBuffered, fileAnyAccess)
	ioctlNexusWfpGetOrigDst   = ctlCode(fileDeviceNetwork, 0x803, methodBuffered, fileAnyAccess)
	ioctlNexusWfpAuditPump    = ctlCode(fileDeviceNetwork, 0x804, methodOutDirect, fileAnyAccess)
)

const nexusWfpDevicePath = `\\.\NexusWFP`

type helloRequest struct {
	ProtocolVersion uint32
	AgentPID        uint32
}

type helloResponse struct {
	DriverProtocolVersion uint32
	Capabilities          uint32
	DriverBuildID         uint64
}

// openDevice opens the NexusWFP control device with FILE_FLAG_OVERLAPPED
// so subsequent DeviceIoControl calls can be asynchronous (required by
// the audit pump's OVERLAPPED I/O completion port pattern).
func openDevice() (windows.Handle, error) {
	pPath, err := syscall.UTF16PtrFromString(nexusWfpDevicePath)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(
		pPath,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // exclusive
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return 0, err
	}
	return h, nil
}

func ioctlHello(handle windows.Handle, req helloRequest) (helloResponse, error) {
	in := make([]byte, 8)
	binary.LittleEndian.PutUint32(in[0:], req.ProtocolVersion)
	binary.LittleEndian.PutUint32(in[4:], req.AgentPID)

	out := make([]byte, 16)
	var bytesReturned uint32
	if err := windows.DeviceIoControl(
		handle,
		ioctlNexusWfpHello,
		&in[0], uint32(len(in)),
		&out[0], uint32(len(out)),
		&bytesReturned, nil,
	); err != nil {
		return helloResponse{}, err
	}
	if bytesReturned < uint32(unsafe.Sizeof(helloResponse{})) {
		return helloResponse{}, errors.New("wfp: HELLO short response")
	}
	return helloResponse{
		DriverProtocolVersion: binary.LittleEndian.Uint32(out[0:]),
		Capabilities:          binary.LittleEndian.Uint32(out[4:]),
		DriverBuildID:         binary.LittleEndian.Uint64(out[8:]),
	}, nil
}

func ioctlSetProxyPort(handle windows.Handle, tcpPort, udpPort uint16) error {
	if tcpPort == 0 || udpPort == 0 || tcpPort != udpPort {
		return errors.New("wfp: TCP and UDP proxy ports must be equal and non-zero")
	}
	in := make([]byte, 4)
	binary.LittleEndian.PutUint16(in[0:], tcpPort)
	binary.LittleEndian.PutUint16(in[2:], udpPort)
	var bytesReturned uint32
	return windows.DeviceIoControl(
		handle,
		ioctlNexusWfpSetProxyPort,
		&in[0], uint32(len(in)),
		nil, 0,
		&bytesReturned, nil,
	)
}

func ioctlPushPolicy(handle windows.Handle, body []byte) error {
	if len(body) == 0 {
		return errors.New("wfp: empty policy body")
	}
	var bytesReturned uint32
	return windows.DeviceIoControl(
		handle,
		ioctlNexusWfpPushPolicy,
		&body[0], uint32(len(body)),
		nil, 0,
		&bytesReturned, nil,
	)
}

type getOrigDstRequest struct {
	LocalPort uint16
	IsUDP     uint8
	Reserved  uint8
}

type getOrigDstResponse struct {
	Family      uint8
	Reserved    [3]uint8
	OrigDstAddr [16]byte
	OrigDstPort uint16
	Reserved2   uint16
	ProcessID   uint32
}

func ioctlGetOrigDst(handle windows.Handle, req getOrigDstRequest) (getOrigDstResponse, bool, error) {
	in := make([]byte, 4)
	binary.LittleEndian.PutUint16(in[0:], req.LocalPort)
	in[2] = req.IsUDP

	out := make([]byte, 28)
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		ioctlNexusWfpGetOrigDst,
		&in[0], uint32(len(in)),
		&out[0], uint32(len(out)),
		&bytesReturned, nil,
	)
	if errors.Is(err, windows.ERROR_NOT_FOUND) {
		return getOrigDstResponse{}, false, nil
	}
	if err != nil {
		return getOrigDstResponse{}, false, err
	}
	if bytesReturned < uint32(unsafe.Sizeof(getOrigDstResponse{})) {
		return getOrigDstResponse{}, false, errors.New("wfp: GET_ORIG_DST short response")
	}
	var resp getOrigDstResponse
	resp.Family = out[0]
	copy(resp.OrigDstAddr[:], out[4:20])
	resp.OrigDstPort = binary.LittleEndian.Uint16(out[20:])
	resp.ProcessID = binary.LittleEndian.Uint32(out[24:])
	return resp, true, nil
}

// ioctlPostAuditPump issues an OVERLAPPED audit-pump IRP. Returns
// ERROR_IO_PENDING (the kernel pended the IRP); the completion lands
// on the IO completion port. Used only by wfp_audit_pump.go.
func ioctlPostAuditPump(handle windows.Handle, buf []byte, overlapped *windows.Overlapped) error {
	var bytesReturned uint32
	err := windows.DeviceIoControl(
		handle,
		ioctlNexusWfpAuditPump,
		nil, 0,
		&buf[0], uint32(len(buf)),
		&bytesReturned, overlapped,
	)
	if err == nil || errors.Is(err, windows.ERROR_IO_PENDING) {
		return nil
	}
	return err
}
