"""Unit tests for scripts/validate_benchmark.py — the result-integrity gate.
Encodes the June-16 / July-14 invalidities as named cases."""
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))

import validate_benchmark as vb  # noqa: E402

BLOCK, WARN = vb.BLOCK, vb.WARN


def _doc(results, env=None):
    e = {"bench_unique_prompts": True, "run_mode": "sequential"}
    if env:
        e.update(env)
    return {"run_id": "t", "environment": e, "results": results}


def _levels(issues):
    return {level for level, _ in issues}


class TestHooksIntegrity(unittest.TestCase):
    def test_valid_hooks_off_passes(self):
        r = {"gateway": "nexus", "governance": {
            "requested_mode": "hooks-off", "gateway_runtime_verified": True,
            "runtime_response_hook_count": 0, "audit_disabled_env_present": False}}
        issues = vb.check_result_file(_doc([r]), 4.0)
        self.assertEqual(issues, [])

    def test_hooks_off_without_runtime_proof_BLOCKS(self):
        r = {"gateway": "nexus", "governance": {
            "requested_mode": "hooks-off", "gateway_runtime_verified": False,
            "runtime_response_hook_count": None, "audit_disabled_env_present": False}}
        issues = vb.check_result_file(_doc([r]), 4.0)
        self.assertIn(BLOCK, _levels(issues))
        self.assertTrue(any("not proven at the gateway runtime" in m.lower() or "gateway_runtime_verified" in m
                            for _, m in issues))

    def test_hooks_off_with_residual_response_hooks_BLOCKS(self):
        r = {"gateway": "nexus", "governance": {
            "requested_mode": "hooks-off", "gateway_runtime_verified": True,
            "runtime_response_hook_count": 2, "audit_disabled_env_present": False}}
        self.assertIn(BLOCK, _levels(vb.check_result_file(_doc([r]), 4.0)))

    def test_hooks_on_with_zero_count_BLOCKS(self):
        r = {"gateway": "nexus", "governance": {
            "requested_mode": "hooks-on", "gateway_runtime_verified": True,
            "runtime_hook_count": 0, "audit_disabled_env_present": False}}
        self.assertIn(BLOCK, _levels(vb.check_result_file(_doc([r]), 4.0)))

    def test_audit_disabled_on_governed_run_BLOCKS(self):
        r = {"gateway": "nexus", "governance": {
            "requested_mode": "hooks-off", "gateway_runtime_verified": True,
            "runtime_response_hook_count": 0, "audit_disabled_env_present": True}}
        self.assertIn(BLOCK, _levels(vb.check_result_file(_doc([r]), 4.0)))


class TestMethodologyGates(unittest.TestCase):
    def test_unique_prompts_off_BLOCKS(self):
        issues = vb.check_result_file(_doc([], env={"bench_unique_prompts": False}), 4.0)
        self.assertTrue(any("BENCH_UNIQUE_PROMPTS" in m for _, m in issues))
        self.assertIn(BLOCK, _levels(issues))

    def test_non_sequential_BLOCKS(self):
        issues = vb.check_result_file(_doc([], env={"run_mode": "parallel"}), 4.0)
        self.assertIn(BLOCK, _levels(issues))


class TestLiteLLMWarnings(unittest.TestCase):
    def test_litellm_without_warmup_and_not_last_WARNS(self):
        results = [
            {"gateway": "litellm", "ttft_p50_ms": 300, "ttft_p95_ms": 400},   # litellm NOT last
            {"gateway": "nexus", "governance": {"requested_mode": "hooks-on",
                "gateway_runtime_verified": True, "runtime_hook_count": 4}},
        ]
        issues = vb.check_result_file(_doc(results), 4.0)
        self.assertIn(WARN, _levels(issues))
        self.assertTrue(any("did not run last" in m for _, m in issues))
        self.assertTrue(any("no warmup" in m for _, m in issues))
        # warnings only — no block from these
        self.assertNotIn(BLOCK, _levels(issues))

    def test_litellm_p95_anomaly_WARNS(self):
        r = {"gateway": "litellm", "ttft_p50_ms": 300, "ttft_p95_ms": 2000,
             "warmup": {"duration_s": 30}, "resource_observations": {"source_status": "available"},
             "anomaly_status": "anomalous", "infrastructure_status": "unknown", "http_5xx": 1}
        issues = vb.check_result_file(_doc([r]), 4.0)
        self.assertTrue(any("p95/p50" in m for _, m in issues))
        self.assertIn(WARN, _levels(issues))


if __name__ == "__main__":
    unittest.main()
