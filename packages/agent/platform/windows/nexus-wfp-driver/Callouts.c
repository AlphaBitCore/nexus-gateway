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

// Sublayer GUID — instantiated HERE (Callouts.c is the single INITGUID
// translation unit) so it gets storage; Filter.c's DEFINE_GUID is the extern
// declaration only (Filter.c does not include <initguid.h>). Without this the
// link fails with LNK2001: unresolved external NEXUS_WFP_SUBLAYER_GUID.
DEFINE_GUID(NEXUS_WFP_SUBLAYER_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x10);

// GUID suffixes ...8B03 / ...8B04 are retired: they belonged to a pair
// of ALE_AUTH_CONNECT callouts that only ever permitted (the redirect
// layer makes every decision), yet were registered and bound to live
// terminating filters — kernel attack/maintenance surface plus a
// per-connect dispatch cost for a feature that does not exist. Add
// AUTH-layer callouts back only when admin deny rules actually ship,
// and mint FRESH GUIDs for them.

// QUIC-force-TCP-fallback callouts (G-2): ALE_AUTH_CONNECT block on UDP/443
// for admin-listed process images, so QUIC-capable apps fall back to TCP/443
// (which the redirect callouts MITM). FRESH GUIDs (…8B05 / …8B06) — per the
// note above, the retired …8B03/8B04 must never be reused.
DEFINE_GUID(NEXUS_WFP_CALLOUT_QUIC_BLOCK_V4_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x05);

DEFINE_GUID(NEXUS_WFP_CALLOUT_QUIC_BLOCK_V6_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x06);

static UINT32 g_RedirectV4CalloutId = 0;
static UINT32 g_RedirectV6CalloutId = 0;
static UINT32 g_QuicBlockV4CalloutId = 0;
static UINT32 g_QuicBlockV6CalloutId = 0;

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
    _In_     const FWPS_FILTER2*                filter,
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

    // Only TCP is redirected to the proxy (a TCP listener). UDP connects (DNS,
    // QUIC) must pass through untouched — redirecting UDP to the TCP proxy port
    // black-holes the datagrams and breaks name resolution. UDP interception is
    // future work (needs a UDP relay on the agent side).
    if (protocol != 6 /* IPPROTO_TCP */) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

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

    // Apply redirect: rewrite the remote address to 127.0.0.1:proxyPort.
    if (!(classifyOut->rights & FWPS_RIGHT_ACTION_WRITE)) {
        return; // can't modify; another filter already terminated.
    }
    if (g_RedirectHandle == NULL || g_TcpProxyPort == 0) {
        return; // not ready; fail-open.
    }

    // Loop guard (REQUIRED for a local-proxy redirect). After we redirect a
    // connection to 127.0.0.1:proxyPort it is re-injected and re-classified at
    // THIS layer. FwpsQueryConnectionRedirectState0 tells us we already
    // redirected it via our handle — permit it through untouched so it reaches
    // the proxy instead of being redirected a second time (which tears the
    // connection down, audited as a filterId=0 "Bad access").
    if (inMetaValues->redirectRecords != NULL) {
        FWPS_CONNECTION_REDIRECT_STATE rstate =
            FwpsQueryConnectionRedirectState0(inMetaValues->redirectRecords,
                                              g_RedirectHandle, NULL);
        if (rstate == FWPS_CONNECTION_REDIRECTED_BY_SELF ||
            rstate == FWPS_CONNECTION_PREVIOUSLY_REDIRECTED_BY_SELF) {
            classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
            return;
        }
    }

    UINT16 proxyPort = g_TcpProxyPort;

    // The agent PID is mandatory for a localhost redirect (stamped below as
    // localRedirectTargetPID). Fail open if HELLO hasn't run yet, rather than
    // redirect to a target the BFE can't resolve.
    UINT32 targetPid = NexusPolicyGetSelfPid();
    if (targetPid == 0) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    // The classify handle is acquired from classifyContext (it is NOT a field
    // of the metadata struct), and is what FwpsAcquireWritableLayerDataPointer0
    // / FwpsApplyModifiedLayerData0 act on. It must be released. Any failure
    // here is fail-open (permit unmodified).
    UINT64 classifyHandle = 0;
    NTSTATUS status = FwpsAcquireClassifyHandle0((VOID*)classifyContext, 0,
                                                 &classifyHandle);
    if (!NT_SUCCESS(status)) {
        return;
    }

    FWPS_CONNECT_REQUEST0* req = NULL;
    status = FwpsAcquireWritableLayerDataPointer0(
        classifyHandle,
        filter->filterId,
        0,
        (PVOID*)&req,
        classifyOut);
    if (!NT_SUCCESS(status) || req == NULL) {
        FwpsReleaseClassifyHandle0(classifyHandle);
        return;
    }

    SOCKADDR_IN* sin = (SOCKADDR_IN*)&req->remoteAddressAndPort;
    sin->sin_family      = AF_INET;
    sin->sin_addr.s_addr = RtlUlongByteSwap(0x7F000001UL); // 127.0.0.1 in network order
    sin->sin_port        = RtlUshortByteSwap(proxyPort);

    // MANDATORY for a localhost redirect (fwpsk.h FWPS_CONNECT_REQUEST0): the
    // callout supplies the PID of the process that will accept the redirected
    // connection (our agent, listening on 127.0.0.1:proxyPort) and associates
    // the redirect with our handle. Without these the BFE rejects the
    // redirected connection at ALE_AUTH_CONNECT (filterId=0, WSAEACCES).
    req->localRedirectTargetPID = targetPid;
    req->localRedirectHandle    = g_RedirectHandle;

    FwpsApplyModifiedLayerData0(classifyHandle, req, 0);
    FwpsReleaseClassifyHandle0(classifyHandle);

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
    _In_     const FWPS_FILTER2*                filter,
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

    // TCP only (see the V4 callout): UDP passes through untouched.
    if (protocol != 6 /* IPPROTO_TCP */) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

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

    // Loop guard — see the V4 callout: permit connections we already redirected
    // so the re-injected loopback connection reaches the proxy instead of being
    // redirected a second time and torn down.
    if (inMetaValues->redirectRecords != NULL) {
        FWPS_CONNECTION_REDIRECT_STATE rstate =
            FwpsQueryConnectionRedirectState0(inMetaValues->redirectRecords,
                                              g_RedirectHandle, NULL);
        if (rstate == FWPS_CONNECTION_REDIRECTED_BY_SELF ||
            rstate == FWPS_CONNECTION_PREVIOUSLY_REDIRECTED_BY_SELF) {
            classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
            return;
        }
    }

    UINT16 proxyPort = g_TcpProxyPort;

    // See the V4 callout: the agent PID is mandatory for a localhost redirect
    // (stamped below as localRedirectTargetPID). Fail open if HELLO hasn't run
    // yet, rather than redirect to a target the BFE can't resolve.
    UINT32 targetPid = NexusPolicyGetSelfPid();
    if (targetPid == 0) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    // WFP connect-redirect handshake (see the V4 callout for the rationale):
    // acquire the classify handle from classifyContext, modify, apply, release.
    UINT64 classifyHandle = 0;
    NTSTATUS status = FwpsAcquireClassifyHandle0((VOID*)classifyContext, 0,
                                                 &classifyHandle);
    if (!NT_SUCCESS(status)) {
        return;
    }

    FWPS_CONNECT_REQUEST0* req = NULL;
    status = FwpsAcquireWritableLayerDataPointer0(
        classifyHandle,
        filter->filterId,
        0,
        (PVOID*)&req,
        classifyOut);
    if (!NT_SUCCESS(status) || req == NULL) {
        FwpsReleaseClassifyHandle0(classifyHandle);
        return;
    }

    SOCKADDR_IN6* sin6 = (SOCKADDR_IN6*)&req->remoteAddressAndPort;
    RtlZeroMemory(sin6, sizeof(*sin6));
    sin6->sin6_family = AF_INET6;
    // ::1 loopback.
    sin6->sin6_addr.s6_addr[15] = 1;
    sin6->sin6_port = RtlUshortByteSwap(proxyPort);

    // See the V4 callout: the proxy's accepting PID is mandatory for a localhost
    // redirect, else the BFE rejects the connection with WSAEACCES.
    req->localRedirectTargetPID = targetPid;
    req->localRedirectHandle    = g_RedirectHandle;

    FwpsApplyModifiedLayerData0(classifyHandle, req, 0);
    FwpsReleaseClassifyHandle0(classifyHandle);

    (VOID)NexusFlowTableInsert(srcPort, /*isUDP=*/FALSE,
                               /*family=*/23, dstAddr16, dstPort, processId);

    EmitAuditRecord(processId, NexusDecisionRedirect,
                    /*family=*/23, protocol,
                    srcAddr16, srcPort, dstAddr16, dstPort);

    classifyOut->actionType = FWP_ACTION_PERMIT;
    classifyOut->rights    &= ~FWPS_RIGHT_ACTION_WRITE;
}

//
// NexusAuthConnectV4 — ALE_AUTH_CONNECT_V4 callout (G-2 QUIC fallback).
//
// Bound (via Filter.c) by a filter conditioned on UDP + remote port 443, so
// it only fires for QUIC handshakes. Blocks the connect for admin-listed
// process images so the app falls back to TCP/443 (which the redirect callout
// then MITMs). Everything else is permitted untouched.
//
static VOID
NTAPI
NexusAuthConnectV4(
    _In_     const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_     const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Inout_  VOID*                              layerData,
    _In_opt_ const VOID*                        classifyContext,
    _In_     const FWPS_FILTER2*                filter,
    _In_     UINT64                             flowContext,
    _Inout_  FWPS_CLASSIFY_OUT0*                classifyOut)
{
    UNREFERENCED_PARAMETER(layerData);
    UNREFERENCED_PARAMETER(classifyContext);
    UNREFERENCED_PARAMETER(filter);
    UNREFERENCED_PARAMETER(flowContext);

    classifyOut->actionType = FWP_ACTION_PERMIT;

    UINT8 protocol = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V4_IP_PROTOCOL].value.uint8;
    UINT16 remotePort = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V4_IP_REMOTE_PORT].value.uint16;

    // The filter conditions already restrict us to UDP/443; re-check
    // defensively so a future filter edit can't silently widen the block.
    if (protocol != 17 /* IPPROTO_UDP */ || remotePort != 443) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    UINT32 processId = (inMetaValues->processId > 0xFFFFFFFFULL)
                     ? 0u : (UINT32)inMetaValues->processId;

    // Never force-fallback the agent itself, an admin-bypassed process, or
    // anything while the kill switch is in full passthrough.
    if (NexusPolicyIsSelfPid(processId) ||
        NexusPolicyIsBypassedProcess(processId) ||
        NexusPolicyKillSwitchActive()) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    const FWP_BYTE_BLOB* appId = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V4_ALE_APP_ID].value.byteBlob;
    if (appId == NULL || appId->data == NULL || appId->size < sizeof(WCHAR) ||
        !NexusPolicyIsQuicForceFallbackImage((PCWSTR)appId->data,
                                             appId->size / sizeof(WCHAR))) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    UINT32 srcAddrHost = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V4_IP_LOCAL_ADDRESS].value.uint32;
    UINT32 dstAddrHost = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V4_IP_REMOTE_ADDRESS].value.uint32;
    UINT16 srcPort = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V4_IP_LOCAL_PORT].value.uint16;
    UINT8 srcAddr16[16] = {0}, dstAddr16[16] = {0};
    srcAddr16[0] = (UINT8)(srcAddrHost >> 24); srcAddr16[1] = (UINT8)(srcAddrHost >> 16);
    srcAddr16[2] = (UINT8)(srcAddrHost >> 8);  srcAddr16[3] = (UINT8)(srcAddrHost);
    dstAddr16[0] = (UINT8)(dstAddrHost >> 24); dstAddr16[1] = (UINT8)(dstAddrHost >> 16);
    dstAddr16[2] = (UINT8)(dstAddrHost >> 8);  dstAddr16[3] = (UINT8)(dstAddrHost);

    EmitAuditRecord(processId, NexusDecisionBlock, /*family=*/2, protocol,
                    srcAddr16, srcPort, dstAddr16, remotePort);

    // Hard veto so no lower-weight permit overrides the block.
    classifyOut->actionType = FWP_ACTION_BLOCK;
    classifyOut->rights    &= ~FWPS_RIGHT_ACTION_WRITE;
}

//
// NexusAuthConnectV6 — ALE_AUTH_CONNECT_V6 callout (G-2 QUIC fallback).
// IPv6 twin of NexusAuthConnectV4.
//
static VOID
NTAPI
NexusAuthConnectV6(
    _In_     const FWPS_INCOMING_VALUES0*       inFixedValues,
    _In_     const FWPS_INCOMING_METADATA_VALUES0* inMetaValues,
    _Inout_  VOID*                              layerData,
    _In_opt_ const VOID*                        classifyContext,
    _In_     const FWPS_FILTER2*                filter,
    _In_     UINT64                             flowContext,
    _Inout_  FWPS_CLASSIFY_OUT0*                classifyOut)
{
    UNREFERENCED_PARAMETER(layerData);
    UNREFERENCED_PARAMETER(classifyContext);
    UNREFERENCED_PARAMETER(filter);
    UNREFERENCED_PARAMETER(flowContext);

    classifyOut->actionType = FWP_ACTION_PERMIT;

    UINT8 protocol = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V6_IP_PROTOCOL].value.uint8;
    UINT16 remotePort = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V6_IP_REMOTE_PORT].value.uint16;

    if (protocol != 17 /* IPPROTO_UDP */ || remotePort != 443) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    UINT32 processId = (inMetaValues->processId > 0xFFFFFFFFULL)
                     ? 0u : (UINT32)inMetaValues->processId;

    if (NexusPolicyIsSelfPid(processId) ||
        NexusPolicyIsBypassedProcess(processId) ||
        NexusPolicyKillSwitchActive()) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    const FWP_BYTE_BLOB* appId = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V6_ALE_APP_ID].value.byteBlob;
    if (appId == NULL || appId->data == NULL || appId->size < sizeof(WCHAR) ||
        !NexusPolicyIsQuicForceFallbackImage((PCWSTR)appId->data,
                                             appId->size / sizeof(WCHAR))) {
        classifyOut->rights &= ~FWPS_RIGHT_ACTION_WRITE;
        return;
    }

    UINT8 srcAddr16[16] = {0}, dstAddr16[16] = {0};
    RtlCopyMemory(srcAddr16, inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V6_IP_LOCAL_ADDRESS].value.byteArray16, 16);
    RtlCopyMemory(dstAddr16, inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V6_IP_REMOTE_ADDRESS].value.byteArray16, 16);
    UINT16 srcPort = inFixedValues->incomingValue[
        FWPS_FIELD_ALE_AUTH_CONNECT_V6_IP_LOCAL_PORT].value.uint16;

    EmitAuditRecord(processId, NexusDecisionBlock, /*family=*/23, protocol,
                    srcAddr16, srcPort, dstAddr16, remotePort);

    classifyOut->actionType = FWP_ACTION_BLOCK;
    classifyOut->rights    &= ~FWPS_RIGHT_ACTION_WRITE;
}

static NTSTATUS
NTAPI
NexusCalloutNotify(
    _In_ FWPS_CALLOUT_NOTIFY_TYPE notifyType,
    _In_ const GUID*              filterKey,
    _Inout_ FWPS_FILTER2*         filter)
{
    UNREFERENCED_PARAMETER(notifyType);
    UNREFERENCED_PARAMETER(filterKey);
    UNREFERENCED_PARAMETER(filter);
    return STATUS_SUCCESS;
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
    // flowDeleteFn stays NULL: it only fires for callouts that
    // associate flow contexts, which the redirect callouts do not —
    // flow-table eviction is consume-on-lookup plus the TTL sweep.

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

    // The FWPM callout management objects go onto the same dynamic
    // session as the sublayer and filters (Filter.c), so all of them
    // auto-delete from the BFE store when that engine closes. A
    // non-dynamic session here would persist the callout objects past
    // unload, and the next load's FwpmCalloutAdd0 would fail with
    // FWP_E_ALREADY_EXISTS — driver unloadable-until-reboot.
    HANDLE engineHandle = NexusWfpFilterEngineHandle();
    if (engineHandle == NULL) {
        return STATUS_INVALID_HANDLE;
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

    // G-2: QUIC-force-TCP-fallback block callouts at ALE_AUTH_CONNECT.
    status = NexusRegisterOneCallout(
        DeviceObject,
        &NEXUS_WFP_CALLOUT_QUIC_BLOCK_V4_GUID,
        &FWPM_LAYER_ALE_AUTH_CONNECT_V4,
        L"NexusQuicBlockV4",
        NexusAuthConnectV4,
        engineHandle,
        &g_QuicBlockV4CalloutId);
    if (!NT_SUCCESS(status)) { goto cleanup; }

    status = NexusRegisterOneCallout(
        DeviceObject,
        &NEXUS_WFP_CALLOUT_QUIC_BLOCK_V6_GUID,
        &FWPM_LAYER_ALE_AUTH_CONNECT_V6,
        L"NexusQuicBlockV6",
        NexusAuthConnectV6,
        engineHandle,
        &g_QuicBlockV6CalloutId);

cleanup:
    // The engine handle belongs to Filter.c — partially-registered
    // callouts are unwound by the caller via UnregisterAllCallouts +
    // FilterEngineClose (the dynamic session deletes the FWPM halves).
    return status;
}

VOID
NexusWfpUnregisterAllCallouts(VOID)
{
    if (g_QuicBlockV6CalloutId) {
        FwpsCalloutUnregisterByKey0(&NEXUS_WFP_CALLOUT_QUIC_BLOCK_V6_GUID);
        g_QuicBlockV6CalloutId = 0;
    }
    if (g_QuicBlockV4CalloutId) {
        FwpsCalloutUnregisterByKey0(&NEXUS_WFP_CALLOUT_QUIC_BLOCK_V4_GUID);
        g_QuicBlockV4CalloutId = 0;
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
