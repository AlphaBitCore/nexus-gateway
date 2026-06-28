// PolicyState.c — atomic policy snapshot management.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §7
//
// One pointer (g_ActivePolicy) holds the current policy. PUSH_POLICY
// builds a new NEXUS_POLICY, fills it, then InterlockedExchangePointer
// swaps it in. Reclamation of the superseded snapshot is refcount +
// grace-period: the swapper drops the "being-active" reference only
// after a per-CPU DPC rendezvous proves no reader can still be inside
// the unprotected load→CAS window of AcquireSnapshot (see the comment
// block above NexusPolicyQuiesceDispatchReaders). Whoever drops the
// count to zero frees — NonPaged pool may be freed at DISPATCH, so the
// last reader releasing inside a callout is fine.
//
// Self-PID bypass: NexusPolicySetSelfPid is called from HELLO once per
// agent process; the PID is stored in a separate global and survives
// any number of PUSH_POLICY generations.

#include "Common.h"

static volatile PNEXUS_POLICY g_ActivePolicy = NULL;
static volatile LONG          g_SelfPid      = 0;

_IRQL_requires_(PASSIVE_LEVEL)
static BOOLEAN NexusPolicyQuiesceDispatchReaders(VOID);

static VOID FreePolicy(_In_ PNEXUS_POLICY Policy)
{
    if (Policy == NULL) return;
    if (Policy->processBypass) {
        ExFreePoolWithTag(Policy->processBypass, NEXUS_WFP_POOL_TAG);
    }
    if (Policy->destBypass) {
        ExFreePoolWithTag(Policy->destBypass, NEXUS_WFP_POOL_TAG);
    }
    if (Policy->quicFallback) {
        ExFreePoolWithTag(Policy->quicFallback, NEXUS_WFP_POOL_TAG);
    }
    ExFreePoolWithTag(Policy, NEXUS_WFP_POOL_TAG);
}

NTSTATUS NexusPolicyInit(VOID)
{
    // Refcount-based free model needs no per-driver init state.
    return STATUS_SUCCESS;
}

VOID NexusPolicyShutdown(VOID)
{
    // Filter engine has already been closed by EvtDriverUnload, so
    // no callouts are running. We just need to drop the final
    // reference on the active policy. If callouts somehow held
    // refs (they shouldn't at this point), the last one to
    // release will free.
    PNEXUS_POLICY p = (PNEXUS_POLICY)InterlockedExchangePointer(
        (PVOID volatile*)&g_ActivePolicy, NULL);
    if (p != NULL && InterlockedDecrement(&p->refCount) == 0) {
        FreePolicy(p);
    }
}

// Body layout (from architecture §7):
//   NexusPolicyHeader header
//   UINT32  processBypass[header.processBypassCount]
//   NexusCidr destBypass[header.destBypassCount]
NTSTATUS NexusPolicyApply(
    _In_reads_bytes_(BufferLength) const VOID* Buffer,
    _In_ ULONG BufferLength)
{
    if (Buffer == NULL || BufferLength < sizeof(NexusPolicyHeader)) {
        return STATUS_BUFFER_TOO_SMALL;
    }
    const NexusPolicyHeader* hdr = (const NexusPolicyHeader*)Buffer;

    if (hdr->version != NEXUS_WFP_PROTOCOL_VERSION) {
        return STATUS_REVISION_MISMATCH;
    }
    if (hdr->processBypassCount > NEXUS_MAX_PROCESS_BYPASS) {
        return STATUS_INVALID_PARAMETER;
    }
    if (hdr->destBypassCount > NEXUS_MAX_DEST_BYPASS) {
        return STATUS_INVALID_PARAMETER;
    }
    if (hdr->quicFallbackCount > NEXUS_MAX_QUIC_FALLBACK) {
        return STATUS_INVALID_PARAMETER;
    }

    // Validate the body length matches the header counts.
    ULONG expected = sizeof(NexusPolicyHeader)
                   + hdr->processBypassCount * sizeof(UINT32)
                   + hdr->destBypassCount    * sizeof(NexusCidr)
                   + hdr->quicFallbackCount  * sizeof(NexusQuicImage);
    if (BufferLength < expected) {
        return STATUS_BUFFER_TOO_SMALL;
    }

    PNEXUS_POLICY newPolicy = (PNEXUS_POLICY)ExAllocatePool2(
        POOL_FLAG_NON_PAGED,
        sizeof(NEXUS_POLICY),
        NEXUS_WFP_POOL_TAG);
    if (newPolicy == NULL) {
        return STATUS_INSUFFICIENT_RESOURCES;
    }
    RtlZeroMemory(newPolicy, sizeof(NEXUS_POLICY));

    // refcount starts at 1 for being-active. The swap-out below
    // decrements the old policy's count; readers acquire +1 / release
    // -1; the last one to release frees.
    newPolicy->refCount = 1;
    newPolicy->generation = hdr->generation;
    newPolicy->killSwitch = hdr->killSwitch != 0;
    newPolicy->processBypassCount = hdr->processBypassCount;
    newPolicy->destBypassCount    = hdr->destBypassCount;
    newPolicy->quicFallbackCount  = hdr->quicFallbackCount;

    if (hdr->processBypassCount > 0) {
        ULONG byteCount = hdr->processBypassCount * sizeof(UINT32);
        newPolicy->processBypass = (PULONG)ExAllocatePool2(
            POOL_FLAG_NON_PAGED, byteCount, NEXUS_WFP_POOL_TAG);
        if (newPolicy->processBypass == NULL) {
            FreePolicy(newPolicy);
            return STATUS_INSUFFICIENT_RESOURCES;
        }
        const UINT32* pBase = (const UINT32*)((const UCHAR*)Buffer + sizeof(NexusPolicyHeader));
        RtlCopyMemory(newPolicy->processBypass, pBase, byteCount);
    }

    if (hdr->destBypassCount > 0) {
        ULONG byteCount = hdr->destBypassCount * sizeof(NexusCidr);
        newPolicy->destBypass = (NexusCidr*)ExAllocatePool2(
            POOL_FLAG_NON_PAGED, byteCount, NEXUS_WFP_POOL_TAG);
        if (newPolicy->destBypass == NULL) {
            FreePolicy(newPolicy);
            return STATUS_INSUFFICIENT_RESOURCES;
        }
        const NexusCidr* cBase = (const NexusCidr*)(
            (const UCHAR*)Buffer + sizeof(NexusPolicyHeader)
            + hdr->processBypassCount * sizeof(UINT32));
        RtlCopyMemory(newPolicy->destBypass, cBase, byteCount);
    }

    if (hdr->quicFallbackCount > 0) {
        ULONG byteCount = hdr->quicFallbackCount * sizeof(NexusQuicImage);
        newPolicy->quicFallback = (NexusQuicImage*)ExAllocatePool2(
            POOL_FLAG_NON_PAGED, byteCount, NEXUS_WFP_POOL_TAG);
        if (newPolicy->quicFallback == NULL) {
            FreePolicy(newPolicy);
            return STATUS_INSUFFICIENT_RESOURCES;
        }
        const NexusQuicImage* qBase = (const NexusQuicImage*)(
            (const UCHAR*)Buffer + sizeof(NexusPolicyHeader)
            + hdr->processBypassCount * sizeof(UINT32)
            + hdr->destBypassCount    * sizeof(NexusCidr));
        RtlCopyMemory(newPolicy->quicFallback, qBase, byteCount);
    }

    // Atomic swap. The returned pointer is the previous policy. Its
    // "being-active" reference may only be dropped after the grace
    // period: a reader that loaded the old pointer but has not yet
    // CAS'd its refcount is invisible to the count, and decrementing
    // to zero here would free pool that reader is about to touch.
    // When the rendezvous cannot run (DPC array allocation failed),
    // the superseded snapshot is deliberately kept alive — leaking a
    // few hundred bytes under memory pressure is bounded and strictly
    // safer than freed-pool access on the connect path.
    PNEXUS_POLICY old = (PNEXUS_POLICY)InterlockedExchangePointer(
        (PVOID volatile*)&g_ActivePolicy, newPolicy);

    if (old != NULL) {
        if (NexusPolicyQuiesceDispatchReaders()
            && InterlockedDecrement(&old->refCount) == 0) {
            FreePolicy(old);
        }
    }

    return STATUS_SUCCESS;
}

// --- DISPATCH-reader grace period -----------------------------------
//
// AcquireSnapshot's two steps — load g_ActivePolicy, then CAS its
// refCount — cannot be made atomic together, and the count lives
// inside the very object it protects, so the count alone can never
// close the window between them: a swapper that frees as soon as its
// own reference drops could free the policy while a reader sits
// between the load and the CAS, and that reader would then touch
// freed NonPaged pool.
//
// Two pieces close the window for every caller IRQL:
//
//   1. AcquireSnapshot raises to DISPATCH_LEVEL across the load→CAS
//      window, making it non-preemptible on its CPU.
//   2. The swapper, after the pointer swap and BEFORE dropping the
//      "being-active" reference, queues one DPC on every active
//      processor and waits for all of them. A DPC queued to a CPU
//      runs only after that CPU leaves its current DISPATCH-level
//      execution — so when every DPC has run, every reader that
//      could have loaded the OLD pointer has either finished the CAS
//      (and is counted) or finished entirely. Readers arriving later
//      can only load the new pointer. The decrement that follows is
//      therefore safe: zero really means no reader.

typedef struct _NEXUS_QUIESCE {
    volatile LONG remaining;
    KEVENT        done;
} NEXUS_QUIESCE;

_Function_class_(KDEFERRED_ROUTINE)
static VOID NexusQuiesceDpc(
    _In_ PKDPC Dpc,
    _In_opt_ PVOID DeferredContext,
    _In_opt_ PVOID SystemArgument1,
    _In_opt_ PVOID SystemArgument2)
{
    NEXUS_QUIESCE* q = (NEXUS_QUIESCE*)DeferredContext;
    UNREFERENCED_PARAMETER(Dpc);
    UNREFERENCED_PARAMETER(SystemArgument1);
    UNREFERENCED_PARAMETER(SystemArgument2);
    if (q != NULL && InterlockedDecrement(&q->remaining) == 0) {
        KeSetEvent(&q->done, IO_NO_INCREMENT, FALSE);
    }
}

// Runs the per-CPU rendezvous. Returns FALSE when it could not run
// (allocation or processor-number lookup failed) — the caller must
// then keep the superseded policy alive instead of freeing it.
_IRQL_requires_(PASSIVE_LEVEL)
static BOOLEAN NexusPolicyQuiesceDispatchReaders(VOID)
{
    ULONG cpuCount = KeQueryActiveProcessorCountEx(ALL_PROCESSOR_GROUPS);
    if (cpuCount == 0) {
        return FALSE;
    }

    // Resolve every processor number BEFORE queueing anything: once a
    // DPC is queued it cannot be taken back, so a mid-loop failure
    // would leave an incomplete rendezvous that still signals done.
    PROCESSOR_NUMBER* procNums = (PROCESSOR_NUMBER*)ExAllocatePool2(
        POOL_FLAG_NON_PAGED, cpuCount * sizeof(PROCESSOR_NUMBER), NEXUS_WFP_POOL_TAG);
    if (procNums == NULL) {
        return FALSE;
    }
    PKDPC dpcs = (PKDPC)ExAllocatePool2(
        POOL_FLAG_NON_PAGED, cpuCount * sizeof(KDPC), NEXUS_WFP_POOL_TAG);
    if (dpcs == NULL) {
        ExFreePoolWithTag(procNums, NEXUS_WFP_POOL_TAG);
        return FALSE;
    }
    for (ULONG i = 0; i < cpuCount; i++) {
        if (!NT_SUCCESS(KeGetProcessorNumberFromIndex(i, &procNums[i]))) {
            ExFreePoolWithTag(dpcs, NEXUS_WFP_POOL_TAG);
            ExFreePoolWithTag(procNums, NEXUS_WFP_POOL_TAG);
            return FALSE;
        }
    }

    NEXUS_QUIESCE q;
    q.remaining = (LONG)cpuCount;
    KeInitializeEvent(&q.done, NotificationEvent, FALSE);

    for (ULONG i = 0; i < cpuCount; i++) {
        KeInitializeDpc(&dpcs[i], NexusQuiesceDpc, &q);
        KeSetTargetProcessorDpcEx(&dpcs[i], &procNums[i]);
        KeInsertQueueDpc(&dpcs[i], NULL, NULL);
    }
    KeWaitForSingleObject(&q.done, Executive, KernelMode, FALSE, NULL);

    ExFreePoolWithTag(dpcs, NEXUS_WFP_POOL_TAG);
    ExFreePoolWithTag(procNums, NEXUS_WFP_POOL_TAG);
    return TRUE;
}

// Acquire a refcounted snapshot. Callers MUST call ReleaseSnapshot
// after they're done reading.
//
// The raise to DISPATCH makes the load→CAS window non-preemptible so
// the swapper's rendezvous (above) is guaranteed to run after it —
// callouts may classify at PASSIVE/APC level, where a preemption
// inside the window would otherwise let the grace period complete
// around a still-suspended reader.
static PNEXUS_POLICY AcquireSnapshot(VOID)
{
    PNEXUS_POLICY result = NULL;
    KIRQL oldIrql;
    KeRaiseIrql(DISPATCH_LEVEL, &oldIrql);
    for (;;) {
        PNEXUS_POLICY p = (PNEXUS_POLICY)g_ActivePolicy;
        if (p == NULL) {
            break;
        }
        LONG cur = p->refCount;
        if (cur <= 0) {
            // Unreachable under the grace-period model: a policy's
            // count can only reach 0 after no reader can load its
            // pointer anymore. Fail open (no snapshot → default
            // verdicts) rather than spin on a count this reader has
            // no claim on.
            break;
        }
        if (InterlockedCompareExchange(&p->refCount, cur + 1, cur) == cur) {
            result = p;
            break;
        }
    }
    KeLowerIrql(oldIrql);
    return result;
}

static VOID ReleaseSnapshot(_In_opt_ PNEXUS_POLICY Policy)
{
    if (Policy == NULL) return;
    // The last release may run inside a DISPATCH-level callout;
    // freeing NonPaged pool is legal up to DISPATCH_LEVEL.
    if (InterlockedDecrement(&Policy->refCount) == 0) {
        FreePolicy(Policy);
    }
}

BOOLEAN NexusPolicyKillSwitchActive(VOID)
{
    PNEXUS_POLICY p = AcquireSnapshot();
    if (p == NULL) return FALSE;
    BOOLEAN ks = p->killSwitch;
    ReleaseSnapshot(p);
    return ks;
}

BOOLEAN NexusPolicyIsBypassedProcess(_In_ UINT32 ProcessId)
{
    PNEXUS_POLICY p = AcquireSnapshot();
    if (p == NULL) return FALSE;
    BOOLEAN hit = FALSE;
    for (ULONG i = 0; i < p->processBypassCount; i++) {
        if (p->processBypass[i] == ProcessId) { hit = TRUE; break; }
    }
    ReleaseSnapshot(p);
    return hit;
}

static BOOLEAN MatchCidr(_In_ const NexusCidr* c,
                         _In_ UINT8 family,
                         _In_reads_bytes_(16) const UINT8* addr16)
{
    if (c->family != family) return FALSE;
    UINT8 prefixLen = c->prefixLen;
    UINT8 maxBits = (family == 2 /*AF_INET*/) ? 32 : 128;
    if (prefixLen > maxBits) prefixLen = maxBits;

    UINT8 fullBytes = prefixLen / 8;
    UINT8 remBits   = prefixLen % 8;

    for (UINT8 i = 0; i < fullBytes; i++) {
        if (c->addr[i] != addr16[i]) return FALSE;
    }
    if (remBits != 0) {
        UINT8 mask = (UINT8)(0xFFu << (8 - remBits));
        if ((c->addr[fullBytes] & mask) != (addr16[fullBytes] & mask)) {
            return FALSE;
        }
    }
    return TRUE;
}

BOOLEAN NexusPolicyIsBypassedDest(
    _In_ UINT8 Family,
    _In_reads_bytes_(16) const UINT8* Addr16)
{
    PNEXUS_POLICY p = AcquireSnapshot();
    if (p == NULL) return FALSE;
    BOOLEAN hit = FALSE;
    for (ULONG i = 0; i < p->destBypassCount; i++) {
        if (MatchCidr(&p->destBypass[i], Family, Addr16)) { hit = TRUE; break; }
    }
    ReleaseSnapshot(p);
    return hit;
}

BOOLEAN NexusPolicyIsQuicForceFallbackImage(
    _In_reads_(AppIdChars) PCWSTR AppIdPath,
    _In_ ULONG AppIdChars)
{
    if (AppIdPath == NULL || AppIdChars == 0) return FALSE;

    // WFP's ALE_APP_ID byte-blob includes a trailing NUL in its size; drop
    // any trailing NUL(s) so they don't pollute the basename compare.
    while (AppIdChars > 0 && AppIdPath[AppIdChars - 1] == L'\0') {
        AppIdChars--;
    }
    if (AppIdChars == 0) return FALSE;

    // Basename = everything past the last backslash (app-id is a device
    // path like \device\harddiskvolume3\...\chrome.exe).
    ULONG start = 0;
    for (ULONG i = 0; i < AppIdChars; i++) {
        if (AppIdPath[i] == L'\\') start = i + 1;
    }
    ULONG baseChars = AppIdChars - start;
    if (baseChars == 0 || baseChars > NEXUS_QUIC_IMAGE_MAX_CHARS) return FALSE;

    UNICODE_STRING appBase;
    appBase.Buffer        = (PWCH)(AppIdPath + start);
    appBase.Length        = (USHORT)(baseChars * sizeof(WCHAR));
    appBase.MaximumLength = appBase.Length;

    PNEXUS_POLICY p = AcquireSnapshot();
    if (p == NULL) return FALSE;
    BOOLEAN hit = FALSE;
    for (ULONG i = 0; i < p->quicFallbackCount; i++) {
        const NexusQuicImage* img = &p->quicFallback[i];
        if (img->len == 0 || img->len > NEXUS_QUIC_IMAGE_MAX_CHARS) continue;
        UNICODE_STRING cand;
        cand.Buffer        = (PWCH)img->name;
        cand.Length        = (USHORT)(img->len * sizeof(WCHAR));
        cand.MaximumLength = cand.Length;
        if (RtlCompareUnicodeString(&appBase, &cand, TRUE) == 0) {
            hit = TRUE;
            break;
        }
    }
    ReleaseSnapshot(p);
    return hit;
}

VOID NexusPolicySetSelfPid(_In_ UINT32 ProcessId)
{
    InterlockedExchange(&g_SelfPid, (LONG)ProcessId);
}

BOOLEAN NexusPolicyIsSelfPid(_In_ UINT32 ProcessId)
{
    return (UINT32)InterlockedCompareExchange(&g_SelfPid, 0, 0) == ProcessId;
}

// NexusPolicyGetSelfPid returns the raw agent PID recorded at HELLO (0 if not
// set yet). The connect-redirect callouts stamp it into the writable
// FWPS_CONNECT_REQUEST0.localRedirectTargetPID — mandatory for a localhost
// redirect, where IsSelfPid's boolean answer is insufficient.
UINT32 NexusPolicyGetSelfPid(VOID)
{
    return (UINT32)InterlockedCompareExchange(&g_SelfPid, 0, 0);
}
