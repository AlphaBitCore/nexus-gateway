// Ioctl.c — IOCTL dispatch for the NexusWFP control surface.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §6
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md §T4 + impl

#include "Common.h"

// Shared with Callouts.c — read on every connect.
volatile UINT16 g_TcpProxyPort = 0;
volatile UINT16 g_UdpProxyPort = 0;

// Set by NexusAuditQueueInit(); reused by HandleAuditPump.
extern WDFQUEUE g_AuditPumpQueue;
WDFQUEUE g_AuditPumpQueue = NULL;

static NTSTATUS
NexusGetInputBuffer(
    _In_  WDFREQUEST  Request,
    _In_  size_t      Required,
    _Out_ PVOID*      Buffer)
{
    size_t actualLen = 0;
    NTSTATUS status = WdfRequestRetrieveInputBuffer(Request, Required, Buffer, &actualLen);
    if (!NT_SUCCESS(status)) return status;
    if (actualLen < Required)  return STATUS_BUFFER_TOO_SMALL;
    return STATUS_SUCCESS;
}

static NTSTATUS
NexusGetOutputBuffer(
    _In_  WDFREQUEST  Request,
    _In_  size_t      Required,
    _Out_ PVOID*      Buffer)
{
    size_t actualLen = 0;
    NTSTATUS status = WdfRequestRetrieveOutputBuffer(Request, Required, Buffer, &actualLen);
    if (!NT_SUCCESS(status)) return status;
    if (actualLen < Required)  return STATUS_BUFFER_TOO_SMALL;
    return STATUS_SUCCESS;
}

static NTSTATUS
HandleHello(_In_ WDFREQUEST Request)
{
    NexusHelloRequest*  in;
    NexusHelloResponse* out;

    NTSTATUS status = NexusGetInputBuffer(Request, sizeof(*in), (PVOID*)&in);
    if (!NT_SUCCESS(status)) return status;
    status = NexusGetOutputBuffer(Request, sizeof(*out), (PVOID*)&out);
    if (!NT_SUCCESS(status)) return status;

    if (in->protocolVersion != NEXUS_WFP_PROTOCOL_VERSION) {
        return STATUS_REVISION_MISMATCH;
    }
    if (in->agentPid == 0) {
        return STATUS_INVALID_PARAMETER;
    }

    NexusPolicySetSelfPid(in->agentPid);

    out->driverProtocolVersion = NEXUS_WFP_PROTOCOL_VERSION;
    out->capabilities = NEXUS_CAP_IPV6_REDIRECT
                      | NEXUS_CAP_UDP_REDIRECT
                      | NEXUS_CAP_KILL_SWITCH
                      | NEXUS_CAP_QUIC_FALLBACK;
    out->driverBuildId = 0;

    WdfRequestCompleteWithInformation(Request, STATUS_SUCCESS, sizeof(*out));
    return STATUS_SUCCESS;
}

static NTSTATUS
HandleSetProxyPort(_In_ WDFREQUEST Request)
{
    NexusSetProxyPortRequest* in;
    NTSTATUS status = NexusGetInputBuffer(Request, sizeof(*in), (PVOID*)&in);
    if (!NT_SUCCESS(status)) return status;

    if (in->tcpPort == 0 || in->udpPort == 0 || in->tcpPort != in->udpPort) {
        return STATUS_INVALID_PARAMETER;
    }

    InterlockedExchange16((volatile SHORT*)&g_TcpProxyPort, (SHORT)in->tcpPort);
    InterlockedExchange16((volatile SHORT*)&g_UdpProxyPort, (SHORT)in->udpPort);

    WdfRequestComplete(Request, STATUS_SUCCESS);
    return STATUS_SUCCESS;
}

static NTSTATUS
HandlePushPolicy(_In_ WDFREQUEST Request, _In_ size_t InputBufferLength)
{
    PVOID  body;
    size_t actual = 0;
    NTSTATUS status = WdfRequestRetrieveInputBuffer(
        Request, sizeof(NexusPolicyHeader), &body, &actual);
    if (!NT_SUCCESS(status)) return status;
    if (actual < sizeof(NexusPolicyHeader)) return STATUS_BUFFER_TOO_SMALL;

    status = NexusPolicyApply(body, (ULONG)InputBufferLength);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    WdfRequestComplete(Request, STATUS_SUCCESS);
    return STATUS_SUCCESS;
}

static NTSTATUS
HandleGetOrigDst(_In_ WDFREQUEST Request)
{
    NexusGetOrigDstRequest*  in;
    NexusGetOrigDstResponse* out;

    NTSTATUS status = NexusGetInputBuffer(Request, sizeof(*in), (PVOID*)&in);
    if (!NT_SUCCESS(status)) return status;
    status = NexusGetOutputBuffer(Request, sizeof(*out), (PVOID*)&out);
    if (!NT_SUCCESS(status)) return status;

    status = NexusFlowTableLookup(in->localPort, in->isUdp != 0, out);
    if (status == STATUS_NOT_FOUND) {
        // Return success with zero-filled response and 0 bytes
        // information; caller treats it as a miss.
        WdfRequestCompleteWithInformation(Request, STATUS_NOT_FOUND, 0);
        return STATUS_SUCCESS;
    }
    if (!NT_SUCCESS(status)) {
        return status;
    }

    // Consume on hit: each redirected connection is accepted by the
    // proxy exactly once, so the entry's purpose is served. Removing
    // it here means a later ephemeral-port reuse can never read THIS
    // flow's stale original destination — the TTL sweep then only
    // covers entries whose lookup never came.
    NexusFlowTableRemove(in->localPort, in->isUdp != 0);

    WdfRequestCompleteWithInformation(Request, STATUS_SUCCESS, sizeof(*out));
    return STATUS_SUCCESS;
}

static NTSTATUS
HandleAuditPump(_In_ WDFREQUEST Request)
{
    if (g_AuditPumpQueue == NULL) {
        return STATUS_DEVICE_NOT_READY;
    }

    NTSTATUS status = WdfRequestForwardToIoQueue(Request, g_AuditPumpQueue);
    if (!NT_SUCCESS(status)) {
        return status;
    }
    // Deliver any records buffered while no IRP was pended. Without
    // this, records enqueued during the gap sit until the NEXT emit —
    // on a quiet host that can be minutes of audit latency.
    NexusAuditDrainPending();
    return STATUS_PENDING;
}

NTSTATUS
NexusWfpDispatchIoctl(
    _In_ WDFREQUEST Request,
    _In_ size_t OutputBufferLength,
    _In_ size_t InputBufferLength,
    _In_ ULONG IoControlCode)
{
    NTSTATUS status;

    UNREFERENCED_PARAMETER(OutputBufferLength);

    switch (IoControlCode) {
    case IOCTL_NEXUS_WFP_HELLO:
        status = HandleHello(Request);
        break;
    case IOCTL_NEXUS_WFP_SET_PROXY_PORT:
        status = HandleSetProxyPort(Request);
        break;
    case IOCTL_NEXUS_WFP_PUSH_POLICY:
        status = HandlePushPolicy(Request, InputBufferLength);
        break;
    case IOCTL_NEXUS_WFP_GET_ORIG_DST:
        status = HandleGetOrigDst(Request);
        break;
    case IOCTL_NEXUS_WFP_AUDIT_PUMP:
        status = HandleAuditPump(Request);
        if (status == STATUS_PENDING) {
            return status;
        }
        break;
    default:
        status = STATUS_INVALID_DEVICE_REQUEST;
        break;
    }

    if (status != STATUS_PENDING && !NT_SUCCESS(status)) {
        WdfRequestComplete(Request, status);
    }
    return status;
}
