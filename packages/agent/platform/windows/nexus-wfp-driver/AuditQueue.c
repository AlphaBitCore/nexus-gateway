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
// from NexusAuditEmit producer side when a record was just enqueued.
static VOID TryDrainOneIrp(VOID)
{
    if (g_AuditPumpQueue == NULL) return;

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

    size_t bytes = written * sizeof(NexusFlowAuditEntry);
    WdfRequestCompleteWithInformation(irp, STATUS_SUCCESS, bytes);
}

VOID NexusAuditEmit(_In_ const NexusFlowAuditEntry* Entry)
{
    if (!g_Initialised || Entry == NULL) return;

    // Back-pressure: if the ring is at capacity, drop the oldest
    // (FIFO with overflow). NFR-4: increments a dropped counter via
    // ETW in the production build; here we silently overwrite.
    PNEXUS_AUDIT_NODE n = (PNEXUS_AUDIT_NODE)ExAllocatePool2(
        POOL_FLAG_NON_PAGED,
        sizeof(NEXUS_AUDIT_NODE),
        NEXUS_WFP_POOL_TAG);
    if (n == NULL) {
        return; // memory pressure; drop this record.
    }
    n->entry = *Entry;

    KIRQL oldIrql;
    KeAcquireSpinLock(&g_AuditRingLock, &oldIrql);

    if (g_AuditRingDepth >= NEXUS_AUDIT_RING_CAPACITY) {
        // Drop oldest.
        PLIST_ENTRY le = RemoveHeadList(&g_AuditRing);
        PNEXUS_AUDIT_NODE old = CONTAINING_RECORD(le, NEXUS_AUDIT_NODE, list);
        ExFreePoolWithTag(old, NEXUS_WFP_POOL_TAG);
        InterlockedDecrement(&g_AuditRingDepth);
    }

    InsertTailList(&g_AuditRing, &n->list);
    InterlockedIncrement(&g_AuditRingDepth);

    KeReleaseSpinLock(&g_AuditRingLock, oldIrql);

    // Wake one waiting consumer IRP if any.
    TryDrainOneIrp();
}
