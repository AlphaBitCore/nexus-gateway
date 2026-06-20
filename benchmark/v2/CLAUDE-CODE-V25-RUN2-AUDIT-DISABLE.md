# Claude Code — v2.5 Run 2: NEXUS_AUDIT_DISABLED=1 flag

**Goal:** Add a `NEXUS_AUDIT_DISABLED=1` env var to the AI gateway that skips the
`audit.Writer.Enqueue()` call on the request path. This is a benchmark-only diagnostic
flag to confirm the GC pause hypothesis: if p95 collapses from 183ms → ~20ms with
audit disabled, the audit writer's body-retention on heap is confirmed as the tail
latency driver.

**Context:** Tieben profiled the gateway under load. GC stop-the-world pauses of
200–900ms match the p95/p99 tail pattern exactly. The primary suspect is the audit
writer holding request + response bodies on the heap during async flush (ring buffer →
NATS → Hub → Postgres INSERT). Disabling Enqueue skips that heap retention entirely
and gives us a clean before/after signal.

**Branch:** create worktree at `./worktrees/v25-audit-flag` on branch
`feature/v25-audit-disabled` branched from `main`.  
**Do NOT commit without asking first.**  
**Do NOT touch `benchmark/v2/`.**

---

## What to build

### 1. Package-level flag in `proxy.go`

**File:** `packages/ai-gateway/internal/ingress/proxy/proxy.go`

Add alongside the existing `traceLatencyEnabled` var (lines ~50–67):

```go
// auditDisabled mirrors the NEXUS_AUDIT_DISABLED env var (default false).
// When set, Enqueue is skipped on the request path — audit records are
// silently dropped. BENCHMARK USE ONLY: this disables the audit trail.
// Never set in production. Gated on the same parse pattern as traceLatencyEnabled.
var auditDisabled = os.Getenv("NEXUS_AUDIT_DISABLED") == "1"
```

No function needed — single env var, single comparison. Keep it as simple as possible.

### 2. Gate the two Enqueue call sites

There are exactly two `AuditWriter.Enqueue(rec)` calls in the proxy package:

**Site A — main request defer (proxy.go line ~449):**
```go
// BEFORE:
h.deps.AuditWriter.Enqueue(rec)

// AFTER:
if !auditDisabled {
    h.deps.AuditWriter.Enqueue(rec)
}
```

**Site B — finalize() for cache hits (proxy.go line ~1272):**
```go
// BEFORE (inside func (h *Handler) finalize(...)):
h.deps.AuditWriter.Enqueue(rec)

// AFTER:
if !auditDisabled {
    h.deps.AuditWriter.Enqueue(rec)
}
```

That's the entire change — two one-line guards. Do not touch the Writer, the ring
buffer, the flushLoop, NATS, or anything in `packages/shared/`.

### 3. Startup log line

In the same file or in the gateway's `main.go` startup sequence, add a single `slog`
warn when the flag is active so it's visible in journalctl:

```go
if auditDisabled {
    slog.Warn("NEXUS_AUDIT_DISABLED=1: audit records will be dropped — benchmark mode only")
}
```

---

## Unit tests required

**File:** `packages/ai-gateway/internal/ingress/proxy/audit_disabled_test.go`
(or add to an existing test file in the package)

Two table-driven cases using the existing test harness (see `proxy_test.go` /
`test_helpers_test.go` for how to wire a Handler with a capture producer):

1. **Flag OFF (default):** make a request → `AuditWriter.Enqueue` is called once →
   audit record is present in the capture producer.

2. **Flag ON (`auditDisabled = true` for the test):** make a request →
   `AuditWriter.Enqueue` is NOT called → capture producer receives zero records.

Reset `auditDisabled` to its original value after each test using `t.Cleanup`.

---

## How to use on AWS (for Run 2)

```bash
# On the Nexus AMI — restart gateway with audit disabled
NEXUS_AUDIT_DISABLED=1 systemctl restart nexus-ai-gateway

# Confirm startup warn in journalctl
journalctl -u nexus-ai-gateway -n 5 | grep AUDIT_DISABLED

# Run S-02 hooks-OFF (same as the re-baseline run, just without audit)
# Compare p95 before (183ms) vs after (~20ms expected if GC hypothesis holds)

# Restore after run
systemctl restart nexus-ai-gateway  # starts without env var, audit re-enabled
```

---

## Self-audit before reporting done

**Round 1:**
- Q1: Both Enqueue sites gated? (`grep -n 'Enqueue' packages/ai-gateway/internal/ingress/proxy/proxy.go` — should show the two guarded sites)
- Q2: No TODO/FIXME/stub in production code?
- Q3: Unit tests cover both flag-ON and flag-OFF paths?
- Q4: No "fix later" deferrals? (This flag is permanent — benchmarks will always need it)

**Round 2:** Re-verify after fixing Round 1 issues.

---

## Do not do

- Do not disable the audit Writer itself, the flushLoop, or NATS publishing
- Do not touch `packages/shared/`
- Do not add this flag to any config yaml — env-only, benchmark-only
- Do not default it to true in any environment
- Do not commit — ask first
