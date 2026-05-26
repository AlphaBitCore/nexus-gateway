# D3 Session 1: Sysinfo Collection + Fleet Management Backend APIs

**Date:** 2026-04-13
**Status:** Approved
**Scope:** D3a (Agent Sysinfo Collection) + D3c (Fleet Management Backend APIs)
**Parent:** `docs/superpowers/specs/2026-04-13-d3-handoff.md`

---

## D3a: Agent Sysinfo Collection

### New Package: `packages/agent/core/sysinfo/`

Build-tagged files per OS, with a common interface.

**Files:**
- `sysinfo.go` — `Collect() (Info, error)` entry point; cross-platform fields via `os.Hostname()`, `os/user.Current()`, `net.Interfaces()`
- `sysinfo_darwin.go` — `sw_vers`, `ioreg` (serial, model), `sysctl` (CPU, memory)
- `sysinfo_windows.go` — `RtlGetVersion`, registry `MachineGuid`, WMI for hardware
- `sysinfo_linux.go` — `/etc/os-release`, `/etc/machine-id`, `/proc/cpuinfo`, `/proc/meminfo`
- `sysinfo_test.go` — unit tests (current OS only; parsing tests with fixtures)

### Info Struct

```go
type Info struct {
    Hostname       string   `json:"hostname"`
    MachineID      string   `json:"machineId"`
    OSName         string   `json:"osName"`
    OSVersion      string   `json:"osVersion"`
    OSBuild        string   `json:"osBuild"`
    Arch           string   `json:"arch"`
    CPUModel       string   `json:"cpuModel"`
    CPUCores       int      `json:"cpuCores"`
    TotalMemMB     int64    `json:"totalMemMB"`
    SerialNumber   string   `json:"serialNumber"`
    ModelName      string   `json:"modelName"`
    NetworkIFs     []NetIF  `json:"networkInterfaces"`
    OSUser         string   `json:"osUser"`
    OSDomain       string   `json:"osDomain"`
    CollectedAt    time.Time `json:"collectedAt"`
}

type NetIF struct {
    Name       string   `json:"name"`
    MACAddress string   `json:"macAddress"`
    IPs        []string `json:"ips"`
}
```

### Integration Points

- **Enrollment:** Collect full sysinfo, send as `deviceInfo` JSON field in enrollment request. Control plane stores in `AgentDevice.sysinfo` JSONB column.
- **Heartbeat:** Refresh variable fields (IPs, osUser, CPU/disk usage) in existing `metadata`. Optionally refresh full sysinfo if stale (>24h since last `collectedAt`).

---

## Schema Migration

Add `sysinfo JSONB` column to `AgentDevice` table (nullable, default null). Single migration in `tools/db-migrate/prisma/`.

No other schema changes required — all new D3c queries use existing tables.

---

## D3c: Fleet Management Backend APIs

### New Handler: `packages/control-plane/internal/handler/fleet.go`

Registered under existing admin auth middleware. Follows `admin_users.go` patterns: `parsePagination(c)`, audit via `h.Audit.Log()`, JSON `{data, total}` list responses.

### Agent Users Endpoints

| Method | Path | Description | Store |
|--------|------|-------------|-------|
| GET | `/api/admin/agent-users` | List agent users (canAccessControlPlane=false) | `ListNexusUsers` with filter |
| GET | `/api/admin/agent-users/:id` | User detail | `FindNexusUserByID` |
| GET | `/api/admin/agent-users/:id/devices` | User's devices via active DeviceAssignment | New: `ListDevicesByUserID` |
| GET | `/api/admin/agent-users/:id/audit` | User's audit events | New: `ListAuditEventsBySubjectID` |
| POST | `/api/admin/agent-users/:id/suspend` | Set status=disabled | `UpdateNexusUser` + audit |
| POST | `/api/admin/agent-users/:id/activate` | Set status=enabled | `UpdateNexusUser` + audit |

### Agent Devices Endpoints

| Method | Path | Description | Store |
|--------|------|-------------|-------|
| GET | `/api/admin/agent-devices/:id/audit` | Device audit events | New: `ListAuditEventsByDeviceID` |
| GET | `/api/admin/agent-devices/:id/config` | Effective config for device | Reuse config resolution |
| GET | `/api/admin/agent-devices/:id/timeline` | Assignment history (active + released) | New: `ListDeviceAssignments` |
| POST | `/api/admin/agent-devices/:id/reassign` | Manual reassignment | Release + create + audit |

### New Store Functions

File: `packages/control-plane/internal/store/fleet_queries.go`

- `ListDevicesByUserID(ctx, userID, pagination)` — JOIN DeviceAssignment (active) with AgentDevice; returns `([]AgentDevice, total, error)`
- `ListAuditEventsBySubjectID(ctx, subjectID, params)` — query `audit_event` WHERE `subject_id = $1`, time window filter, pagination
- `ListAuditEventsByDeviceID(ctx, deviceID, params)` — query `audit_event` WHERE `device_id = $1`, time window filter, pagination
- `ListDeviceAssignments(ctx, deviceID)` — all assignments for a device (active + released), ordered by `assignedAt DESC`
- `UpdateAgentDeviceSysinfo(ctx, deviceID, sysinfo json.RawMessage)` — `UPDATE agent_device SET sysinfo = $2 WHERE id = $1`

### Reassign Flow

1. Validate target user exists (NexusUser by ID)
2. Release current assignment via `ReleaseDeviceAssignment(ctx, deviceID)`
3. Create new assignment: `CreateDeviceAssignment(ctx, {deviceID, userID, source: "manual"})`
4. Audit log with before/after state (old user ID -> new user ID)

### Config Viewer

Reuses the config resolution logic from `GET /config` agent endpoint. Resolves org-level policies + device group rules into effective config. Returns read-only JSON — no mutations.

---

## Files Touch List

### New Files
- `packages/agent/core/sysinfo/sysinfo.go`
- `packages/agent/core/sysinfo/sysinfo_darwin.go`
- `packages/agent/core/sysinfo/sysinfo_windows.go`
- `packages/agent/core/sysinfo/sysinfo_linux.go`
- `packages/agent/core/sysinfo/sysinfo_test.go`
- `packages/control-plane/internal/handler/fleet.go`
- `packages/control-plane/internal/handler/fleet_test.go`
- `packages/control-plane/internal/store/fleet_queries.go`
- `tools/db-migrate/prisma/migrations/<timestamp>_add_device_sysinfo/migration.sql`

### Modified Files
- `packages/agent/core/security/enrollment/enroll.go` — add sysinfo to enrollment payload
- `packages/agent/core/heartbeat/sender.go` — refresh variable sysinfo in heartbeat
- `packages/control-plane/internal/handler/agent_api.go` — store sysinfo on enroll/heartbeat
- `packages/control-plane/internal/handler/routes.go` — register fleet routes
- `packages/control-plane/internal/store/agent_device.go` — add UpdateAgentDeviceSysinfo
- `tools/db-migrate/prisma/schema.prisma` — add sysinfo column

---

## Testing Strategy

- **Sysinfo collectors:** Unit tests per platform with build tags. Current-OS integration test via `Collect()`. Parsing tests with fixture data for cross-platform coverage.
- **Store functions:** Table-driven tests against real DB (existing repo pattern).
- **Handler endpoints:** Request/response shape tests, pagination, error cases. Follow existing handler test patterns in the repo.

---

## Out of Scope

- D3b (Enterprise Login / device auth modes) — separate session
- D3d (Fleet Dashboard UI) — separate session
- Time-series metrics / device telemetry history
