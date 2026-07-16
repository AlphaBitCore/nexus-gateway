"""
Nexus hooks control for the benchmark — through the Control Plane, with PROOF
that the gateway runtime actually reloaded.

WHY THIS EXISTS: the June-16 hooks-OFF arm was produced by editing hook rows in
the DB directly. That is INVALID — hooks are a Category-B config: a change only
takes effect after CP → Hub /api/hub/config/update → shadow desired_ver bump →
WebSocket push → thingclient.OnConfigChanged → HookConfigCache.Reload →
resolver.Swap. A DB edit never propagates to the running gateway, so the arm
measured a governed gateway while claiming it was ungoverned.

This module toggles hooks the ONLY valid way and proves convergence at the
gateway runtime before a run starts:
  - the actual on/off toggle DELEGATES to the proven scripts/hooks_toggle.sh
    (OAuth-PKCE + PUT /api/admin/hooks/{id} + its own convergence poll); we do
    not re-implement CP auth here.
  - this module owns the STRUCTURED runtime read (for result metadata) and a
    reusable convergence poll, reading the authoritative gateway snapshot at
    GET /api/admin/nodes/{id}/runtime (snapshot.sources["config.hooks"].value[]
    + meta.desired_ver/reported_ver).

Secrets (admin key, OAuth password) are NEVER logged or returned.
"""
from __future__ import annotations

import os
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

import httpx

# Request-stage compliance hooks the OFF arm disables (mirrors hooks_toggle.sh).
COMPLIANCE_HOOKS = ("pii-scanner", "keyword-blocker")
_SCRIPT = Path(__file__).resolve().parent.parent / "scripts" / "hooks_toggle.sh"


@dataclass
class RuntimeState:
    """Structured, authoritative gateway hook state (for result metadata)."""
    source_ok: bool                       # could we read the runtime snapshot at all?
    hook_count: Optional[int]             # total hooks loaded (None if source unavailable)
    response_hook_count: Optional[int]    # response-stage hooks loaded
    enabled_names: list[str] = field(default_factory=list)  # names with enabled=true
    desired_ver: Optional[int] = None
    reported_ver: Optional[int] = None
    detail: str = ""

    @property
    def versions_converged(self) -> bool:
        return (self.desired_ver is not None
                and self.desired_ver == self.reported_ver)


def _admin_headers() -> Optional[dict]:
    """x-admin-key headers if NEXUS_ADMIN_API_KEY is set, else None.

    The runtime read needs CP `settings:read`. We use x-admin-key when a key is
    provisioned; otherwise the caller falls back to the bash exit-code proof and
    records null counts (honest — convergence proven, counts not independently
    read). The key VALUE is never logged.
    """
    key = os.getenv("NEXUS_ADMIN_API_KEY", "").strip()
    if not key:
        return None
    return {"x-admin-key": key}


def discover_node_id(cp_url: str, headers: dict, client: Optional[httpx.Client] = None) -> Optional[str]:
    """Find the AI-gateway node id via GET /api/admin/nodes (id contains the
    gateway port, e.g. '-3050'). NEXUS_GW_NODE_ID overrides."""
    override = os.getenv("NEXUS_GW_NODE_ID", "").strip()
    if override:
        return override
    c = client or httpx.Client(timeout=10.0)
    try:
        r = c.get(f"{cp_url}/api/admin/nodes", headers=headers)
        if r.status_code != 200:
            return None
        data = r.json()
        nodes = data.get("data", data) if isinstance(data, dict) else data
        for n in nodes if isinstance(nodes, list) else []:
            nid = str(n.get("id", ""))
            if "-3050" in nid or n.get("type") == "ai-gateway":
                return nid
    except Exception:
        return None
    finally:
        if client is None:
            c.close()
    return None


def read_runtime_state(cp_url: str, node_id: Optional[str] = None,
                       client: Optional[httpx.Client] = None) -> RuntimeState:
    """Read the gateway's ACTUAL loaded hook set (not DB rows) from
    GET /api/admin/nodes/{id}/runtime. Returns source_ok=False (counts None) when
    no admin auth is available or the read fails — never fabricates a count."""
    headers = _admin_headers()
    if headers is None:
        return RuntimeState(source_ok=False, hook_count=None, response_hook_count=None,
                            detail="no NEXUS_ADMIN_API_KEY — runtime not read; rely on hooks_toggle.sh exit code")
    c = client or httpx.Client(timeout=15.0)
    try:
        nid = node_id or discover_node_id(cp_url, headers, client=c)
        if not nid:
            return RuntimeState(source_ok=False, hook_count=None, response_hook_count=None,
                                detail="could not discover AI-gateway node id")
        r = c.get(f"{cp_url}/api/admin/nodes/{nid}/runtime", headers=headers)
        if r.status_code != 200:
            return RuntimeState(source_ok=False, hook_count=None, response_hook_count=None,
                                detail=f"runtime endpoint HTTP {r.status_code}")
        d = r.json()
        meta = d.get("meta", {}) or {}
        src = (d.get("snapshot", {}) or {}).get("sources", {}).get("config.hooks", {}) or {}
        if not src.get("ok", False):
            return RuntimeState(source_ok=False, hook_count=None, response_hook_count=None,
                                desired_ver=meta.get("desired_ver"), reported_ver=meta.get("reported_ver"),
                                detail="config.hooks source not ok in snapshot")
        hooks = src.get("value", []) or []
        enabled = [h.get("name", "?") for h in hooks if h.get("enabled")]
        resp = [h for h in hooks if h.get("stage") == "response" and h.get("enabled")]
        return RuntimeState(
            source_ok=True, hook_count=len(enabled), response_hook_count=len(resp),
            enabled_names=sorted(enabled),
            desired_ver=meta.get("desired_ver"), reported_ver=meta.get("reported_ver"),
            detail=f"{len(enabled)} enabled ({len(resp)} response-stage)")
    except Exception as e:
        return RuntimeState(source_ok=False, hook_count=None, response_hook_count=None,
                            detail=f"runtime read error: {type(e).__name__}")
    finally:
        if client is None:
            c.close()


def poll_until_converged(desired: str, cp_url: str, node_id: Optional[str] = None,
                         timeout_s: int = 60, interval_s: float = 2.0,
                         reader=read_runtime_state) -> tuple[bool, int, RuntimeState]:
    """Poll runtime until it matches `desired` ('off'|'on'). Converged (OFF) =
    versions equal AND zero response-stage hooks AND none of COMPLIANCE_HOOKS
    enabled. Returns (converged, elapsed_ms, last_state). If the runtime can't be
    read (no admin key), returns (False, ..., source_ok=False) so the caller
    falls back to the bash exit code rather than treating it as converged."""
    deadline = time.monotonic() + timeout_s
    start = time.monotonic()
    last = RuntimeState(source_ok=False, hook_count=None, response_hook_count=None)
    while True:
        last = reader(cp_url, node_id)
        if last.source_ok and last.versions_converged:
            if desired == "off":
                if last.response_hook_count == 0 and not (set(COMPLIANCE_HOOKS) & set(last.enabled_names)):
                    return True, int((time.monotonic() - start) * 1000), last
            else:  # on
                if last.hook_count and last.hook_count > 0:
                    return True, int((time.monotonic() - start) * 1000), last
        if time.monotonic() >= deadline:
            return False, int((time.monotonic() - start) * 1000), last
        time.sleep(interval_s)


def set_hooks(mode: str, timeout_s: int = 120) -> subprocess.CompletedProcess:
    """Toggle hooks by delegating to the proven scripts/hooks_toggle.sh (which
    does OAuth + PUT + its own convergence poll and exits nonzero if it does not
    converge). Raises CalledProcessError on non-zero exit — the caller must not
    start load if this fails. Does NOT print secrets (the script sources
    .env.local itself; we pass no secret on argv)."""
    if mode not in ("on", "off"):
        raise ValueError(f"mode must be on|off, got {mode!r}")
    if not _SCRIPT.exists():
        raise FileNotFoundError(f"hooks_toggle.sh not found at {_SCRIPT}")
    return subprocess.run(
        ["bash", str(_SCRIPT), mode],
        check=True, timeout=timeout_s,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )


def _cp_url() -> str:
    return os.getenv("NEXUS_CP_URL", "http://localhost:3001").rstrip("/")


def main(argv: Optional[list[str]] = None) -> int:
    """CLI: status | off | on. Exit 0 = converged/ok, 1 = not converged/failed,
    2 = usage."""
    import sys
    argv = argv if argv is not None else sys.argv[1:]
    if len(argv) != 1 or argv[0] not in ("status", "off", "on"):
        print("usage: nexus_hooks_control.py status|off|on", file=sys.stderr)
        return 2
    cmd = argv[0]
    cp = _cp_url()

    if cmd == "status":
        st = read_runtime_state(cp)
        if not st.source_ok:
            print(f"runtime: UNREADABLE ({st.detail}) — set NEXUS_ADMIN_API_KEY for a structured read")
            return 1
        print(f"runtime: {st.hook_count} hooks enabled ({st.response_hook_count} response-stage); "
              f"names={st.enabled_names}; desired_ver={st.desired_ver} reported_ver={st.reported_ver} "
              f"converged={st.versions_converged}")
        return 0

    # off / on: delegate the toggle, then confirm/record convergence
    try:
        proc = set_hooks(cmd)
    except subprocess.CalledProcessError as e:
        print(f"hooks_toggle.sh {cmd} FAILED (rc={e.returncode}) — hooks NOT in desired state:\n{e.output}",
              file=sys.stderr)
        return 1
    except Exception as e:
        print(f"could not run hooks_toggle.sh: {e}", file=sys.stderr)
        return 1

    conv, ms, st = poll_until_converged(cmd, cp)
    if st.source_ok:
        status = "converged" if conv else "NOT converged"
        print(f"hooks {cmd}: {status} in {ms}ms — {st.detail}")
        return 0 if conv else 1
    # no independent read available — the bash already proved convergence (exit 0)
    print(f"hooks {cmd}: toggle succeeded (hooks_toggle.sh converged); "
          f"runtime not independently read ({st.detail})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
