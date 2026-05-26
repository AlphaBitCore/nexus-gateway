# Settings & Navigation Restructure — Design Spec

**Date:** 2026-04-14  
**Status:** Approved  
**Scope:** Control Plane UI navigation restructure + Settings split into My Account (personal) and System Settings (system-wide)

---

## Problem Statement

The current `/settings` page mixes personal user settings (profile, API keys, audit log) with system-wide admin settings (SSO, SIEM, Agent, Observability). The page is restricted to `super_admin`, meaning no other role can edit their own profile or view their API keys.

Beyond Settings, the sidebar navigation has structural problems: the "System" section is a catch-all with 13 items, agent/fleet/device features are scattered, compliance features are split between System and Forward Proxy, and several complete pages are hidden from navigation.

## Design Decisions

### 1. My Account — Personal Settings

**Route:** `/account`  
**Access:** All authenticated users (no role restriction)  
**Entry point:** User avatar dropdown menu in top-right corner of the shell (not in sidebar)

**Tabs:**

| Tab | Content | API |
|---|---|---|
| Profile | displayName, email, change password | `GET /api/admin/users/:id` (self), `PATCH /api/admin/me` |
| API Keys | Admin API keys owned by current user | `GET /api/admin/api-keys?scope=owned` |
| Activity | Current user's admin audit log | `GET /api/admin/me/admin-audit-logs` |

**Backend fixes required:**
- `PATCH /api/admin/me` must support `displayName` and `currentPassword`/`newPassword` (currently only handles `email`)
- Profile data should come from the full user record (`GET /api/admin/users/:id` for the current user), not from whoami which only returns `keyId, keyName, role, email`

### 2. User Menu — Top-Right Corner

Replace the current minimal auth display with a proper user menu:

- Trigger: User avatar/initials + display name in the top-right of the shell header
- Dropdown items:
  - **My Account** → navigates to `/account`
  - **Logout** → calls logout API

### 3. System Settings — Admin Configuration

**Route:** `/settings` (unchanged)  
**Access:** `super_admin` only  
**Entry point:** Sidebar → System section

**Tabs:**

| Tab | Content | Source |
|---|---|---|
| General | trafficFlushIntervalMs, maintenanceMode, logLevel, hookTimeout, failBehavior | Current General tab minus personal info |
| Authentication | SSO/OIDC config + Device Auth mode (two sections in one tab) | Merge current SSO tab + DeviceAuthSettingsPage |
| Agent | Shutdown warning (multi-locale) + auditPolicy + forensicsEnabled | Current Agent tab + expose existing backend `agent.settings` |
| Observability | OTel config + Trace Viewer URL | Unchanged |
| SIEM | Webhook config + event types + test | Unchanged |

**Route removal:** `/settings/device-auth` is deleted; content moves to Authentication tab.

### 4. Sidebar Navigation Restructure

**Order principle:** High-frequency daily use → Runtime monitoring → Compliance reporting → Low-frequency configuration → System admin

#### Section 1: OVERVIEW
- collapsible: false
- Items: Dashboard, Traffic & Analytics

#### Section 2: GATEWAY
- collapsible: true, defaultOpen: true
- Items: Providers & Models, Routing Rules, Hooks & Policies, Quotas (promoted from hidden route)

#### Section 3: FORWARD PROXY
- collapsible: true, defaultOpen: true
- Items:
  - Status & Compliance (merge current Proxy Status + Compliance Dashboard)
  - Proxy Audit
  - Alerts & Exemptions (merge current Alerts + Exemptions)
  - Discovery

#### Section 4: FLEET
- collapsible: true, defaultOpen: false
- Items: Fleet Overview, Devices, Device Groups, Agent Events, Agent Exemptions

#### Section 5: COMPLIANCE & AUDIT
- collapsible: true, defaultOpen: false
- Items: Audit Logs, Data Classification, DSAR

#### Section 6: KEYS & ACCESS
- collapsible: true, defaultOpen: false
- Merges current "Keys & Credentials" + "Access Control" sections
- Items: Organizations, Projects, Virtual Keys, Credentials, Users (super_admin), Roles (super_admin), Policies (super_admin), Simulator (super_admin)

#### Section 7: SYSTEM
- collapsible: true, defaultOpen: false
- Items: System Settings (super_admin), Status & Health, Config History (super_admin), Setup Wizard (super_admin)

### 5. Items Removed from Navigation

| Item | Reason |
|---|---|
| Device Auth (standalone sidebar entry) | Merged into System Settings → Authentication tab |
| Unified Audit | Redundant with Audit Logs; if it's the next-gen version, it replaces Audit Logs |
| Agent Users / Fleet Users | Accessible from Fleet Overview or Device detail; not worth a top-level entry |

### 6. Pages Promoted to Navigation

| Item | Section | Reason |
|---|---|---|
| Quotas | Gateway | Full CRUD page exists but was invisible |

### 7. Merged Pages

| New Item | Merges | Implementation |
|---|---|---|
| Status & Compliance | Proxy Status + Compliance Dashboard | Single page with two tabs: "Status" (connections, kill switch, metrics) and "Compliance" (coverage, hook health, reject stats) |
| Alerts & Exemptions | Alert History + Exemptions | Single page with two tabs: "Alerts" (history, channels, thresholds) and "Exemptions" (temporary compliance-hook exemptions) |

### 8. Role Visibility Summary

| Section | Default visibility | Restricted items |
|---|---|---|
| Overview | All | Traffic & Analytics: super_admin, compliance_admin, viewer |
| Gateway | All | — |
| Forward Proxy | super_admin | — |
| Fleet | super_admin, compliance_admin | — (viewer sees Fleet Overview) |
| Compliance & Audit | super_admin, compliance_admin, viewer | — |
| Keys & Access | All (for Orgs/Projects/VKs/Creds) | Users, Roles, Policies, Simulator: super_admin |
| System | Mixed | Settings, Config History, Setup: super_admin; Status & Health: all |

## Migration Plan (High-Level)

### Phase 1: Backend Fixes
- Fix `PATCH /api/admin/me` to handle displayName + password change
- Add `GET /api/admin/me/profile` or allow users to read their own user record

### Phase 2: My Account Page + User Menu
- Create `/account` route with Profile, API Keys, Activity tabs
- Add user avatar dropdown in shell header
- Remove personal sections from Settings General tab

### Phase 3: System Settings Consolidation
- Merge Device Auth into Settings Authentication tab
- Expose agent.settings (auditPolicy, forensicsEnabled) in Agent tab
- Move System Tuning to be the sole content of General tab
- Delete `/settings/device-auth` route

### Phase 4: Navigation Restructure
- Reorganize `shellRouteConfig.tsx` sections and ordering
- Merge Proxy Status + Compliance into one page
- Merge Alerts + Exemptions into one page
- Promote Quotas to Gateway section
- Remove redundant nav entries (Unified Audit, Agent Users, Device Auth)
- Update all icons (replace default dots with proper icons for Fleet/Compliance items)

### Phase 5: Cleanup
- Remove orphaned components
- Update i18n keys for renamed/merged sections
- Verify role-based visibility for all routes

## Out of Scope

- Appearance/theme/language settings (no current need)
- Notification preferences (no notification system exists)
- New features — this is purely restructuring existing functionality
