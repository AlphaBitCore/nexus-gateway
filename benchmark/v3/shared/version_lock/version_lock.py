"""benchmark/v3 — version resolution + lockfile.

The longitudinal axis: resolve each gateway's release channel to an EXACT
version + image digest (and, for Nexus, a source commit), write it to
versions.lock, and detect when a lock entry changes so a latency time series can
be annotated at version boundaries ("did the new LiteLLM release regress TTFT" is
then a lookup, not a re-run).

`resolve_lock` takes an injected `resolver` callable so it is fully testable
offline — the CLI (scripts/resolve_versions.py) supplies a real resolver that
reads pinned digests from config or queries a registry. Stdlib-only.
"""
from __future__ import annotations

from typing import Any, Callable

LOCK_SCHEMA = "versions.lock/v1"

# resolver(gateway_spec: dict) -> {"resolved_version": str, "image_digest": str, "commit": str|None}
Resolver = Callable[[dict[str, Any]], dict[str, Any]]


def resolve_lock(
    gateways: list[dict[str, Any]],
    resolver: Resolver,
    resolved_at: str | None = None,
    prior_lock: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Resolve every gateway channel to an exact pin and return a lock dict.

    `changed` lists the gateway names whose resolved pin differs from prior_lock
    (all names on a first resolve). Raises ValueError if the resolver returns an
    entry without both a resolved_version and an image_digest — an unpinned
    result is never written to the lock.
    """
    prior_entries = (prior_lock or {}).get("entries", {}) if prior_lock else {}
    entries: dict[str, Any] = {}
    changed: list[str] = []

    for spec in gateways:
        name = spec.get("name")
        if not name:
            raise ValueError(f"gateway spec missing 'name': {spec!r}")
        resolved = resolver(spec) or {}
        version = resolved.get("resolved_version")
        digest = resolved.get("image_digest")
        if not version or not digest:
            raise ValueError(
                f"resolver returned an unpinned result for '{name}' "
                f"(version={version!r}, digest={digest!r}) — refusing to lock"
            )
        entry = {
            "channel": spec.get("channel"),
            "resolved_version": version,
            "image_digest": digest,
            "commit": resolved.get("commit"),
            "source": spec.get("source"),
        }
        entries[name] = entry
        prior = prior_entries.get(name)
        if prior is None or (
            prior.get("resolved_version") != version or prior.get("image_digest") != digest
        ):
            changed.append(name)

    return {
        "schema": LOCK_SCHEMA,
        "resolved_at": resolved_at,
        "entries": entries,
        "changed": changed,
    }


def diff_locks(old: dict[str, Any] | None, new: dict[str, Any]) -> list[str]:
    """Names whose pin differs between two locks (added or version/digest change)."""
    old_entries = (old or {}).get("entries", {})
    new_entries = new.get("entries", {})
    changed = []
    for name, entry in new_entries.items():
        prev = old_entries.get(name)
        if prev is None or (
            prev.get("resolved_version") != entry.get("resolved_version")
            or prev.get("image_digest") != entry.get("image_digest")
        ):
            changed.append(name)
    return changed
