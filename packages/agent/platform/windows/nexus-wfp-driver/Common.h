// Common.h — shared definitions between the NexusWFP kernel driver and
// the user-mode Go agent client.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md
//
// Any change here is a contract break — bump NEXUS_WFP_PROTOCOL_VERSION,
// update the architecture doc IOCTL/wire tables in the SAME PR, and
// update the Go user-mode side (packages/agent/internal/platform/windows/
// wfp_*.go) in the same PR per CLAUDE.md code/doc lockstep.

#pragma once

#include <ntddk.h>
#include <wdf.h>
#include <fwpsk.h>
#include <fwpmk.h>

//
// Protocol version. Increment on any incompatible wire change to the
// IOCTL request/response structs or the PUSH_POLICY body layout.
//
#define NEXUS_WFP_PROTOCOL_VERSION 1u

//
// Driver-reported capability bits returned in NexusHelloResponse.
// New capabilities go in higher bits without bumping
// NEXUS_WFP_PROTOCOL_VERSION as long as the layouts below stay
// backwards-compatible.
//
#define NEXUS_CAP_IPV6_REDIRECT   0x00000001u
#define NEXUS_CAP_UDP_REDIRECT    0x00000002u
#define NEXUS_CAP_KILL_SWITCH     0x00000004u

//
// Device + symlink names.
//
#define NEXUS_WFP_DEVICE_NAME   L"\\Device\\NexusWFP"
#define NEXUS_WFP_SYMLINK_NAME  L"\\??\\NexusWFP"

//
// SCM service name (matches the INF [NexusWfpService_Inst] / wxs CA
// "sc.exe start NexusWFP").
//
#define NEXUS_WFP_SERVICE_NAME  L"NexusWFP"

//
// IOCTL codes. CTL_CODE(DeviceType, Function, Method, Access).
// FILE_DEVICE_NETWORK = 0x12.
// Function codes start at 0x800 (vendor-reserved range, per MSDN).
//
#define IOCTL_NEXUS_WFP_HELLO \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x800, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_SET_PROXY_PORT \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x801, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_PUSH_POLICY \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x802, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_GET_ORIG_DST \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x803, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_AUDIT_PUMP \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x804, METHOD_OUT_DIRECT, FILE_ANY_ACCESS)

//
// IOCTL_NEXUS_WFP_HELLO
//
#pragma pack(push, 1)
typedef struct _NexusHelloRequest {
    UINT32  protocolVersion;   // Client must set to NEXUS_WFP_PROTOCOL_VERSION.
    UINT32  agentPid;          // PID of the user-mode agent process; the
                               // driver hard-bypasses this PID (FR-9).
} NexusHelloRequest;

typedef struct _NexusHelloResponse {
    UINT32  driverProtocolVersion;
    UINT32  capabilities;      // Bit-set of NEXUS_CAP_*.
    UINT64  driverBuildId;     // Reserved; 0 for v1.
} NexusHelloResponse;
#pragma pack(pop)

//
// IOCTL_NEXUS_WFP_SET_PROXY_PORT
//
#pragma pack(push, 1)
typedef struct _NexusSetProxyPortRequest {
    UINT16  tcpPort;           // host byte order
    UINT16  udpPort;           // host byte order — MUST be same numeric
                               // port as tcpPort (architecture §5.2 binding)
} NexusSetProxyPortRequest;
#pragma pack(pop)

//
// IOCTL_NEXUS_WFP_PUSH_POLICY — body layout follows architecture §7.
// Variable-length; the driver caps every count field by a constant
// in NexusPolicyLimits to defend against malformed input (NFR-5).
//
#pragma pack(push, 1)
typedef struct _NexusPolicyHeader {
    UINT32  version;           // == NEXUS_WFP_PROTOCOL_VERSION
    UINT32  generation;        // monotonic per agent process
    UINT8   killSwitch;        // 0 or 1
    UINT8   reserved[3];
    UINT32  processBypassCount;
    UINT32  destBypassCount;
    // followed by:
    //   UINT32 processBypass[processBypassCount];
    //   NexusCidr destBypass[destBypassCount];
} NexusPolicyHeader;

typedef struct _NexusCidr {
    UINT8   family;            // AF_INET = 2, AF_INET6 = 23
    UINT8   prefixLen;
    UINT8   reserved[2];
    UINT8   addr[16];          // IPv4 in first 4 bytes, rest zero
} NexusCidr;
#pragma pack(pop)

#define NEXUS_MAX_PROCESS_BYPASS  256
#define NEXUS_MAX_DEST_BYPASS     1024

//
// IOCTL_NEXUS_WFP_GET_ORIG_DST
//
#pragma pack(push, 1)
typedef struct _NexusGetOrigDstRequest {
    UINT16  localPort;         // The proxy-side port (i.e. the port the
                               // app connected to after we redirected to
                               // 127.0.0.1:proxyPort). Host byte order.
    UINT8   isUdp;             // 0 = TCP, 1 = UDP
    UINT8   reserved;
} NexusGetOrigDstRequest;

typedef struct _NexusGetOrigDstResponse {
    UINT8   family;            // AF_INET / AF_INET6
    UINT8   reserved[3];
    UINT8   origDstAddr[16];   // IPv4 in first 4 bytes for AF_INET
    UINT16  origDstPort;       // host byte order
    UINT16  reserved2;
    UINT32  processId;
} NexusGetOrigDstResponse;
#pragma pack(pop)

//
// IOCTL_NEXUS_WFP_AUDIT_PUMP — inverted-call pattern.
//
// The user-mode agent posts a batch of OVERLAPPED IRPs with a 4 KB
// output buffer each. The driver completes one IRP per redirect/block
// event, packing as many NexusFlowAuditEntry records as fit in 4 KB.
//
// The agent must immediately re-post the completed IRP to keep the
// queue full. NEXUS_AUDIT_IRP_DEPTH = 8 is the recommended depth
// (architecture §6).
//
#define NEXUS_AUDIT_BUFFER_SIZE   4096u
#define NEXUS_AUDIT_IRP_DEPTH     8u

typedef enum _NexusDecision {
    NexusDecisionRedirect = 1,
    NexusDecisionPermit   = 2,
    NexusDecisionBlock    = 3,
} NexusDecision;

#pragma pack(push, 1)
typedef struct _NexusFlowAuditEntry {
    UINT64  timestampUs;       // Microseconds since boot.
    UINT32  processId;
    UINT32  parentPid;
    UINT8   family;            // AF_INET / AF_INET6
    UINT8   protocol;          // IPPROTO_TCP / IPPROTO_UDP
    UINT8   decision;          // NexusDecision
    UINT8   reserved;
    UINT8   srcAddr[16];
    UINT16  srcPort;           // host byte order
    UINT16  reserved2;
    UINT8   origDstAddr[16];
    UINT16  origDstPort;       // host byte order
    UINT16  reserved3;
} NexusFlowAuditEntry;
#pragma pack(pop)

//
// Pool tag used by ExAllocatePool2 throughout the driver. Visible in
// poolmon as "NXWF".
//
#define NEXUS_WFP_POOL_TAG 'FWXN'

//
// In-driver policy snapshot. Hot path: callouts read this struct on
// every connect to decide redirect / block / permit. Pointer swap is
// atomic via InterlockedExchangePointer; the previous pointer is
// freed at PASSIVE_LEVEL via a deferred work-item.
//
typedef struct _NEXUS_POLICY {
    volatile LONG refCount;      // active readers + 1 for being-active.
                                 // Drops to 0 → safe to free.
    ULONG     generation;
    BOOLEAN   killSwitch;
    ULONG     processBypassCount;
    PULONG    processBypass;     // pointer to count UINT32 process IDs
    ULONG     destBypassCount;
    NexusCidr* destBypass;       // pointer to count NexusCidr entries
} NEXUS_POLICY, *PNEXUS_POLICY;

//
// PolicyState.c — atomic policy snapshot + lookup.
//
NTSTATUS NexusPolicyInit(VOID);
VOID     NexusPolicyShutdown(VOID);

// Apply a serialised policy body (architecture §7 layout) atomically.
// On success the new policy is active; the previous one is queued for
// PASSIVE-level free.
NTSTATUS NexusPolicyApply(
    _In_reads_bytes_(BufferLength) const VOID* Buffer,
    _In_ ULONG BufferLength);

// Lookup primitives called from inside DISPATCH-level callouts.
// They take a reference to the active policy snapshot via
// InterlockedIncrement on its refcount so the freeing path waits.
BOOLEAN  NexusPolicyKillSwitchActive(VOID);
BOOLEAN  NexusPolicyIsBypassedProcess(_In_ UINT32 ProcessId);
BOOLEAN  NexusPolicyIsBypassedDest(_In_ UINT8 Family, _In_reads_bytes_(16) const UINT8* Addr16);

// Self-PID bypass (FR-9). Set once at HELLO via Ioctl.c; survives all
// PUSH_POLICY generations.
VOID     NexusPolicySetSelfPid(_In_ UINT32 ProcessId);
BOOLEAN  NexusPolicyIsSelfPid(_In_ UINT32 ProcessId);

//
// FlowTable.c — kernel-mode hash map keyed by (srcPort, isUDP).
// Holds the original destination + processId for every flow we
// redirected. Lookup is called by Ioctl.c's GET_ORIG_DST handler.
//
NTSTATUS NexusFlowTableInit(VOID);
VOID     NexusFlowTableShutdown(VOID);

NTSTATUS NexusFlowTableInsert(
    _In_ UINT16 SrcPort,
    _In_ BOOLEAN IsUDP,
    _In_ UINT8 Family,
    _In_reads_bytes_(16) const UINT8* OrigDstAddr16,
    _In_ UINT16 OrigDstPort,
    _In_ UINT32 ProcessId);

// Lookup. Returns STATUS_NOT_FOUND if no entry, else STATUS_SUCCESS
// and fills OutEntry. Caller may copy fields out before returning.
NTSTATUS NexusFlowTableLookup(
    _In_  UINT16 SrcPort,
    _In_  BOOLEAN IsUDP,
    _Out_ NexusGetOrigDstResponse* OutEntry);

VOID NexusFlowTableRemove(_In_ UINT16 SrcPort, _In_ BOOLEAN IsUDP);

// Periodic sweeper; called from a WDF timer in Driver.c. Drops
// entries older than NEXUS_FLOW_TTL_SECONDS.
#define NEXUS_FLOW_TTL_SECONDS 300
VOID NexusFlowTableSweep(VOID);

//
// AuditQueue.c — kernel-side audit event queue + IRP completion.
// Producers (callouts) call NexusAuditEmit at DISPATCH; consumers
// (user-mode via IOCTL_NEXUS_WFP_AUDIT_PUMP) complete via
// NexusAuditPumpComplete pulled from the pended IRP queue.
//
NTSTATUS NexusAuditQueueInit(_In_ WDFDEVICE Device, _Out_ WDFQUEUE* OutAuditPumpQueue);
VOID     NexusAuditQueueShutdown(VOID);

VOID NexusAuditEmit(_In_ const NexusFlowAuditEntry* Entry);

//
// Internal forward decls (defined in each .c file).
//
extern WDFDEVICE g_NexusWfpDevice;

NTSTATUS NexusWfpFilterEngineOpen(VOID);
VOID     NexusWfpFilterEngineClose(VOID);
NTSTATUS NexusWfpRegisterAllCallouts(_In_ PDEVICE_OBJECT DeviceObject);
VOID     NexusWfpUnregisterAllCallouts(VOID);
NTSTATUS NexusWfpFilterAddAll(VOID);       // Add filters that bind callouts to layers.
VOID     NexusWfpFilterRemoveAll(VOID);

// Proxy ports (host byte order). Read by callouts on every connect.
extern volatile UINT16 g_TcpProxyPort;
extern volatile UINT16 g_UdpProxyPort;

NTSTATUS NexusWfpDispatchIoctl(
    _In_ WDFREQUEST Request,
    _In_ size_t OutputBufferLength,
    _In_ size_t InputBufferLength,
    _In_ ULONG IoControlCode);
