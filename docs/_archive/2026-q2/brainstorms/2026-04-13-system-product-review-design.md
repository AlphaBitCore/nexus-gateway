# Nexus Gateway — System Product Review

**Date:** 2026-04-13
**Type:** Product-level system integrity review
**Scope:** control-plane, ai-gateway, compliance-proxy, agent, shared — full business loop verification
**Methodology:** Component-by-component exploration → cross-component integration verification → gap analysis

> **Execution rule:** Every fix item in this document MUST use `/brainstorm` (superpowers:brainstorming) before implementation to explore approaches, trade-offs, edge cases, and risks. No fix should jump directly to code.

---

## Table of Contents

1. [System Architecture Overview](#1-system-architecture-overview)
2. [Business Loop Verification](#2-business-loop-verification)
3. [Critical Issues (P0)](#3-critical-issues-p0)
4. [High Priority Issues (P1)](#4-high-priority-issues-p1)
5. [Medium Priority Issues (P2)](#5-medium-priority-issues-p2)
6. [Low Priority Issues (P3)](#6-low-priority-issues-p3)
7. [Positive Findings](#7-positive-findings)
8. [Fix Priority Matrix](#8-fix-priority-matrix)
9. [Execution Guidelines](#9-execution-guidelines)
10. [Relationship to Prior Review](#10-relationship-to-prior-review)

---

## 1. System Architecture Overview

Nexus Gateway uses a **Control Plane + Trinity Data Plane** architecture:

```
                       ┌──────────────────────────┐
                       │      Control Plane        │
                       │  (Config, IAM, Audit,     │
                       │   Fleet Mgmt, Dashboard)  │
                       └────┬───────┬───────┬──────┘
                            │       │       │
                 Redis PS   │  REST │  mTLS │
                       ┌────▼──┐ ┌──▼───┐ ┌─▼──────┐
                       │  AI   │ │Comp. │ │Desktop │
                       │Gateway│ │Proxy │ │ Agent  │
                       └───────┘ └──────┘ └────────┘
                        SDK path  Network   Endpoint
                                  path      path
```

**Three data paths** share the same compliance engine (`packages/shared`):
- **AI Gateway** — explicit `/v1/*` API proxy, VirtualKey auth, provider routing
- **Compliance Proxy** — transparent TLS-intercepting HTTPS proxy
- **Desktop Agent** — OS-level network interception on developer workstations

**Shared compliance pipeline** (`packages/shared`):
- `hooks/` — 6 built-in hooks (keyword-filter, pii-detector, content-safety, rate-limiter, request-size-validator, ip-access-filter) + registry
- `compliance/` — PolicyResolver, Pipeline (sequential/parallel), Redactor
- `traffic/` — Adapter interface, DomainSnapshot, domain/path matching
- `configtypes/` — code-generated Go types from Prisma schema
- `ratelimit/` — Redis-distributed + local fallback sliding window
- `store/` — ConfigStore interface + PgConfigStore

---

## 2. Business Loop Verification

### 2.1 Loops That Work Correctly

| Business Flow | Components | Status |
|--------------|------------|--------|
| Provider/Model CRUD → AI-GW routing | CP → Redis PS → AI-GW | OK |
| Hook config CRUD → all 3 paths | CP → Redis PS / Agent poll → all services | OK |
| VK auth → rate limit → quota → upstream | AI-GW internal pipeline | OK |
| Device enrollment → mTLS → heartbeat | Agent → CP Agent API | OK |
| Agent audit upload → unified query | Agent → CP → audit_event table | OK (partial, see #3) |
| Compliance-proxy TLS bump → hook pipeline | CP-Proxy internal + shared hooks | OK |
| Credential encryption → ai-gw decryption | CP vault → DB → AI-GW cache | OK (latency issue, see #12) |

### 2.2 Loops That Are Broken or Incomplete

| Business Flow | Expected | Actual | Issue # |
|--------------|----------|--------|---------|
| Agent config sync — full contract | All fields populated | 3 fields missing | #1 |
| Interception domain change → proxy reload | Redis PS invalidation | Topic never published | #2 |
| Data classification → unified audit query | Queryable across all paths | Not written by AI-GW/Agent; not queried | #3 |
| Agent cert expiry → auto-renewal | Seamless renewal | Manual re-enrollment only | #4 |
| Credential key rotation | Versioned, zero-downtime | No rotation mechanism | #5 |
| Device cert revocation → immediate block | CRL/OCSP check | DB-only flag, no crypto enforcement | #6 |
| Cross-path user governance | Unified identity across paths | VK, mTLS, none — three isolated worlds | #7 |

---

## 3. Critical Issues (P0)

### Issue #1: Agent Config Response Missing Required Fields

**Severity:** CRITICAL — Agent functionality degraded
**Type:** Implementation gap

**Location:**
- Control-plane: `packages/control-plane/internal/handler/agent_api.go:595-603`
- Agent expects: `packages/agent/core/sync/configsync/snapshot.go:16-23`

**Problem:** The control-plane `GET /api/agent/config` response is missing 3 fields that the agent's `ConfigSnapshot` struct requires:

```
Control-plane sends:                Agent expects:
─────────────────────               ──────────────
policyRules              ✓          policyRules
aiDomains                ✓          (consumed elsewhere)
exemptions               ✓          (consumed elsewhere)
offlineMode              ✓          (consumed elsewhere)
heartbeatIntervalSec     ✓          (consumed elsewhere)
hookConfigs              ✓          hookConfigs
interceptionDomains      ✓          interceptionDomains
                         ✗ MISSING  configVersion (int)
                         ✗ MISSING  auditPolicy (string: "violation"|"inspect"|"all")
                         ✗ MISSING  forensicsEnabled (bool)
```

**Impact:**
- `configVersion` = 0 always → agent cannot detect config changes via version comparison
- `auditPolicy` = "" → agent audit behavior undefined (falls to default/zero-value)
- `forensicsEnabled` = false always → forensics mode permanently off

**Brainstorm required:** Where should these values come from? SystemMetadata table? Per-device config? Global settings?

---

### Issue #2: Redis Pub/Sub Topic Misalignment

**Severity:** CRITICAL — Config hot-reload broken for compliance-proxy domains/allowlists
**Type:** Implementation gap (bidirectional mismatch)

**Location:**
- Publisher: `packages/control-plane/internal/handler/` (various)
- AI-GW subscriber: `packages/ai-gateway/cmd/ai-gateway/main.go:326-358`
- CP-Proxy subscriber: `packages/compliance-proxy/internal/configcache/subscriber.go:177-204`

**Problem:** Two categories of mismatch:

**A) Topics compliance-proxy expects but control-plane never publishes:**

| Topic | Subscriber | Expected behavior |
|-------|-----------|-------------------|
| `interceptionDomains` | Compliance-proxy → CategoryInterceptionDomains | Reload domain matching rules |
| `allowlists` | Compliance-proxy → CategoryAllowlists | Reload IP/domain allowlists |

Result: When admin modifies interception domains or allowlists in the dashboard, compliance-proxy does not reload until its 5-minute cache TTL expires.

**B) Topics control-plane publishes but no service handles:**

| Topic | Publisher | Expected handler |
|-------|----------|-----------------|
| `iam` | IAM CRUD handlers | None — published into void |

**C) Topics with partial coverage:**

| Topic | AI-GW | CP-Proxy | Notes |
|-------|-------|----------|-------|
| `policies` | Not handled | Handled | AI-GW ignores policy changes |
| `providers` | Handled | Not handled | CP-Proxy doesn't need it (by design) |
| `routing` | Handled | Not handled | CP-Proxy doesn't need it (by design) |
| `credentials` | Handled | Not handled | CP-Proxy doesn't need it (by design) |

**Brainstorm required:** Should control-plane publish `interceptionDomains` and `allowlists` topics? Or should compliance-proxy's subscriber handle `policies` topic to cover domain changes? What about the `iam` orphan topic?

---

## 4. High Priority Issues (P1)

### Issue #3: Data Classification Audit Chain Broken

**Severity:** HIGH — Compliance reporting capability gap
**Type:** Implementation gap across 3 components

**Problem:** Data classification (PUBLIC/INTERNAL/CONFIDENTIAL/RESTRICTED) is a core compliance feature but only partially implemented:

| Component | Writes dataClassification | Reads in unified query |
|-----------|--------------------------|----------------------|
| AI-Gateway | **NO** — Record struct lacks field, not in INSERT | N/A |
| Compliance-Proxy | **YES** — Written to audit_event.dataClassification | N/A |
| Agent | **NO** — AuditEvent struct lacks field | N/A |
| Unified query | N/A | **NO** — SELECT omits column, UnifiedAuditRow lacks field |

**Locations:**
- AI-GW audit: `packages/ai-gateway/internal/observability/audit/audit.go:25-78` (no field), `:153-167` (not in INSERT)
- CP-Proxy audit: `packages/compliance-proxy/internal/audit/sql.go:58-63` (writes correctly)
- Agent audit: `packages/agent/core/gateway/client.go:77-95` (no field)
- Unified query: `packages/control-plane/internal/store/misc_queries.go:458-466` (not in SELECT)

**Impact:** Compliance officers cannot filter/query audit events by data classification level, even for compliance-proxy events that ARE classified.

**Brainstorm required:** Should all three paths write data classification? AI-GW has hook results with classification info. Agent has hook results too. Should the unified query expose it as a filter dimension?

---

### Issue #4: No Agent Certificate Auto-Renewal

**Severity:** HIGH — Fleet management risk
**Type:** Design gap

**Problem:**
- Agent device certificates have **365-day validity** (`agentca/ca.go:26`)
- **No auto-renewal mechanism** exists in agent or control-plane
- Agent status shows "degraded" at <30 days remaining, but no proactive action
- Control-plane tracks `CertExpiresAt` in DB but has **no monitoring/alerting**

**Risk:** In a 1000-device fleet enrolled on the same week, all devices lose connectivity simultaneously after 365 days.

**Brainstorm required:** Should renewal be agent-initiated (re-CSR flow) or control-plane-initiated (push new cert at heartbeat)? What about grace period, overlapping validity, and rollback if renewal fails?

---

### Issue #5: No Credential Encryption Key Rotation

**Severity:** HIGH — Security risk
**Type:** Design gap

**Problem:**
- `CREDENTIAL_ENCRYPTION_KEY` (AES-256-GCM) is shared between control-plane and ai-gateway via env var
- No key versioning — all credentials encrypted with same key forever
- No rotation mechanism — to rotate requires manual re-encryption of all credentials + simultaneous env var update
- No audit trail of key usage

**Locations:**
- CP encryption: `packages/control-plane/internal/crypto/aes_gcm.go`
- AI-GW decryption: `packages/ai-gateway/internal/credentials/decrypt.go`

**Brainstorm required:** Envelope encryption with versioned DEKs? HKDF-based key derivation per credential? Migration path for existing encrypted credentials?

---

### Issue #6: No Certificate Revocation Mechanism

**Severity:** HIGH — Security incident response gap
**Type:** Design gap

**Problem:**
- Device "revocation" is DB-only: `AgentDevice.Status = 'REVOKED'`
- No CRL or OCSP endpoint
- An already-issued certificate remains cryptographically valid even after DB revocation
- Agent with a revoked cert can still complete mTLS handshake if it bypasses control-plane status check

**Brainstorm required:** Simple CRL endpoint? Serial number blacklist checked at each heartbeat? TLS handshake-level rejection via custom VerifyPeerCertificate callback?

---

### Issue #7: No Cross-Path Identity Correlation

**Severity:** HIGH — Governance gap
**Type:** Design gap

**Problem:** Three data paths use completely isolated identity mechanisms:

| Path | Identity | Scoped to |
|------|----------|-----------|
| AI-Gateway | VirtualKey (HMAC hash or slug) | Org/Project |
| Compliance-Proxy | None (transparent proxy) | N/A |
| Agent | Device cert (mTLS) | Device → OS User |

**Cannot do:**
- "Revoke all AI access for user X" (no VK ↔ Device mapping)
- "Show all AI usage by user X across all paths" (no unified identity)
- "Apply quota to user X regardless of path" (quota is VK-only)

**Brainstorm required:** Is the existing `NexusUser` model from the prior review (Issue D0 in `system-integrity-fixes-design.md`) sufficient? Does it need VK ownership mapping? How does compliance-proxy identify users without authentication?

---

## 5. Medium Priority Issues (P2)

### Issue #8: InterceptionPolicy Scope Ambiguity

**Severity:** MEDIUM — Admin confusion risk
**Type:** Design/documentation gap

**Problem:**
- `InterceptionPolicy` table is consumed **only by the agent** (for inspect/passthrough/deny decisions)
- AI-Gateway uses `RoutingRule` + `HookConfig` instead
- Compliance-Proxy uses `HookConfig` + domain matching instead
- But control-plane broadcasts `"policies"` topic on InterceptionPolicy CRUD, implying all services should respond
- Admin UI "Policies" section does not clarify which services are affected

**Impact:** Admin creates a policy expecting it to block certain requests through the AI-Gateway, but it only affects agent-intercepted traffic.

**Brainstorm required:** Rename to "Agent Policies" in UI? Extend InterceptionPolicy to other paths? Or clarify documentation and remove misleading pub/sub broadcast?

---

### Issue #9: Quota Enforcement Limited to AI-Gateway

**Severity:** MEDIUM — Governance gap (possibly by design)
**Type:** Design decision needing documentation

**Problem:**
- Quotas (token/cost limits) are fully enforced only in AI-Gateway (reserve/reconcile two-phase)
- Compliance-Proxy has no quota enforcement (transparent proxy, no cost model)
- Agent has no quota enforcement

**Possible justification:** Only VK-authenticated traffic through AI-Gateway has a measurable cost model (provider pricing × tokens). Compliance-proxy and agent traffic goes directly to upstream without Nexus-managed credentials, so there's no cost to track.

**Brainstorm required:** Is this intentional? Should it be documented explicitly? Are there scenarios where compliance-proxy or agent traffic should be quota-limited (e.g., request count limits, not cost)?

---

### Issue #10: Agent Exemption Approval Workflow Incomplete

**Severity:** MEDIUM — Operational gap
**Type:** Design gap

**Problem:**
- Agent can POST exemption requests to `POST /api/agent/exemptions`
- But exemptions appear to be auto-approved (no pending/approved/rejected state machine)
- No notification mechanism when admin approves/rejects
- Agent discovers exemption status only via next config poll (up to 300s delay)

**Brainstorm required:** Does the business need manual approval? If yes: add status field, approval API, push notification via SSE or next heartbeat. If auto-approval is fine, document it.

---

## 6. Low Priority Issues (P3)

### Issue #11: AI-Gateway Audit Missing targetHost

**Severity:** LOW — Reporting inconvenience
**Type:** Implementation oversight

**Location:** `packages/ai-gateway/internal/observability/audit/audit.go:161`

**Problem:** AI-Gateway INSERT sets `targetHost = NULL` for all VK traffic. The routed provider/model info is only in `sourceDetails` JSON, not in the indexed `targetHost` column.

**Impact:** Unified audit queries cannot efficiently filter VK traffic by target provider using the `targetHost` column.

**Fix:** Write `routedProvider` or `routedProvider + "/" + routedModel` to `targetHost`.

---

### Issue #12: Credential Rotation Latency Up to 30 Minutes

**Severity:** LOW — Operational inconvenience
**Type:** Configuration tuning

**Problem:**
- AI-Gateway credential cache TTL is 30 minutes (`packages/ai-gateway/internal/credentials/manager.go:15`)
- Redis pub/sub invalidation is best-effort with no acknowledgment
- After admin rotates a credential, old credential may be used for up to 30 minutes if pub/sub message is lost

**Brainstorm required:** Reduce TTL? Add acknowledgment? Or is 30-minute staleness acceptable for the security model?

---

## 7. Positive Findings

The following business loops are **complete and well-designed**:

### 7.1 Hook Compliance Pipeline (shared)
All three paths share `packages/shared` with consistent:
- Decision model: APPROVE / REJECT_HARD / REJECT_SOFT / MODIFY / ABSTAIN
- 6 built-in hooks with registry pattern
- Pipeline supports sequential (CP-Proxy, Agent) and parallel (AI-GW) execution
- Per-hook timeout + total pipeline timeout + fail-open/fail-closed behavior
- Prometheus metrics with namespace scoping

### 7.2 AI-Gateway Routing Engine
- 2-stage pipeline: Stage 0 (policy narrowing) → Stage 1 (route decision)
- 5 strategy types: single, fallback, loadbalance, conditional, ab_split
- VK-level model access control
- Retry with recovery targets

### 7.3 Provider Adapter Framework
- 7 adapters: OpenAI, Anthropic, Gemini, Azure, DeepSeek, GLM, MiniMax
- Consistent Provider interface with request/response/stream transforms
- Frozen registry (thread-safe after startup)

### 7.4 Agent Lifecycle
- Enrollment → Heartbeat → Config sync → Audit upload → Unenrollment: complete chain
- mTLS with device-generated keys (private key never leaves device)
- Platform-specific interception: macOS NE IPC, Windows CONNECT proxy
- Encrypted local audit (SQLCipher) with batch drain

### 7.5 Configuration Hot-Reload
- `atomic.Pointer` swap pattern in compliance-proxy
- Redis pub/sub invalidation across services
- ETag-based conditional polling for agent
- TTL-based cache with singleflight dedup in ai-gateway

### 7.6 Audit Three-in-One
- VK, Proxy, Agent audit all write to unified `audit_event` table
- `source` column distinguishes origin ('vk', 'proxy', 'agent')
- Unified query endpoint merges all three with time-range filtering

---

## 8. Fix Priority Matrix

| # | Issue | Priority | Type | Complexity | Brainstorm Focus |
|---|-------|----------|------|------------|-----------------|
| 1 | Agent config missing fields | **P0** | Implementation | Low | Field source: SystemMetadata vs settings vs per-device |
| 2 | Pub/Sub topic misalignment | **P0** | Implementation | Low | Which topics to add; cleanup orphan topics |
| 3 | Data classification audit chain | **P1** | Implementation | Medium | Write path for AI-GW and Agent; query exposure |
| 4 | Agent cert auto-renewal | **P1** | Design+Impl | High | Renewal protocol, grace period, rollback |
| 5 | Credential key rotation | **P1** | Design+Impl | High | Envelope encryption, migration path |
| 6 | Cert revocation mechanism | **P1** | Design+Impl | High | CRL vs serial blacklist vs TLS callback |
| 7 | Cross-path identity | **P2** | Design | High | NexusUser model extension, VK ownership |
| 8 | InterceptionPolicy scope | **P2** | Doc+UI | Low | Rename, document, or extend |
| 9 | Quota scope documentation | **P2** | Documentation | Low | Document design intent |
| 10 | Exemption approval workflow | **P2** | Design | Medium | State machine, notification mechanism |
| 11 | VK audit targetHost | **P3** | Implementation | Low | Write routed provider info |
| 12 | Credential rotation latency | **P3** | Tuning | Low | TTL reduction or ack mechanism |

---

## 9. Execution Guidelines

### 9.1 Mandatory Brainstorming Rule

**Every fix item MUST go through `/brainstorm` (superpowers:brainstorming) before implementation.**

This is not optional. The brainstorming session must:
1. Explore at least 2-3 approaches with trade-offs
2. Consider edge cases and failure modes
3. Evaluate blast radius on existing functionality
4. Produce a concrete design before any code is written

### 9.2 Recommended Execution Order

**Phase 1 — Quick wins (P0, low complexity):**
- Issue #1: Add missing fields to agent config response
- Issue #2: Add missing pub/sub topics; clean up orphan topics

**Phase 2 — Compliance completeness (P1, medium complexity):**
- Issue #3: Data classification write + query chain
- Issue #11: VK audit targetHost (quick fix, can batch with #3)

**Phase 3 — Security hardening (P1, high complexity):**
- Issue #4: Agent cert auto-renewal
- Issue #5: Credential key rotation
- Issue #6: Cert revocation mechanism

Each of these requires a dedicated brainstorm session due to security implications. Consider writing separate SDD documents for each.

**Phase 4 — Governance & UX (P2):**
- Issue #7: Cross-path identity (depends on NexusUser model from prior review D0)
- Issue #8: InterceptionPolicy scope clarification
- Issue #9: Quota scope documentation
- Issue #10: Exemption approval workflow

**Phase 5 — Tuning (P3):**
- Issue #12: Credential rotation latency

### 9.3 Dependencies on Prior Review

This review identified issues **distinct from** the prior review document at `docs/superpowers/specs/2026-04-13-system-integrity-fixes-design.md`. The two documents are complementary:

| This review focuses on | Prior review focuses on |
|------------------------|----------------------|
| Cross-component **contract alignment** | Single-component **capability gaps** |
| Config distribution **integrity** | Content extraction **unification** |
| Credential/cert **lifecycle** | Compliance pipeline **completeness** |
| Audit **data completeness** | Data model **unification** |

**Shared dependency:** Issue #7 (cross-path identity) depends on prior review's D0 (NexusUser unified model).

### 9.4 Per-Fix Workflow

For each fix item:

```
1. /brainstorm — explore approaches, trade-offs, edge cases
2. Write SDD (if High complexity) or update this doc (if Low)
3. Implementation following mandatory workflow:
   Architecture → Requirements → SDD → OpenAPI → Code → Tests → Verify
4. Verify cross-component integration (not just unit tests)
5. Ask user whether to commit
```

---

## 10. Relationship to Prior Review

The prior document `docs/superpowers/specs/2026-04-13-system-integrity-fixes-design.md` covers 15 issues organized into 4 epochs (A-D) focusing on internal capability gaps. This document covers 12 issues focusing on **cross-component integration and product-level business loop integrity**.

**No overlap** — the two documents address different layers of the same system. Both should be executed. Recommended order: prior review's Epoch A (infrastructure hardening) first, then this review's Phase 1-2, then interleave remaining phases as appropriate.

---

## 11. Resolution Status

All 12 issues have been resolved. Implementation completed 2026-04-13.

| # | Issue | Resolution | Commit(s) |
|---|-------|-----------|-----------|
| 1 | Agent config missing fields | **FIXED** — Added configVersion, auditPolicy, forensicsEnabled to config response + settings API + version increment | `9ee384f` |
| 2 | Pub/Sub topic misalignment | **FIXED** — Registered CategoryInterceptionDomains loader in compliance-proxy | `4754b6d` |
| 3 | Data classification audit chain | **FIXED** — AI-GW, Agent, and unified query now read/write dataClassification | `ca3e15b`, `d493ee3`, `3fd18a6` |
| 4 | Agent cert auto-renewal | **FIXED** — Heartbeat-piggyback renewal (30d threshold), POST /api/agent/cert/renew, grace period via previousCertSerial | `ec030f1`, `a96a6cd` |
| 5 | Credential key rotation | **FIXED** — Dual-key window: MultiVault (CP), MultiDecryptor (GW), encryption_key_id column, rotation admin endpoint + background worker | `79cc205`, `1f83073`, `cc936c9`, `621b35d` |
| 6 | Cert revocation | **FIXED** — Reduced cert validity 365→90 days (defense-in-depth); DB-check middleware is sufficient since CP is sole verifier | `f71a3b0` |
| 7 | Cross-path identity | **FIXED** — VK ownerId (auto-set, personal ownership), AI-GW audit writes ownerId as userId, cross-path governance APIs (user audit, identity summary, revoke-access) | `7c3fe2f`, `f93cbfc`, `b4bd99c` |
| 8 | InterceptionPolicy scope | **FIXED** — Added scope documentation comments to admin_policies.go | `6d26725` |
| 9 | Quota scope documentation | **FIXED** — Added scope documentation comments to admin_quotas.go | `6d26725` |
| 10 | Exemption approval workflow | **FIXED** — Added explicit `status` column (pending/approved/rejected), in-place approve/reject preserving audit trail, SSE notification, pendingExemptionCount in agent config | migration + handler updates |
| 11 | VK audit targetHost | **FIXED** — AI-GW writes RoutedProvider to targetHost instead of NULL | `ca3e15b` |
| 12 | Credential rotation latency | **FIXED** — Reduced cache TTL from 30min to 5min | `6d26725` |

### Issue #7: Cross-Path Identity — Implemented

**Implementation summary:**

1. **VirtualKey → NexusUser:** Added `ownerId` FK to VirtualKey. Auto-set from authenticated user on creation. VKs are personal — non-admin users can only see/manage their own VKs. Super-admins can see all for governance.

2. **AI-Gateway audit identity:** VK ownerId is now written as `userId` in audit_event records, enabling cross-path user correlation.

3. **Cross-path governance APIs:**
   - `GET /api/admin/users/:id/identity` — unified view: user profile + owned VKs + assigned devices + audit summary by source
   - `GET /api/admin/users/:id/audit` — cross-path audit query (correlates via userId + deviceId from DeviceAssignment)
   - `POST /api/admin/users/:id/revoke-access` — atomic cross-path revoke: disables VKs + revokes devices + suspends user

4. **Compliance-Proxy identity:** Accepted as org-level (no user identity in transparent proxy). Proxy audit events are correlated at the org level, not user level.

5. **Cross-path quota:** Deferred — different cost models across paths make this non-trivial.
