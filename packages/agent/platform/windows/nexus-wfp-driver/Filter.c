// Filter.c — FwpmEngine session + sublayer + filter wiring.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §4
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md
//
// Persistent FwpmEngine session opened in DriverEntry and closed in
// EvtDriverUnload. The dynamic-session flag means our sub-layer,
// callouts, and filters auto-disappear when the session handle is
// closed.
//
// Filter wiring: one filter per callout, bound to the matching
// layer with action.type = FWP_ACTION_CALLOUT_TERMINATING so that
// our callout makes the decision for the layer.

#include "Common.h"

static HANDLE g_EngineHandle = NULL;

DEFINE_GUID(NEXUS_WFP_SUBLAYER_GUID,
    0x6F1E4D17, 0x7C19, 0x4D7B,
    0x9B, 0x4C, 0x1A, 0x5F, 0x2E, 0x2D, 0x8B, 0x10);

extern const GUID NEXUS_WFP_CALLOUT_REDIRECT_V4_GUID;
extern const GUID NEXUS_WFP_CALLOUT_REDIRECT_V6_GUID;
extern const GUID NEXUS_WFP_CALLOUT_QUIC_BLOCK_V4_GUID;
extern const GUID NEXUS_WFP_CALLOUT_QUIC_BLOCK_V6_GUID;
static UINT64 g_FilterIdRedirectV4   = 0;
static UINT64 g_FilterIdRedirectV6   = 0;
static UINT64 g_FilterIdQuicV4       = 0;
static UINT64 g_FilterIdQuicV6       = 0;

NTSTATUS NexusWfpFilterEngineOpen(VOID)
{
    NTSTATUS       status;
    FWPM_SESSION0  session     = {0};
    FWPM_SUBLAYER0 sublayer    = {0};

    session.flags = FWPM_SESSION_FLAG_DYNAMIC;

    status = FwpmEngineOpen0(NULL, RPC_C_AUTHN_WINNT, NULL, &session,
                             &g_EngineHandle);
    if (!NT_SUCCESS(status)) {
        g_EngineHandle = NULL;
        return status;
    }

    sublayer.subLayerKey   = NEXUS_WFP_SUBLAYER_GUID;
    sublayer.displayData.name        = L"NexusWFP Sublayer";
    sublayer.displayData.description = L"Nexus WFP Sublayer";
    sublayer.weight        = 0x8000;

    status = FwpmSubLayerAdd0(g_EngineHandle, &sublayer, NULL);
    if (!NT_SUCCESS(status)) {
        FwpmEngineClose0(g_EngineHandle);
        g_EngineHandle = NULL;
        return status;
    }

    return STATUS_SUCCESS;
}

VOID NexusWfpFilterEngineClose(VOID)
{
    if (g_EngineHandle != NULL) {
        FwpmEngineClose0(g_EngineHandle);
        g_EngineHandle = NULL;
    }
}

// The dynamic-session engine is the lifecycle anchor for EVERY FWPM
// object this driver creates (sublayer, filters, callout management
// objects): closing it auto-deletes them all from the BFE store, so a
// crashed or unloaded driver can never leave stale objects behind that
// would make the next FwpmCalloutAdd0 fail and block reload until
// reboot. Callouts.c borrows the handle through this accessor.
HANDLE NexusWfpFilterEngineHandle(VOID)
{
    return g_EngineHandle;
}

// AddOneFilter binds a callout to a layer in the Nexus sublayer. Pass
// Conditions=NULL / NumConditions=0 to match every flow at the layer (the
// redirect filters); pass an FWPM_FILTER_CONDITION0 array to restrict the
// callout to matching flows (the QUIC-block filters use UDP + remote port 443
// so they never fire on the hot TCP connect path).
static NTSTATUS AddOneFilter(
    _In_ const GUID*    LayerKey,
    _In_ const GUID*    CalloutKey,
    _In_ const wchar_t* DisplayName,
    _In_reads_opt_(NumConditions) const FWPM_FILTER_CONDITION0* Conditions,
    _In_ UINT32         NumConditions,
    _Out_ UINT64*       OutFilterId)
{
    if (g_EngineHandle == NULL) return STATUS_INVALID_HANDLE;

    FWPM_FILTER0 filter = {0};
    filter.layerKey                = *LayerKey;
    filter.subLayerKey             = NEXUS_WFP_SUBLAYER_GUID;
    filter.displayData.name        = (wchar_t*)DisplayName;
    filter.displayData.description = (wchar_t*)DisplayName;
    filter.action.type             = FWP_ACTION_CALLOUT_TERMINATING;
    filter.action.calloutKey       = *CalloutKey;
    filter.weight.type             = FWP_UINT8;
    filter.weight.uint8            = 0xF;
    filter.numFilterConditions     = NumConditions;
    filter.filterCondition         = (FWPM_FILTER_CONDITION0*)Conditions;

    return FwpmFilterAdd0(g_EngineHandle, &filter, NULL, OutFilterId);
}

NTSTATUS NexusWfpFilterAddAll(VOID)
{
    NTSTATUS status;

    status = AddOneFilter(&FWPM_LAYER_ALE_CONNECT_REDIRECT_V4,
                          &NEXUS_WFP_CALLOUT_REDIRECT_V4_GUID,
                          L"NexusWFP redirect filter v4",
                          NULL, 0, &g_FilterIdRedirectV4);
    if (!NT_SUCCESS(status)) return status;

    status = AddOneFilter(&FWPM_LAYER_ALE_CONNECT_REDIRECT_V6,
                          &NEXUS_WFP_CALLOUT_REDIRECT_V6_GUID,
                          L"NexusWFP redirect filter v6",
                          NULL, 0, &g_FilterIdRedirectV6);
    if (!NT_SUCCESS(status)) return status;

    // QUIC-block filters: UDP + remote port 443 only, so the callout fires
    // exclusively for QUIC/HTTP-3 handshakes (never the hot TCP path). (G-2)
    FWPM_FILTER_CONDITION0 quicConds[2] = {0};
    quicConds[0].fieldKey              = FWPM_CONDITION_IP_PROTOCOL;
    quicConds[0].matchType             = FWP_MATCH_EQUAL;
    quicConds[0].conditionValue.type   = FWP_UINT8;
    quicConds[0].conditionValue.uint8  = 17; // IPPROTO_UDP
    quicConds[1].fieldKey              = FWPM_CONDITION_IP_REMOTE_PORT;
    quicConds[1].matchType             = FWP_MATCH_EQUAL;
    quicConds[1].conditionValue.type   = FWP_UINT16;
    quicConds[1].conditionValue.uint16 = 443; // QUIC / HTTP-3

    status = AddOneFilter(&FWPM_LAYER_ALE_AUTH_CONNECT_V4,
                          &NEXUS_WFP_CALLOUT_QUIC_BLOCK_V4_GUID,
                          L"NexusWFP QUIC block filter v4",
                          quicConds, 2, &g_FilterIdQuicV4);
    if (!NT_SUCCESS(status)) return status;

    status = AddOneFilter(&FWPM_LAYER_ALE_AUTH_CONNECT_V6,
                          &NEXUS_WFP_CALLOUT_QUIC_BLOCK_V6_GUID,
                          L"NexusWFP QUIC block filter v6",
                          quicConds, 2, &g_FilterIdQuicV6);
    return status;
}

VOID NexusWfpFilterRemoveAll(VOID)
{
    if (g_EngineHandle == NULL) return;

    if (g_FilterIdQuicV6) {
        FwpmFilterDeleteById0(g_EngineHandle, g_FilterIdQuicV6);
        g_FilterIdQuicV6 = 0;
    }
    if (g_FilterIdQuicV4) {
        FwpmFilterDeleteById0(g_EngineHandle, g_FilterIdQuicV4);
        g_FilterIdQuicV4 = 0;
    }
    if (g_FilterIdRedirectV6) {
        FwpmFilterDeleteById0(g_EngineHandle, g_FilterIdRedirectV6);
        g_FilterIdRedirectV6 = 0;
    }
    if (g_FilterIdRedirectV4) {
        FwpmFilterDeleteById0(g_EngineHandle, g_FilterIdRedirectV4);
        g_FilterIdRedirectV4 = 0;
    }
}
