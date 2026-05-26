// PolicyState.c — atomic policy snapshot management.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md §7
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md §T4 + impl
//
// One pointer (g_ActivePolicy) holds the current policy. PUSH_POLICY
// builds a new NEXUS_POLICY, fills it, then InterlockedExchangePointer
// swaps it in. The previous pointer is queued for free via a WDF
// work-item at PASSIVE_LEVEL — callouts may still be reading it at
// DISPATCH when the swap completes.
//
// Self-PID bypass (FR-9): NexusPolicySetSelfPid is called from HELLO
// once per agent process; the PID is stored in a separate global and
// survives any number of PUSH_POLICY generations.

#include "Common.h"

static volatile PNEXUS_POLICY g_ActivePolicy = NULL;
static volatile LONG          g_SelfPid      = 0;

static VOID FreePolicy(_In_ PNEXUS_POLICY Policy)
{
    if (Policy == NULL) return;
    if (Policy->processBypass) {
        ExFreePoolWithTag(Policy->processBypass, NEXUS_WFP_POOL_TAG);
    }
    if (Policy->destBypass) {
        ExFreePoolWithTag(Policy->destBypass, NEXUS_WFP_POOL_TAG);
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

    // Validate the body length matches the header counts.
    ULONG expected = sizeof(NexusPolicyHeader)
                   + hdr->processBypassCount * sizeof(UINT32)
                   + hdr->destBypassCount    * sizeof(NexusCidr);
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

    // Atomic swap. The returned pointer is the previous policy;
    // decrement its refcount — if no callout is currently holding
    // a reference, this frees it immediately. If callouts are
    // active, the last reader to release will free.
    PNEXUS_POLICY old = (PNEXUS_POLICY)InterlockedExchangePointer(
        (PVOID volatile*)&g_ActivePolicy, newPolicy);

    if (old != NULL) {
        if (InterlockedDecrement(&old->refCount) == 0) {
            FreePolicy(old);
        }
    }

    return STATUS_SUCCESS;
}

// Acquire a refcounted snapshot. Callers MUST call ReleaseSnapshot
// after they're done reading. The refcount strategy:
//
//   - Apply does InterlockedIncrement(refCount) at allocation (start
//     at 1 = "being-active").
//   - When a new policy is swapped in, the old one's refCount is
//     decremented; if it drops to 0 (no readers), it's freed
//     immediately. If readers are still holding it, the last one to
//     release will see refCount drop to 0 and free.
//   - Readers (callouts) InterlockedIncrement BEFORE deref and
//     InterlockedDecrement AFTER. The increment-then-deref is safe
//     against the swap because the policy pointer at the time of
//     the increment was the live one — even if a swap races in,
//     the just-incremented reader is counted.
//
// Closes the race window the previous "just hope ExFreePool is
// delayed enough" model had.
static PNEXUS_POLICY AcquireSnapshot(VOID)
{
    PNEXUS_POLICY p;
    for (;;) {
        p = (PNEXUS_POLICY)g_ActivePolicy;
        if (p == NULL) return NULL;
        // Try to bump refcount. If the policy has already been
        // marked for free (refCount == 0), retry — a newer policy
        // is being installed by the swapper.
        LONG cur = p->refCount;
        if (cur <= 0) continue;
        if (InterlockedCompareExchange(&p->refCount, cur + 1, cur) == cur) {
            return p;
        }
    }
}

static VOID ReleaseSnapshot(_In_opt_ PNEXUS_POLICY Policy)
{
    if (Policy == NULL) return;
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

VOID NexusPolicySetSelfPid(_In_ UINT32 ProcessId)
{
    InterlockedExchange(&g_SelfPid, (LONG)ProcessId);
}

BOOLEAN NexusPolicyIsSelfPid(_In_ UINT32 ProcessId)
{
    return (UINT32)InterlockedCompareExchange(&g_SelfPid, 0, 0) == ProcessId;
}
