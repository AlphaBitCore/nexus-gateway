"""benchmark/v3 — validator rule tests (stdlib unittest).

Asserts each pre-registered invalidation rule fires on the named bad input, that a
clean run passes, that rules are SYMMETRIC across gateways (the fix for v2's
litellm-only gate), and that Story A/B cannot be merged. No third-party deps.

    python -m pytest tests/test_validator.py     # if pytest present
    python tests/test_validator.py                # plain unittest
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

V3_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(V3_ROOT))

from shared.validation.validator import (  # noqa: E402
    assert_consistent_deployment,
    assert_no_cross_story,
    classify,
    validate_run,
)


def valid_run(gateway: str = "nexus") -> dict:
    """A fully-valid Story A run. Tests mutate a copy to exercise one rule each."""
    return {
        "schema_version": "v3",
        "story": "A",
        "run_id": "11111111-1111-1111-1111-111111111111",
        "run_mode": "continuous_latency",
        "gateway": {
            "name": gateway,
            "version": "1.2.3",
            "image_digest": "sha256:" + "a" * 64,
            "commit": "deadbeef",
            "tuning": {"workers": 4, "config_reference": "config/gateways.yaml"},
        },
        "offered_load": {
            "model": "open_loop",
            "target_rps": 50,
            "duration_seconds": 300,
            "warmup_seconds": 60,
        },
        "environment": {
            "launch_type": "fargate",
            "cpu_model": "Intel(R) Xeon(R) Platinum 8375C",
            "cpu_vcpus": 4,
            "memory_mb": 8192,
            "disk_type": "gp3",
            "disk_iops": 12000,
            "disk_throughput_mibps": 1000,
            "audit_writes_enabled": False,
            "deployment_method": "container",
            "services_running": ["nexus-ai-gateway", "nexus-mock-provider"],
        },
        "upstream": {
            "name": "nexus-mock-provider",
            "expected_response_id": "chatcmpl-llm-mock",
            "verified": True,
        },
        "outcomes": {
            "total": 100,
            "success": 98,
            "timeout": 1,
            "http_4xx": 0,
            "http_5xx": 1,
            "broken_stream": 0,
            "content_invalid": 0,
        },
        "latency": {
            "p50_ms": 40, "p95_ms": 80, "p99_ms": 120,
            "ttft_p50_ms": 20, "ttft_p95_ms": 50, "co_corrected": True,
        },
        "validation": {"status": "valid", "reasons": [], "validator_version": "v3.0.0"},
        "artifacts": {
            "raw_results_uri": "s3://bucket/raw.json",
            "provenance_uri": "s3://bucket/prov.json",
            "versions_lock_uri": "s3://bucket/versions.lock",
        },
    }


class ValidRunPasses(unittest.TestCase):
    def test_clean_run_is_valid(self):
        status, reasons = classify(valid_run())
        self.assertEqual(status, "valid", reasons)
        self.assertEqual(reasons, [])


class Rules(unittest.TestCase):
    def _reasons(self, mutate) -> list[str]:
        r = valid_run()
        mutate(r)
        return validate_run(r)

    def _assert_flagged(self, reasons, needle):
        self.assertTrue(any(needle in x for x in reasons), f"expected '{needle}' in {reasons}")

    def test_closed_loop_story_a_rejected(self):
        r = self._reasons(lambda r: r["offered_load"].update(model="closed_loop"))
        self._assert_flagged(r, "open_loop")

    def test_open_loop_required_only_for_latency_mode(self):
        # a saturation campaign may be closed-loop — the open-loop rule must NOT fire
        def m(r):
            r["story"] = "B"
            r["run_mode"] = "saturation_campaign"
            r["offered_load"]["model"] = "closed_loop"
        reasons = self._reasons(m)
        self.assertFalse(any("open_loop" in x for x in reasons), reasons)

    def test_upstream_unverified_invalidates(self):
        r = self._reasons(lambda r: r["upstream"].update(verified=False))
        self._assert_flagged(r, "upstream content check")

    def test_outcome_accounting_must_sum_to_total(self):
        # drop a success so 97 != 100
        r = self._reasons(lambda r: r["outcomes"].update(success=95))
        self._assert_flagged(r, "do not sum to total")

    def test_all_terminal_outcomes_counted(self):
        # a run that has failures but accounts for every one stays valid
        def m(r):
            r["outcomes"] = {
                "total": 100, "success": 50, "timeout": 4, "http_4xx": 10,
                "http_5xx": 6, "broken_stream": 15, "content_invalid": 15,
            }
        reasons = self._reasons(m)
        self.assertFalse(any("sum to total" in x for x in reasons), reasons)

    def test_timeout_rate_ceiling(self):
        def m(r):
            r["outcomes"] = {
                "total": 100, "success": 80, "timeout": 20, "http_4xx": 0,
                "http_5xx": 0, "broken_stream": 0, "content_invalid": 0,
            }
        self._assert_flagged(self._reasons(m), "timeout rate")

    def test_burstable_ec2_rejected(self):
        def m(r):
            r["environment"]["launch_type"] = "ec2"
            r["environment"]["instance_type"] = "t3.xlarge"
        self._assert_flagged(self._reasons(m), "burstable")

    def test_fixed_class_ec2_allowed(self):
        def m(r):
            r["environment"]["launch_type"] = "ec2"
            r["environment"]["instance_type"] = "c6i.4xlarge"
        reasons = self._reasons(m)
        self.assertFalse(any("burstable" in x for x in reasons), reasons)

    def test_disk_spec_required_when_audit_writes_enabled(self):
        def m(r):
            r["environment"]["audit_writes_enabled"] = True
            r["environment"]["disk_iops"] = 0
            r["environment"]["disk_throughput_mibps"] = 0
        self._assert_flagged(self._reasons(m), "disk IOPS/throughput")

    def test_floating_tag_rejected(self):
        r = self._reasons(lambda r: r["gateway"].update(version="latest"))
        self._assert_flagged(r, "floating tag")

    def test_missing_digest_rejected(self):
        r = self._reasons(lambda r: r["gateway"].update(image_digest=""))
        self._assert_flagged(r, "not pinned by a sha256 digest")

    def test_missing_versions_lock_rejected(self):
        r = self._reasons(lambda r: r["artifacts"].update(versions_lock_uri=""))
        self._assert_flagged(r, "versions.lock")

    def test_missing_raw_artifact_rejected(self):
        r = self._reasons(lambda r: r["artifacts"].update(raw_results_uri=""))
        self._assert_flagged(r, "raw results artifact")

    def test_missing_provenance_rejected(self):
        r = self._reasons(lambda r: r["artifacts"].update(provenance_uri=""))
        self._assert_flagged(r, "provenance artifact")

    def test_missing_warmup_rejected(self):
        r = self._reasons(lambda r: r["offered_load"].update(warmup_seconds=0))
        self._assert_flagged(r, "warmup")

    def test_unlisted_services_rejected(self):
        r = self._reasons(lambda r: r["environment"].update(services_running=[]))
        self._assert_flagged(r, "services_running is empty")

    def test_unmeasured_service_on_box_rejected(self):
        # Tieben's bar: compliance-proxy (unmeasured) on the box under test skews it.
        r = self._reasons(lambda r: r["environment"]["services_running"].append("compliance-proxy"))
        self._assert_flagged(r, "unmeasured service")

    def test_deployment_method_required(self):
        r = self._reasons(lambda r: r["environment"].pop("deployment_method", None))
        self._assert_flagged(r, "deployment_method")
        # an unknown value is also rejected
        r2 = self._reasons(lambda r: r["environment"].update(deployment_method="k8s"))
        self._assert_flagged(r2, "deployment_method")


class Symmetry(unittest.TestCase):
    """The fix for v2's litellm-only gate: an identical bad input must invalidate
    the run for EVERY gateway, not just one."""

    def test_same_bad_input_invalidates_all_gateways(self):
        for gw in ("nexus", "litellm", "bifrost", "tensorzero", "portkey", "kong"):
            r = valid_run(gateway=gw)
            r["offered_load"]["model"] = "closed_loop"  # same bad input
            status, reasons = classify(r)
            self.assertEqual(status, "invalid", f"{gw} should be invalid")
            self.assertTrue(any("open_loop" in x for x in reasons), f"{gw}: {reasons}")

    def test_clean_run_valid_for_all_gateways(self):
        for gw in ("nexus", "litellm", "bifrost"):
            status, _ = classify(valid_run(gateway=gw))
            self.assertEqual(status, "valid", gw)


class CrossStory(unittest.TestCase):
    def test_single_story_ok(self):
        assert_no_cross_story([{"story": "A"}, {"story": "A"}])  # must not raise

    def test_mixed_stories_raises(self):
        with self.assertRaises(ValueError):
            assert_no_cross_story([{"story": "A"}, {"story": "B"}])


class CrossDeployment(unittest.TestCase):
    def _run(self, method):
        return {"environment": {"deployment_method": method}}

    def test_consistent_deployment_ok(self):
        assert_consistent_deployment([self._run("container"), self._run("container")])

    def test_mixed_deployment_raises(self):
        # native + container in one comparison → container overhead would masquerade
        # as a gateway difference (Tieben's bar).
        with self.assertRaises(ValueError):
            assert_consistent_deployment([self._run("native"), self._run("container")])


class V2Quarantined(unittest.TestCase):
    """v2 results must be visibly marked PRE-GATE — NOT PUBLISHABLE."""

    def setUp(self):
        self.v2 = V3_ROOT.parent / "v2"

    def test_marker_file_present(self):
        marker = self.v2 / "results" / "PRE-GATE-NOT-PUBLISHABLE.md"
        self.assertTrue(marker.exists(), "PRE-GATE marker missing")
        self.assertIn("NOT PUBLISHABLE", marker.read_text(encoding="utf-8"))

    def test_flagship_report_carries_banner(self):
        report = self.v2 / "results" / "NEXUS-BENCHMARK-FULL-REPORT.md"
        if not report.exists():
            self.skipTest("flagship report not present in this checkout")
        head = report.read_text(encoding="utf-8")[:400]
        self.assertIn("PRE-GATE", head)


if __name__ == "__main__":
    unittest.main(verbosity=2)
