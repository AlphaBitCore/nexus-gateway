"""benchmark/v3 — gateway-agnostic run validator.

Pure, importable validation logic for a single v3 run record (shape defined by
shared/schemas/run.schema.json). The CLI wrapper (scripts/validate_run.py) adds
schema-checking, quarantine, and exit codes on top of this.

Design invariants (from Tiebin's PR #57 review + the v3 mandate):
  * EVERY rule is gateway-agnostic — there is no `if gateway == "litellm"` path.
    The same bad input invalidates a run whichever gateway produced it. This is
    the fix for the asymmetric-gate finding.
  * Rules are pre-registered and exhaustive; a violated rule appends a reason and
    marks the run invalid. Invalid runs are quarantined WITH their reasons and raw
    artifacts, never silently dropped.
  * Failure outcomes are first-class: outcome accounting must be complete, so a
    gateway cannot look cleaner by timing requests out (the censored-percentile
    finding).

No third-party imports here — this module is stdlib-only so it can run anywhere.
"""
from __future__ import annotations

import re
from typing import Any, Iterable

VALIDATOR_VERSION = "v3.0.0"

# A run's image is "pinned" only when its digest is a real sha256 AND its version
# is not a moving tag. Both checks are needed: a digest with version "latest" is
# still a reproducibility hazard because the human-facing label lies.
_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
_DEFAULT_FLOATING_TAGS = {"latest", "main-latest", "main", "stable", "nightly", "edge", ""}
_DEFAULT_BURSTABLE_RE = r"^t[0-9]"
# Latency is only meaningful under open-loop offered load; saturation campaigns may
# use a labeled closed-loop ceiling run, so the open-loop rule is keyed on run_mode.
_DEFAULT_OPEN_LOOP_MODES = ("continuous_latency",)
_DEFAULT_MAX_TIMEOUT_RATE = 0.05

_OUTCOME_KEYS = ("success", "timeout", "http_4xx", "http_5xx", "broken_stream", "content_invalid")


def default_config() -> dict[str, Any]:
    """The pre-registered thresholds. Overridable via config/validation.yaml but
    the defaults are the contract."""
    return {
        "max_timeout_rate": _DEFAULT_MAX_TIMEOUT_RATE,
        "open_loop_required_modes": list(_DEFAULT_OPEN_LOOP_MODES),
        "burstable_instance_pattern": _DEFAULT_BURSTABLE_RE,
        "floating_tags": sorted(_DEFAULT_FLOATING_TAGS - {""}),
    }


def _cfg(config: dict[str, Any] | None, key: str) -> Any:
    d = default_config()
    if config:
        d.update({k: v for k, v in config.items() if v is not None})
    return d[key]


def validate_run(run: dict[str, Any], config: dict[str, Any] | None = None) -> list[str]:
    """Return a list of reasons the run is invalid. Empty list == valid.

    `run` is assumed to already satisfy run.schema.json (the CLI checks that
    first); this function still guards against missing keys so it is safe to call
    directly in tests.
    """
    reasons: list[str] = []
    r = run if isinstance(run, dict) else {}

    offered = r.get("offered_load", {}) or {}
    env = r.get("environment", {}) or {}
    gw = r.get("gateway", {}) or {}
    upstream = r.get("upstream", {}) or {}
    outcomes = r.get("outcomes", {}) or {}

    # 1. Open-loop required for any latency claim (keyed on run_mode, not gateway).
    open_loop_modes = set(_cfg(config, "open_loop_required_modes"))
    if r.get("run_mode") in open_loop_modes and offered.get("model") != "open_loop":
        reasons.append(
            f"offered_load.model must be 'open_loop' for run_mode "
            f"'{r.get('run_mode')}' (got '{offered.get('model')}')"
        )

    # 2. Upstream content must be verified (the silent mock-fallback finding).
    if upstream.get("verified") is not True:
        reasons.append(
            "upstream content check did not pass (upstream.verified is not true) — "
            "cannot prove the intended mock answered"
        )

    # 3. Every terminal outcome accounted for (the censored-percentile finding).
    total = outcomes.get("total")
    if isinstance(total, int):
        summed = sum(int(outcomes.get(k, 0) or 0) for k in _OUTCOME_KEYS)
        if summed != total:
            reasons.append(
                f"terminal outcomes do not sum to total ({summed} != {total}); "
                "failures must be accounted for, not dropped"
            )
        # 3b. Timeout rate ceiling.
        if total > 0:
            rate = int(outcomes.get("timeout", 0) or 0) / total
            max_rate = float(_cfg(config, "max_timeout_rate"))
            if rate > max_rate:
                reasons.append(
                    f"timeout rate {rate:.3f} exceeds max {max_rate:.3f} — run is unstable"
                )

    # 4. No burstable compute (the t-class finding).
    if env.get("launch_type") == "ec2":
        itype = str(env.get("instance_type") or "")
        if re.match(_cfg(config, "burstable_instance_pattern"), itype, re.IGNORECASE):
            reasons.append(
                f"burstable instance '{itype}' is banned — CPU credits throttle silently; "
                "use a fixed-performance class or Fargate dedicated vCPU"
            )

    # 5. Disk spec required when audit writes are on the write path (the no-IOPS finding).
    if env.get("audit_writes_enabled") and env.get("disk_type") not in (None, "none"):
        if not env.get("disk_iops") or not env.get("disk_throughput_mibps"):
            reasons.append(
                "audit writes enabled but disk IOPS/throughput unspecified — "
                "scanning cost and disk throttling cannot be separated"
            )

    # 6. Gateway pinned by digest, no floating tags (the floating-tag finding).
    digest = str(gw.get("image_digest") or "")
    if not _DIGEST_RE.match(digest):
        reasons.append(
            f"gateway image not pinned by a sha256 digest (got '{digest or 'none'}')"
        )
    floating = {t.lower() for t in _cfg(config, "floating_tags")}
    if str(gw.get("version") or "").lower() in floating:
        reasons.append(
            f"gateway version '{gw.get('version')}' is a floating tag — pin an exact version"
        )

    # 7-10. Mandatory provenance/artifacts/warmup.
    artifacts = r.get("artifacts", {}) or {}
    if not artifacts.get("versions_lock_uri"):
        reasons.append("missing versions.lock artifact — versions are not pinned/archived")
    if not artifacts.get("raw_results_uri"):
        reasons.append("missing raw results artifact — run is unauditable")
    if not artifacts.get("provenance_uri"):
        reasons.append("missing provenance artifact — hardware/version context absent")
    if not offered.get("warmup_seconds") or float(offered.get("warmup_seconds", 0)) <= 0:
        reasons.append("missing/zero warmup — cold-start latency contaminates the measurement")

    # 11. Services enumerated (the opaque-AMI / stray-service finding).
    services = env.get("services_running")
    if not isinstance(services, list) or len(services) == 0:
        reasons.append("environment.services_running is empty — running services must be enumerated")

    return reasons


def classify(run: dict[str, Any], config: dict[str, Any] | None = None) -> tuple[str, list[str]]:
    """Return (status, reasons). status is 'valid' | 'invalid'. Quarantine (moving
    the artifact) is the CLI's job; the pure verdict is valid/invalid."""
    reasons = validate_run(run, config)
    return ("valid" if not reasons else "invalid"), reasons


def assert_no_cross_story(runs: Iterable[dict[str, Any]]) -> None:
    """Guardrail: Story A and Story B answer different questions and must never
    share a ranking/table. Raises ValueError if a collection mixes stories."""
    stories = {r.get("story") for r in runs}
    stories.discard(None)
    if len(stories) > 1:
        raise ValueError(
            f"cannot combine Story A and Story B in one table/ranking (found stories {sorted(stories)})"
        )
