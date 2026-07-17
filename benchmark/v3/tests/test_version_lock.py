"""benchmark/v3 — version-lock tests (stdlib unittest, offline via injected resolver).

    python tests/test_version_lock.py
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

V3_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(V3_ROOT))

from shared.version_lock.version_lock import diff_locks, resolve_lock  # noqa: E402

GATEWAYS = [
    {"name": "nexus", "channel": "main", "source": "build-from-source"},
    {"name": "litellm", "channel": "main-latest", "source": "ghcr.io/berriai/litellm"},
]


def fake_resolver(pins):
    """Return a resolver that maps gateway name → (version, digest, commit)."""
    def _resolve(spec):
        return pins[spec["name"]]
    return _resolve


class ResolveLock(unittest.TestCase):
    def test_first_resolve_pins_all_and_marks_all_changed(self):
        r = fake_resolver({
            "nexus": {"resolved_version": "0.9.0", "image_digest": "sha256:" + "1" * 64, "commit": "abc1234"},
            "litellm": {"resolved_version": "1.40.0", "image_digest": "sha256:" + "2" * 64},
        })
        lock = resolve_lock(GATEWAYS, r, resolved_at="2026-07-17T00:00:00Z")
        self.assertEqual(lock["schema"], "versions.lock/v1")
        self.assertEqual(set(lock["entries"]), {"nexus", "litellm"})
        self.assertEqual(lock["entries"]["nexus"]["commit"], "abc1234")
        self.assertEqual(sorted(lock["changed"]), ["litellm", "nexus"])

    def test_unpinned_resolution_refused(self):
        bad = fake_resolver({
            "nexus": {"resolved_version": "0.9.0", "image_digest": ""},  # no digest
            "litellm": {"resolved_version": "1.40.0", "image_digest": "sha256:" + "2" * 64},
        })
        with self.assertRaises(ValueError):
            resolve_lock(GATEWAYS, bad, resolved_at="t")

    def test_only_changed_gateway_flagged_on_reresolve(self):
        r1 = fake_resolver({
            "nexus": {"resolved_version": "0.9.0", "image_digest": "sha256:" + "1" * 64, "commit": "abc"},
            "litellm": {"resolved_version": "1.40.0", "image_digest": "sha256:" + "2" * 64},
        })
        first = resolve_lock(GATEWAYS, r1, resolved_at="t1")
        # litellm bumps, nexus unchanged
        r2 = fake_resolver({
            "nexus": {"resolved_version": "0.9.0", "image_digest": "sha256:" + "1" * 64, "commit": "abc"},
            "litellm": {"resolved_version": "1.41.0", "image_digest": "sha256:" + "3" * 64},
        })
        second = resolve_lock(GATEWAYS, r2, resolved_at="t2", prior_lock=first)
        self.assertEqual(second["changed"], ["litellm"])
        self.assertEqual(diff_locks(first, second), ["litellm"])

    def test_identical_reresolve_no_change(self):
        r = fake_resolver({
            "nexus": {"resolved_version": "0.9.0", "image_digest": "sha256:" + "1" * 64, "commit": "abc"},
            "litellm": {"resolved_version": "1.40.0", "image_digest": "sha256:" + "2" * 64},
        })
        first = resolve_lock(GATEWAYS, r, resolved_at="t1")
        second = resolve_lock(GATEWAYS, r, resolved_at="t2", prior_lock=first)
        self.assertEqual(second["changed"], [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
