// FlowTable.c — kernel-side flow table keyed by (srcPort, isUDP).
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §5.1
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md §T4 (GET_ORIG_DST)
//
// Bucket-hashed table. The redirect callout inserts on every flow it
// rewrites; the user-mode proxy looks up via IOCTL_NEXUS_WFP_GET_ORIG_DST
// when accepting a redirected connection.

#include "Common.h"

#define NEXUS_FT_BUCKETS 256

typedef struct _NEXUS_FLOW_ENTRY {
    LIST_ENTRY list;
    UINT16     srcPort;
    BOOLEAN    isUDP;
    UINT8      family;
    UINT8      origDstAddr[16];
    UINT16     origDstPort;
    UINT32     processId;
    LARGE_INTEGER createdAt;     // KeQuerySystemTime
} NEXUS_FLOW_ENTRY, *PNEXUS_FLOW_ENTRY;

typedef struct _NEXUS_FT_BUCKET {
    LIST_ENTRY head;
    KSPIN_LOCK lock;
} NEXUS_FT_BUCKET;

static NEXUS_FT_BUCKET g_Buckets[NEXUS_FT_BUCKETS];
static BOOLEAN         g_Initialised = FALSE;

static __forceinline ULONG BucketIndex(UINT16 srcPort, BOOLEAN isUDP)
{
    // 16-bit Knuth multiplicative hash + UDP flag fold.
    ULONG h = (ULONG)srcPort * 2654435769u;
    if (isUDP) h ^= 0xA5A5A5A5u;
    return h % NEXUS_FT_BUCKETS;
}

NTSTATUS NexusFlowTableInit(VOID)
{
    for (ULONG i = 0; i < NEXUS_FT_BUCKETS; i++) {
        InitializeListHead(&g_Buckets[i].head);
        KeInitializeSpinLock(&g_Buckets[i].lock);
    }
    g_Initialised = TRUE;
    return STATUS_SUCCESS;
}

VOID NexusFlowTableShutdown(VOID)
{
    if (!g_Initialised) return;
    for (ULONG i = 0; i < NEXUS_FT_BUCKETS; i++) {
        KIRQL oldIrql;
        KeAcquireSpinLock(&g_Buckets[i].lock, &oldIrql);
        while (!IsListEmpty(&g_Buckets[i].head)) {
            PLIST_ENTRY le = RemoveHeadList(&g_Buckets[i].head);
            PNEXUS_FLOW_ENTRY e = CONTAINING_RECORD(le, NEXUS_FLOW_ENTRY, list);
            ExFreePoolWithTag(e, NEXUS_WFP_POOL_TAG);
        }
        KeReleaseSpinLock(&g_Buckets[i].lock, oldIrql);
    }
    g_Initialised = FALSE;
}

NTSTATUS NexusFlowTableInsert(
    _In_ UINT16 SrcPort,
    _In_ BOOLEAN IsUDP,
    _In_ UINT8 Family,
    _In_reads_bytes_(16) const UINT8* OrigDstAddr16,
    _In_ UINT16 OrigDstPort,
    _In_ UINT32 ProcessId)
{
    PNEXUS_FLOW_ENTRY e = (PNEXUS_FLOW_ENTRY)ExAllocatePool2(
        POOL_FLAG_NON_PAGED,
        sizeof(NEXUS_FLOW_ENTRY),
        NEXUS_WFP_POOL_TAG);
    if (e == NULL) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    RtlZeroMemory(e, sizeof(*e));
    e->srcPort     = SrcPort;
    e->isUDP       = IsUDP;
    e->family      = Family;
    e->origDstPort = OrigDstPort;
    e->processId   = ProcessId;
    RtlCopyMemory(e->origDstAddr, OrigDstAddr16, 16);
    KeQuerySystemTime(&e->createdAt);

    ULONG bIdx = BucketIndex(SrcPort, IsUDP);
    KIRQL oldIrql;
    KeAcquireSpinLock(&g_Buckets[bIdx].lock, &oldIrql);

    // Idempotent: if an entry with the same key exists, replace it.
    PLIST_ENTRY le = g_Buckets[bIdx].head.Flink;
    while (le != &g_Buckets[bIdx].head) {
        PNEXUS_FLOW_ENTRY ex = CONTAINING_RECORD(le, NEXUS_FLOW_ENTRY, list);
        PLIST_ENTRY next = le->Flink;
        if (ex->srcPort == SrcPort && ex->isUDP == IsUDP) {
            RemoveEntryList(le);
            ExFreePoolWithTag(ex, NEXUS_WFP_POOL_TAG);
        }
        le = next;
    }
    InsertHeadList(&g_Buckets[bIdx].head, &e->list);
    KeReleaseSpinLock(&g_Buckets[bIdx].lock, oldIrql);
    return STATUS_SUCCESS;
}

NTSTATUS NexusFlowTableLookup(
    _In_  UINT16 SrcPort,
    _In_  BOOLEAN IsUDP,
    _Out_ NexusGetOrigDstResponse* OutEntry)
{
    if (OutEntry == NULL) return STATUS_INVALID_PARAMETER;
    RtlZeroMemory(OutEntry, sizeof(*OutEntry));

    ULONG bIdx = BucketIndex(SrcPort, IsUDP);
    KIRQL oldIrql;
    KeAcquireSpinLock(&g_Buckets[bIdx].lock, &oldIrql);

    PLIST_ENTRY le = g_Buckets[bIdx].head.Flink;
    while (le != &g_Buckets[bIdx].head) {
        PNEXUS_FLOW_ENTRY e = CONTAINING_RECORD(le, NEXUS_FLOW_ENTRY, list);
        if (e->srcPort == SrcPort && e->isUDP == IsUDP) {
            OutEntry->family      = e->family;
            OutEntry->origDstPort = e->origDstPort;
            OutEntry->processId   = e->processId;
            RtlCopyMemory(OutEntry->origDstAddr, e->origDstAddr, 16);
            KeReleaseSpinLock(&g_Buckets[bIdx].lock, oldIrql);
            return STATUS_SUCCESS;
        }
        le = le->Flink;
    }
    KeReleaseSpinLock(&g_Buckets[bIdx].lock, oldIrql);
    return STATUS_NOT_FOUND;
}

VOID NexusFlowTableRemove(_In_ UINT16 SrcPort, _In_ BOOLEAN IsUDP)
{
    ULONG bIdx = BucketIndex(SrcPort, IsUDP);
    KIRQL oldIrql;
    KeAcquireSpinLock(&g_Buckets[bIdx].lock, &oldIrql);

    PLIST_ENTRY le = g_Buckets[bIdx].head.Flink;
    while (le != &g_Buckets[bIdx].head) {
        PNEXUS_FLOW_ENTRY e = CONTAINING_RECORD(le, NEXUS_FLOW_ENTRY, list);
        PLIST_ENTRY next = le->Flink;
        if (e->srcPort == SrcPort && e->isUDP == IsUDP) {
            RemoveEntryList(le);
            ExFreePoolWithTag(e, NEXUS_WFP_POOL_TAG);
        }
        le = next;
    }
    KeReleaseSpinLock(&g_Buckets[bIdx].lock, oldIrql);
}

VOID NexusFlowTableSweep(VOID)
{
    LARGE_INTEGER now;
    KeQuerySystemTime(&now);
    // 100-nanosecond intervals → seconds: divide by 10,000,000.
    LONGLONG cutoff = now.QuadPart - (LONGLONG)NEXUS_FLOW_TTL_SECONDS * 10000000LL;

    for (ULONG i = 0; i < NEXUS_FT_BUCKETS; i++) {
        KIRQL oldIrql;
        KeAcquireSpinLock(&g_Buckets[i].lock, &oldIrql);
        PLIST_ENTRY le = g_Buckets[i].head.Flink;
        while (le != &g_Buckets[i].head) {
            PNEXUS_FLOW_ENTRY e = CONTAINING_RECORD(le, NEXUS_FLOW_ENTRY, list);
            PLIST_ENTRY next = le->Flink;
            if (e->createdAt.QuadPart < cutoff) {
                RemoveEntryList(le);
                ExFreePoolWithTag(e, NEXUS_WFP_POOL_TAG);
            }
            le = next;
        }
        KeReleaseSpinLock(&g_Buckets[i].lock, oldIrql);
    }
}
