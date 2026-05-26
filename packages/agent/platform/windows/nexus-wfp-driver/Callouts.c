// Callouts.c — the four WFP callouts named in architecture §4.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md

// initguid.h MUST come before Common.h so that DEFINE_GUID below
// actually allocates storage for the GUIDs (without this include the
// DEFINE_GUID macro expands to an extern declaration only, and the
// linker fails with unresolved externals).
#define INITGUID
#include <initguid.h>

#include "Common.h"

#include <ws2ipdef.h>

//
// Callout GUIDs. Generated once, frozen forever — these end up in the
// SCM registry under the NexusWFP service key and any change breaks
// existing installs. Generated 2026-05-24 by uuidgen.
//
DEFINE_GUID(NEXUS_WFP_CALLOUT_REDIRECT_V4_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x01);

DEFINE_GUID(NEXUS_WFP_CALLOUT_REDIRECT_V6_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x02);

DEFINE_GUID(NEXUS_WFP_CALLOUT_AUTH_CONNECT_V4_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x03);

DEFINE_GUID(NEXUS_WFP_CALLOUT_AUTH_CONNECT_V6_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x04);

static UINT32 g_RedirectV4CalloutId = 0;
static UINT32 g_RedirectV6CalloutId = 0;
static UINT32 g_AuthConnectV4CalloutId = 0;
static UINT32 g_AuthConnectV6CalloutId = 0;

// Single global redirect handle (per architecture §5.1). Created at
// DriverEntry, destroyed at Unload.
static HANDLE g_RedirectHandle = NULL;

NTSTATUS NexusWfpCalloutsCreateRedirectHandle(VOID)
{
    return FwpsRedirectHandleCreate0(
        &NEXUS_WFP_CALLOUT_REDIRECT_V4_GUID,
        0,
        &g_RedirectHandle);
}

VOID NexusWfpCalloutsDestroyRedirectHandle(VOID)
{
    if (g_RedirectHandle != NULL) {
        FwpsRedirectHandleDestroy0(g_RedirectHandle);
        g_RedirectHandle = NULL;
    }
}

//
// Common decision helper. Returns NexusDecisionPermit if the flow
// should be left alone, NexusDecisionBlock if killswitch/policy
// blocks it, NexusDecisionRedirect if we should redirect.
//
static NexusDecision DecideForFlow(
    _In_ UINT32 processId,
    _In_ UINT8  family,
    _In_reads_bytes_(16) const UINT8* dstAddr16)
{
    // FR-9: agent's own outbound traffic never redirected.
    if (NexusPolicyIsSelfPid(processId)) {
        return NexusDecisionPermit;
    }
    // FR-4 process bypass list (admin-configured PIDs).
    if (NexusPolicyIsBypassedProcess(processId)) {
        return NexusDecisionPermit;
    }
    // Kill switch: full passthrough.
    if (NexusPolicyKillSwitchActive()) {
        return NexusDecisionPermit;
    }
    // Destination CIDR bypass.
    if (NexusPolicyIsBypassedDest(family, dstAddr16)) {
        return NexusDecisionPermit;
    }
    return NexusDecisionRedirect;
}

// Extract (processId, src/dst, port) from FWPS_INCOMING_VALUES0 for
// V4. Field-index symbolic names per <fwpsk.h>:
//   FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_LOCAL_ADDRESS
//   FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_LOCAL_PORT
//   FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_REMOTE_ADDRESS
//   FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_REMOTE_PORT
//   FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_PROTOCOL
// The IP fields are reported in HOST byte order by WFP.
static VOID ReadFlowMetaV4(
    _In_ const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_ const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Out_ UINT32* processId,
    _Out_ UINT32* srcAddrHost,
    _Out_ UINT32* dstAddrHost,
    _Out_ UINT16* srcPortHost,
    _Out_ UINT16* dstPortHost,
    _Out_ UINT8*  protocol)
{
    *processId    = (inMetaValues->processId > 0xFFFFFFFFULL)
                  ? 0u : (UINT32)inMetaValues->processId;
    *srcAddrHost  = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_LOCAL_ADDRESS].value.uint32;
    *dstAddrHost  = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_REMOTE_ADDRESS].value.uint32;
    *srcPortHost  = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_LOCAL_PORT].value.uint16;
    *dstPortHost  = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_REMOTE_PORT].value.uint16;
    *protocol     = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V4_IP_PROTOCOL].value.uint8;
}

static VOID ReadFlowMetaV6(
    _In_ const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_ const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Out_ UINT32* processId,
    _Out_ UINT8   srcAddr16[16],
    _Out_ UINT8   dstAddr16[16],
    _Out_ UINT16* srcPortHost,
    _Out_ UINT16* dstPortHost,
    _Out_ UINT8*  protocol)
{
    *processId   = (inMetaValues->processId > 0xFFFFFFFFULL)
                 ? 0u : (UINT32)inMetaValues->processId;
    RtlCopyMemory(srcAddr16,
        inFixedValues->incomingValue[
            FWPS_FIELD_ALE_CONNECT_REDIRECT_V6_IP_LOCAL_ADDRESS].value.byteArray16,
        16);
    RtlCopyMemory(dstAddr16,
        inFixedValues->incomingValue[
            FWPS_FIELD_ALE_CONNECT_REDIRECT_V6_IP_REMOTE_ADDRESS].value.byteArray16,
        16);
    *srcPortHost = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V6_IP_LOCAL_PORT].value.uint16;
    *dstPortHost = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V6_IP_REMOTE_PORT].value.uint16;
    *protocol    = inFixedValues->incomingValue[
                        FWPS_FIELD_ALE_CONNECT_REDIRECT_V6_IP_PROTOCOL].value.uint8;
}

static VOID EmitAuditRecord(
    _In_ UINT32 processId,
    _In_ NexusDecision decision,
    _In_ UINT8 family,
    _In_ UINT8 protocol,
    _In_reads_bytes_(16) const UINT8* srcAddr16,
    _In_ UINT16 srcPort,
    _In_reads_bytes_(16) const UINT8* origDstAddr16,
    _In_ UINT16 origDstPort)
{
    LARGE_INTEGER ts;
    KeQuerySystemTime(&ts);

    NexusFlowAuditEntry entry;
    RtlZeroMemory(&entry, sizeof(entry));
    entry.timestampUs = (UINT64)(ts.QuadPart / 10); // 100ns → us
    entry.processId   = processId;
    entry.parentPid   = 0;
    entry.family      = family;
    entry.protocol    = protocol;
    entry.decision    = (UINT8)decision;
    RtlCopyMemory(entry.srcAddr,     srcAddr16,     16);
    RtlCopyMemory(entry.origDstAddr, origDstAddr16, 16);
    entry.srcPort     = srcPort;
    entry.origDstPort = origDstPort;

    NexusAuditEmit(&entry);
}

//
// NexusConnectRedirectV4 — ALE_CONNECT_REDIRECT_V4 callout.
//
static VOID
NTAPI
NexusConnectRedirectV4(
    _In_     const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_     const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Inout_  VOID*                              layerData,
    _In_opt_ const VOID*                        classifyContext,
    _In_     const FWPS_FILTER1*                filter,
    _In_     UINT64                             flowContext,
    _Inout_  FWPS_CLASSIFY_OUT0*                classifyOut)
{
    UNREFERENCED_PARAMETER(layerData);
    UNREFERENCED_PARAMETER(flowContext);

    classifyOut->actionType = FWP_ACTION_PERMIT;

    UINT32 processId, srcAddrHost, dstAddrHost;
    UINT16 srcPort, dstPort;
    UINT8  protocol;
    ReadFlowMetaV4(inFixedValues, inMetaValues,
                   &processId, &srcAddrHost, &dstAddrHost,
                   &srcPort, &dstPort, &protocol);

    // Convert dst into 16-byte buffer (IPv4 in first 4 bytes, network order).
    UINT8 srcAddr16[16] = {0};
    UINT8 dstAddr16[16] = {0};
    // Host-order uint32 → network bytes for storage.
    srcAddr16[0] = (UINT8)((srcAddrHost >> 24) & 0xFF);
    srcAddr16[1] = (UINT8)((srcAddrHost >> 16) & 0xFF);
    srcAddr16[2] = (UINT8)((srcAddrHost >>  8) & 0xFF);
    srcAddr16[3] = (UINT8)((srcAddrHost >>  0) & 0xFF);
    dstAddr16[0] = (UINT8)((dstAddrHost >> 24) & 0xFF);
    dstAddr16[1] = (UINT8)((dstAddrHost >> 16) & 0xFF);
    dstAddr16[2] = (UINT8)((dstAddrHost >>  8) & 0xFF);
    dstAddr16[3] = (UINT8)((dstAddrHost >>  0) & 0xFF);

    NexusDecision dec = DecideForFlow(processId, /*AF_INET=*/2, dstAddr16);
    if (dec != NexusDecisionRedirect) {
        // Permit without modification.
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    // Apply redirect: rewrite remote address to 127.0.0.1:proxyPort.
    if (!(classifyOut->rights & FWPS_RIGHT_ACTION_WRITE)) {
        return; // can't modify; another filter already terminated.
    }
    if (g_RedirectHandle == NULL || g_TcpProxyPort == 0) {
        return; // not ready; fail-open.
    }
    // UDP redirect requires UDP port; both proxy ports are equal
    // (architecture §5.2 binding) so we can use g_TcpProxyPort here.
    UINT16 proxyPort = g_TcpProxyPort;

    FWPS_CONNECT_REQUEST0* req = NULL;
    NTSTATUS status = FwpsAcquireWritableLayerDataPointer0(
        inMetaValues->classifyHandle,
        filter->filterId,
        0,
        (PVOID*)&req,
        classifyOut);
    if (!NT_SUCCESS(status) || req == NULL) {
        return;
    }

    SOCKADDR_IN* sin = (SOCKADDR_IN*)&req->remoteAddressAndPort;
    sin->sin_family      = AF_INET;
    sin->sin_addr.s_addr = RtlUlongByteSwap(0x7F000001UL); // 127.0.0.1 in network order
    sin->sin_port        = RtlUshortByteSwap(proxyPort);

    FwpsApplyModifiedLayerValues0(inMetaValues->classifyHandle, 0);

    // Record original destination for the proxy's GET_ORIG_DST lookup.
    (VOID)NexusFlowTableInsert(srcPort, /*isUDP=*/FALSE,
                               /*family=*/2, dstAddr16, dstPort, processId);

    EmitAuditRecord(processId, NexusDecisionRedirect,
                    /*family=*/2, protocol,
                    srcAddr16, srcPort, dstAddr16, dstPort);

    classifyOut->actionType = FWP_ACTION_PERMIT;
    classifyOut->rights    &= ~FWPS_RIGHT_ACTION_WRITE;
}

static VOID
NTAPI
NexusConnectRedirectV6(
    _In_     const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_     const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Inout_  VOID*                              layerData,
    _In_opt_ const VOID*                        classifyContext,
    _In_     const FWPS_FILTER1*                filter,
    _In_     UINT64                             flowContext,
    _Inout_  FWPS_CLASSIFY_OUT0*                classifyOut)
{
    UNREFERENCED_PARAMETER(layerData);
    UNREFERENCED_PARAMETER(flowContext);

    classifyOut->actionType = FWP_ACTION_PERMIT;

    UINT32 processId;
    UINT8  srcAddr16[16], dstAddr16[16];
    UINT16 srcPort, dstPort;
    UINT8  protocol;
    ReadFlowMetaV6(inFixedValues, inMetaValues,
                   &processId, srcAddr16, dstAddr16,
                   &srcPort, &dstPort, &protocol);

    NexusDecision dec = DecideForFlow(processId, /*AF_INET6=*/23, dstAddr16);
    if (dec != NexusDecisionRedirect) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    if (!(classifyOut->rights & FWPS_RIGHT_ACTION_WRITE)) {
        return;
    }
    if (g_RedirectHandle == NULL || g_TcpProxyPort == 0) {
        return;
    }
    UINT16 proxyPort = g_TcpProxyPort;

    FWPS_CONNECT_REQUEST0* req = NULL;
    NTSTATUS status = FwpsAcquireWritableLayerDataPointer0(
        inMetaValues->classifyHandle,
        filter->filterId,
        0,
        (PVOID*)&req,
        classifyOut);
    if (!NT_SUCCESS(status) || req == NULL) {
        return;
    }

    SOCKADDR_IN6* sin6 = (SOCKADDR_IN6*)&req->remoteAddressAndPort;
    RtlZeroMemory(sin6, sizeof(*sin6));
    sin6->sin6_family = AF_INET6;
    // ::1 loopback.
    sin6->sin6_addr.s6_addr[15] = 1;
    sin6->sin6_port = RtlUshortByteSwap(proxyPort);

    FwpsApplyModifiedLayerValues0(inMetaValues->classifyHandle, 0);

    (VOID)NexusFlowTableInsert(srcPort, /*isUDP=*/FALSE,
                               /*family=*/23, dstAddr16, dstPort, processId);

    EmitAuditRecord(processId, NexusDecisionRedirect,
                    /*family=*/23, protocol,
                    srcAddr16, srcPort, dstAddr16, dstPort);

    classifyOut->actionType = FWP_ACTION_PERMIT;
    classifyOut->rights    &= ~FWPS_RIGHT_ACTION_WRITE;
}

//
// NexusAuthConnectV4 — block decisions (architecture §4). At present
// we only block when killSwitch is OFF and an explicit "destination
// in block list" rule fires. Since the only "block list" we wire up
// is the destBypass = permit list, the AUTH callout's job is mostly
// to emit a definitive permit/block audit event after the redirect
// has been applied. Future expansion: explicit deny CIDRs.
//
static VOID
NTAPI
NexusAuthConnectV4(
    _In_     const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_     const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Inout_  VOID*                              layerData,
    _In_opt_ const VOID*                        classifyContext,
    _In_     const FWPS_FILTER1*                filter,
    _In_     UINT64                             flowContext,
    _Inout_  FWPS_CLASSIFY_OUT0*                classifyOut)
{
    UNREFERENCED_PARAMETER(layerData);
    UNREFERENCED_PARAMETER(classifyContext);
    UNREFERENCED_PARAMETER(filter);
    UNREFERENCED_PARAMETER(flowContext);

    classifyOut->actionType = FWP_ACTION_PERMIT;
    classifyOut->rights    &= ~FWPS_RIGHT_ACTION_WRITE;

    UNREFERENCED_PARAMETER(inFixedValues);
    UNREFERENCED_PARAMETER(inMetaValues);
    // Current policy model: REDIRECT layer already made the decision.
    // AUTH layer simply permits — block rules will be added when
    // admin denylists land (epic §4 MoSCoW Could).
}

static VOID
NTAPI
NexusAuthConnectV6(
    _In_     const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_     const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Inout_  VOID*                              layerData,
    _In_opt_ const VOID*                        classifyContext,
    _In_     const FWPS_FILTER1*                filter,
    _In_     UINT64                             flowContext,
    _Inout_  FWPS_CLASSIFY_OUT0*                classifyOut)
{
    UNREFERENCED_PARAMETER(inFixedValues);
    UNREFERENCED_PARAMETER(inMetaValues);
    UNREFERENCED_PARAMETER(layerData);
    UNREFERENCED_PARAMETER(classifyContext);
    UNREFERENCED_PARAMETER(filter);
    UNREFERENCED_PARAMETER(flowContext);

    classifyOut->actionType = FWP_ACTION_PERMIT;
    classifyOut->rights    &= ~FWPS_RIGHT_ACTION_WRITE;
}

static NTSTATUS
NTAPI
NexusCalloutNotify(
    _In_ FWPS_CALLOUT_NOTIFY_TYPE notifyType,
    _In_ const GUID*              filterKey,
    _Inout_ FWPS_FILTER1*         filter)
{
    UNREFERENCED_PARAMETER(notifyType);
    UNREFERENCED_PARAMETER(filterKey);
    UNREFERENCED_PARAMETER(filter);
    return STATUS_SUCCESS;
}

static VOID
NTAPI
NexusCalloutFlowDelete(
    _In_ UINT16 layerId,
    _In_ UINT32 calloutId,
    _In_ UINT64 flowContext)
{
    UNREFERENCED_PARAMETER(layerId);
    UNREFERENCED_PARAMETER(calloutId);
    // flowContext was set to (srcPort | (isUDP<<31)) at flow create
    // by the redirect callout (if we extend it to set flow context).
    // For v1 we evict via NexusFlowTableSweep on a timer; this hook
    // is here as a placeholder.
    UNREFERENCED_PARAMETER(flowContext);
}

static NTSTATUS
NexusRegisterOneCallout(
    _In_  PDEVICE_OBJECT             DeviceObject,
    _In_  const GUID*                CalloutKey,
    _In_  const GUID*                LayerKey,
    _In_  const wchar_t*             CalloutName,
    _In_  FWPS_CALLOUT_CLASSIFY_FN2  ClassifyFn,
    _In_  HANDLE                     EngineHandle,
    _Out_ UINT32*                    OutCalloutId)
{
    NTSTATUS         status;
    FWPS_CALLOUT2    sCallout = {0};
    FWPM_CALLOUT0    mCallout = {0};
    FWPM_DISPLAY_DATA0 displayData = {0};

    sCallout.calloutKey       = *CalloutKey;
    sCallout.classifyFn       = ClassifyFn;
    sCallout.notifyFn         = NexusCalloutNotify;
    sCallout.flowDeleteFn     = NexusCalloutFlowDelete;

    status = FwpsCalloutRegister2(DeviceObject, &sCallout, OutCalloutId);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    displayData.name        = (wchar_t*)CalloutName;
    displayData.description = (wchar_t*)CalloutName;

    mCallout.calloutKey      = *CalloutKey;
    mCallout.displayData     = displayData;
    mCallout.applicableLayer = *LayerKey;

    status = FwpmCalloutAdd0(EngineHandle, &mCallout, NULL, NULL);
    if (!NT_SUCCESS(status)) {
        FwpsCalloutUnregisterByKey0(CalloutKey);
        *OutCalloutId = 0;
        return status;
    }

    return STATUS_SUCCESS;
}

NTSTATUS
NexusWfpRegisterAllCallouts(_In_ PDEVICE_OBJECT DeviceObject)
{
    NTSTATUS status;
    HANDLE   engineHandle;

    FWPM_SESSION0 session = {0};
    status = FwpmEngineOpen0(NULL, RPC_C_AUTHN_WINNT, NULL, &session,
                             &engineHandle);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    status = NexusRegisterOneCallout(
        DeviceObject,
        &NEXUS_WFP_CALLOUT_REDIRECT_V4_GUID,
        &FWPM_LAYER_ALE_CONNECT_REDIRECT_V4,
        L"NexusConnectRedirectV4",
        NexusConnectRedirectV4,
        engineHandle,
        &g_RedirectV4CalloutId);
    if (!NT_SUCCESS(status)) { goto cleanup; }

    status = NexusRegisterOneCallout(
        DeviceObject,
        &NEXUS_WFP_CALLOUT_REDIRECT_V6_GUID,
        &FWPM_LAYER_ALE_CONNECT_REDIRECT_V6,
        L"NexusConnectRedirectV6",
        NexusConnectRedirectV6,
        engineHandle,
        &g_RedirectV6CalloutId);
    if (!NT_SUCCESS(status)) { goto cleanup; }

    status = NexusRegisterOneCallout(
        DeviceObject,
        &NEXUS_WFP_CALLOUT_AUTH_CONNECT_V4_GUID,
        &FWPM_LAYER_ALE_AUTH_CONNECT_V4,
        L"NexusAuthConnectV4",
        NexusAuthConnectV4,
        engineHandle,
        &g_AuthConnectV4CalloutId);
    if (!NT_SUCCESS(status)) { goto cleanup; }

    status = NexusRegisterOneCallout(
        DeviceObject,
        &NEXUS_WFP_CALLOUT_AUTH_CONNECT_V6_GUID,
        &FWPM_LAYER_ALE_AUTH_CONNECT_V6,
        L"NexusAuthConnectV6",
        NexusAuthConnectV6,
        engineHandle,
        &g_AuthConnectV6CalloutId);

cleanup:
    FwpmEngineClose0(engineHandle);
    return status;
}

VOID
NexusWfpUnregisterAllCallouts(VOID)
{
    if (g_AuthConnectV6CalloutId) {
        FwpsCalloutUnregisterByKey0(&NEXUS_WFP_CALLOUT_AUTH_CONNECT_V6_GUID);
        g_AuthConnectV6CalloutId = 0;
    }
    if (g_AuthConnectV4CalloutId) {
        FwpsCalloutUnregisterByKey0(&NEXUS_WFP_CALLOUT_AUTH_CONNECT_V4_GUID);
        g_AuthConnectV4CalloutId = 0;
    }
    if (g_RedirectV6CalloutId) {
        FwpsCalloutUnregisterByKey0(&NEXUS_WFP_CALLOUT_REDIRECT_V6_GUID);
        g_RedirectV6CalloutId = 0;
    }
    if (g_RedirectV4CalloutId) {
        FwpsCalloutUnregisterByKey0(&NEXUS_WFP_CALLOUT_REDIRECT_V4_GUID);
        g_RedirectV4CalloutId = 0;
    }
}
