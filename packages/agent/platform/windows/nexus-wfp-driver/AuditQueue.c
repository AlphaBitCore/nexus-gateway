// AuditQueue.c — kernel-side audit event queue + IRP completion.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §6
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md §T4 (AUDIT_PUMP)
//
// Two sides:
//   producer (NexusAuditEmit) — called from callouts at DISPATCH.
//   consumer (NexusAuditPumpComplete) — drains pended IRPs from
//     g_AuditPumpQueue, fills user-mode buffers, completes the IRP.
//
// Records sit in a non-paged ring while waiting for an IRP. When an
// IRP arrives, we drain as many records as fit in its 4 KB output
// buffer (1..63 records per IRP) and complete.

#include "Common.h"

#define NEXUS_AUDIT_RING_CAPACITY 4096  // ~ 256 KB of pending records

typedef struct _NEXUS_AUDIT_NODE {
    LIST_ENTRY          list;
    NexusFlowAuditEntry entry;
} NEXUS_AUDIT_NODE;

static LIST_ENTRY g_AuditRing;        // pending records, head = oldest
static volatile LONG g_AuditRingDepth = 0;
static KSPIN_LOCK g_AuditRingLock;

static WDFQUEUE   g_AuditPumpQueue = NULL;  // pended IRPs (set by Init)
static BOOLEAN    g_Initialised    = FALSE;

// Records dropped since the last drained batch (ring overflow or
// allocation failure). The next completed batch carries the count in
// its first record's droppedSinceLast field, so user mode always
// learns that the audit stream has a gap — a compliance pipeline that
// loses evidence silently cannot be trusted by anyone auditing it.
static volatile LONG g_AuditDropped = 0;

NTSTATUS NexusAuditQueueInit(_In_ WDFDEVICE Device,
                              _Out_ WDFQUEUE* OutAuditPumpQueue)
{
    NTSTATUS              status;
    WDF_IO_QUEUE_CONFIG   cfg;

    InitializeListHead(&g_AuditRing);
    KeInitializeSpinLock(&g_AuditRingLock);
    g_AuditRingDepth = 0;

    WDF_IO_QUEUE_CONFIG_INIT(&cfg, WdfIoQueueDispatchManual);
    status = WdfIoQueueCreate(Device, &cfg, WDF_NO_OBJECT_ATTRIBUTES, &g_AuditPumpQueue);
    if (!NT_SUCCESS(status)) {
        return status;
    }

    *OutAuditPumpQueue = g_AuditPumpQueue;
    g_Initialised = TRUE;
    return STATUS_SUCCESS;
}

VOID NexusAuditQueueShutdown(VOID)
{
    if (!g_Initialised) return;

    // Drain pending records.
    KIRQL oldIrql;
    KeAcquireSpinLock(&g_AuditRingLock, &oldIrql);
    while (!IsListEmpty(&g_AuditRing)) {
        PLIST_ENTRY le = RemoveHeadList(&g_AuditRing);
        PNEXUS_AUDIT_NODE n = CONTAINING_RECORD(le, NEXUS_AUDIT_NODE, list);
        ExFreePoolWithTag(n, NEXUS_WFP_POOL_TAG);
    }
    g_AuditRingDepth = 0;
    KeReleaseSpinLock(&g_AuditRingLock, oldIrql);

    // WDF will purge any IRPs still in g_AuditPumpQueue when the
    // device is torn down. The IRPs were marked pending; KMDF
    // cancels them with STATUS_CANCELLED.
    g_AuditPumpQueue = NULL;
    g_Initialised    = FALSE;
}

// Try to complete one waiting IRP with as many records as fit. Called
// from the producer side when a record was just enqueued, and from the
// IOCTL path when a fresh pump IRP arrives (so records buffered during
// an IRP gap are delivered immediately instead of waiting for the NEXT
// emit — on a quiet host that next emit can be minutes away).
static VOID TryDrainOneIrp(VOID)
{
    if (g_AuditPumpQueue == NULL) return;
    if (g_AuditRingDepth <= 0) {
        // Nothing buffered: leave any pended IRP in the queue. Pulling
        // it out to complete with zero records would turn the user-mode
        // pump into a busy loop.
        return;
    }

    WDFREQUEST irp = NULL;
    NTSTATUS status = WdfIoQueueRetrieveNextRequest(g_AuditPumpQueue, &irp);
    if (!NT_SUCCESS(status) || irp == NULL) {
        return; // no IRP waiting; record stays in the ring.
    }

    PVOID  outBuf = NULL;
    size_t outLen = 0;
    status = WdfRequestRetrieveOutputBuffer(irp,
                                            sizeof(NexusFlowAuditEntry),
                                            &outBuf, &outLen);
    if (!NT_SUCCESS(status) || outLen < sizeof(NexusFlowAuditEntry)) {
        WdfRequestComplete(irp, status);
        return;
    }

    // How many records fit?
    size_t maxRecords = outLen / sizeof(NexusFlowAuditEntry);
    size_t written = 0;
    NexusFlowAuditEntry* dst = (NexusFlowAuditEntry*)outBuf;

    KIRQL oldIrql;
    KeAcquireSpinLock(&g_AuditRingLock, &oldIrql);
    while (written < maxRecords && !IsListEmpty(&g_AuditRing)) {
        PLIST_ENTRY le = RemoveHeadList(&g_AuditRing);
        PNEXUS_AUDIT_NODE n = CONTAINING_RECORD(le, NEXUS_AUDIT_NODE, list);
        dst[written] = n->entry;
        written++;
        InterlockedDecrement(&g_AuditRingDepth);
        ExFreePoolWithTag(n, NEXUS_WFP_POOL_TAG);
    }
    KeReleaseSpinLock(&g_AuditRingLock, oldIrql);

    if (written > 0) {
        LONG drops = InterlockedExchange(&g_AuditDropped, 0);
        if (drops > 0) {
            dst[0].droppedSinceLast =
                (UINT16)((drops > 0xFFFF) ? 0xFFFF : drops);
        }
    }

    size_t bytes = written * sizeof(NexusFlowAuditEntry);
    WdfRequestCompleteWithInformation(irp, STATUS_SUCCESS, bytes);
}

// IOCTL-path entry point: called by HandleAuditPump right after a pump
// IRP is pended, so buffered records don't wait for new traffic.
VOID NexusAuditDrainPending(VOID)
{
    TryDrainOneIrp();
}

VOID NexusAuditEmit(_In_ const NexusFlowAuditEntry* Entry)
{
    if (!g_Initialised || Entry == NULL) return;

    // Back-pressure: if the ring is at capacity, drop the oldest
    // (FIFO with overflow). Every drop — overflow or allocation
    // failure — increments g_AuditDropped so the gap is visible to
    // user mode in the next batch's droppedSinceLast field.
    PNEXUS_AUDIT_NODE n = (PNEXUS_AUDIT_NODE)ExAllocatePool2(
        POOL_FLAG_NON_PAGED,
        sizeof(NEXUS_AUDIT_NODE),
        NEXUS_WFP_POOL_TAG);
    if (n == NULL) {
        InterlockedIncrement(&g_AuditDropped);
        return; // memory pressure; record dropped but counted.
    }
    n->entry = *Entry;

    KIRQL oldIrql;
    KeAcquireSpinLock(&g_AuditRingLock, &oldIrql);

    if (g_AuditRingDepth >= NEXUS_AUDIT_RING_CAPACITY) {
        // Drop oldest, counted.
        PLIST_ENTRY le = RemoveHeadList(&g_AuditRing);
        PNEXUS_AUDIT_NODE old = CONTAINING_RECORD(le, NEXUS_AUDIT_NODE, list);
        ExFreePoolWithTag(old, NEXUS_WFP_POOL_TAG);
        InterlockedDecrement(&g_AuditRingDepth);
        InterlockedIncrement(&g_AuditDropped);
    }

    InsertTailList(&g_AuditRing, &n->list);
    InterlockedIncrement(&g_AuditRingDepth);

    KeReleaseSpinLock(&g_AuditRingLock, oldIrql);

    // Wake one waiting consumer IRP if any.
    TryDrainOneIrp();
}
