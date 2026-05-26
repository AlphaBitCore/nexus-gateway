// Driver.c — NexusWFP DriverEntry, EvtDriverUnload, device-object setup.
//
// Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md
// SDD: docs/developers/specs/e59-s1-driver-skeleton.md

#include "Common.h"

WDFDEVICE g_NexusWfpDevice = NULL;

static WDFTIMER g_FlowSweepTimer = NULL;

static EVT_WDF_DRIVER_UNLOAD       NexusWfpEvtDriverUnload;
static EVT_WDF_IO_QUEUE_IO_DEVICE_CONTROL NexusWfpEvtIoDeviceControl;
static EVT_WDF_TIMER               NexusWfpEvtFlowSweepTimer;

// From Callouts.c — explicit redirect-handle lifecycle.
NTSTATUS NexusWfpCalloutsCreateRedirectHandle(VOID);
VOID     NexusWfpCalloutsDestroyRedirectHandle(VOID);

DRIVER_INITIALIZE DriverEntry;

#ifdef ALLOC_PRAGMA
#pragma alloc_text(INIT,   DriverEntry)
#pragma alloc_text(PAGE,   NexusWfpEvtDriverUnload)
#pragma alloc_text(PAGE,   NexusWfpEvtIoDeviceControl)
#endif

static VOID
NexusWfpEvtFlowSweepTimer(_In_ WDFTIMER Timer)
{
    UNREFERENCED_PARAMETER(Timer);
    NexusFlowTableSweep();
}

NTSTATUS DriverEntry(
    _In_ PDRIVER_OBJECT  DriverObject,
    _In_ PUNICODE_STRING RegistryPath)
{
    NTSTATUS              status;
    WDF_DRIVER_CONFIG     driverConfig;
    PWDFDEVICE_INIT       deviceInit  = NULL;
    WDFDEVICE             device      = NULL;
    UNICODE_STRING        deviceName;
    UNICODE_STRING        symlinkName;
    WDF_IO_QUEUE_CONFIG   queueConfig;
    WDFQUEUE              defaultQueue;
    WDFQUEUE              auditPumpQ;
    WDF_TIMER_CONFIG      timerConfig;
    WDF_OBJECT_ATTRIBUTES timerAttr;

    UNREFERENCED_PARAMETER(RegistryPath);

    WDF_DRIVER_CONFIG_INIT(&driverConfig, WDF_NO_EVENT_CALLBACK);
    driverConfig.DriverInitFlags = WdfDriverInitNonPnpDriver;
    driverConfig.EvtDriverUnload = NexusWfpEvtDriverUnload;

    status = WdfDriverCreate(
        DriverObject,
        RegistryPath,
        WDF_NO_OBJECT_ATTRIBUTES,
        &driverConfig,
        WDF_NO_HANDLE);
    if (!NT_SUCCESS(status)) return status;

    // DACL: LocalSystem + BUILTIN\Administrators only (NFR-5).
    DECLARE_CONST_UNICODE_STRING(deviceSddl, L"D:P(A;;GA;;;SY)(A;;GA;;;BA)");

    deviceInit = WdfControlDeviceInitAllocate(WdfGetDriver(), &deviceSddl);
    if (deviceInit == NULL) return STATUS_INSUFFICIENT_RESOURCES;

    RtlInitUnicodeString(&deviceName,  NEXUS_WFP_DEVICE_NAME);
    RtlInitUnicodeString(&symlinkName, NEXUS_WFP_SYMLINK_NAME);

    status = WdfDeviceInitAssignName(deviceInit, &deviceName);
    if (!NT_SUCCESS(status)) { WdfDeviceInitFree(deviceInit); return status; }
    WdfDeviceInitSetExclusive(deviceInit, FALSE);

    status = WdfDeviceCreate(&deviceInit, WDF_NO_OBJECT_ATTRIBUTES, &device);
    if (!NT_SUCCESS(status)) { WdfDeviceInitFree(deviceInit); return status; }
    g_NexusWfpDevice = device;

    status = WdfDeviceCreateSymbolicLink(device, &symlinkName);
    if (!NT_SUCCESS(status)) return status;

    WDF_IO_QUEUE_CONFIG_INIT_DEFAULT_QUEUE(&queueConfig, WdfIoQueueDispatchParallel);
    queueConfig.EvtIoDeviceControl = NexusWfpEvtIoDeviceControl;

    status = WdfIoQueueCreate(device, &queueConfig, WDF_NO_OBJECT_ATTRIBUTES, &defaultQueue);
    if (!NT_SUCCESS(status)) return status;

    WdfControlFinishInitializing(device);

    // Subsystems init.
    status = NexusPolicyInit();
    if (!NT_SUCCESS(status)) return status;

    status = NexusFlowTableInit();
    if (!NT_SUCCESS(status)) goto err_policy;

    status = NexusAuditQueueInit(device, &auditPumpQ);
    if (!NT_SUCCESS(status)) goto err_flow;

    status = NexusWfpFilterEngineOpen();
    if (!NT_SUCCESS(status)) goto err_audit;

    status = NexusWfpRegisterAllCallouts(WdfDeviceWdmGetDeviceObject(device));
    if (!NT_SUCCESS(status)) goto err_engine;

    status = NexusWfpCalloutsCreateRedirectHandle();
    if (!NT_SUCCESS(status)) goto err_callouts;

    status = NexusWfpFilterAddAll();
    if (!NT_SUCCESS(status)) goto err_redirect;

    // Periodic flow-table sweep: every 60s, evict stale flows.
    WDF_TIMER_CONFIG_INIT_PERIODIC(&timerConfig, NexusWfpEvtFlowSweepTimer, 60000);
    WDF_OBJECT_ATTRIBUTES_INIT(&timerAttr);
    timerAttr.ParentObject = device;
    status = WdfTimerCreate(&timerConfig, &timerAttr, &g_FlowSweepTimer);
    if (!NT_SUCCESS(status)) goto err_filter;
    WdfTimerStart(g_FlowSweepTimer, WDF_REL_TIMEOUT_IN_MS(60000));

    return STATUS_SUCCESS;

err_filter:
    NexusWfpFilterRemoveAll();
err_redirect:
    NexusWfpCalloutsDestroyRedirectHandle();
err_callouts:
    NexusWfpUnregisterAllCallouts();
err_engine:
    NexusWfpFilterEngineClose();
err_audit:
    NexusAuditQueueShutdown();
err_flow:
    NexusFlowTableShutdown();
err_policy:
    NexusPolicyShutdown();
    return status;
}

static VOID NexusWfpEvtDriverUnload(_In_ WDFDRIVER Driver)
{
    UNREFERENCED_PARAMETER(Driver);
    PAGED_CODE();

    if (g_FlowSweepTimer) {
        WdfTimerStop(g_FlowSweepTimer, TRUE);
        WdfObjectDelete(g_FlowSweepTimer);
        g_FlowSweepTimer = NULL;
    }

    NexusWfpFilterRemoveAll();
    NexusWfpCalloutsDestroyRedirectHandle();
    NexusWfpUnregisterAllCallouts();
    NexusWfpFilterEngineClose();
    NexusAuditQueueShutdown();
    NexusFlowTableShutdown();
    NexusPolicyShutdown();

    g_NexusWfpDevice = NULL;
}

static VOID NexusWfpEvtIoDeviceControl(
    _In_ WDFQUEUE   Queue,
    _In_ WDFREQUEST Request,
    _In_ size_t     OutputBufferLength,
    _In_ size_t     InputBufferLength,
    _In_ ULONG      IoControlCode)
{
    UNREFERENCED_PARAMETER(Queue);
    PAGED_CODE();

    (VOID)NexusWfpDispatchIoctl(
        Request,
        OutputBufferLength,
        InputBufferLength,
        IoControlCode);
}
