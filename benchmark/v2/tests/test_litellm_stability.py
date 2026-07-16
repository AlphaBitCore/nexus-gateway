"""Unit tests for engine.stability — LiteLLM run-order, anomaly classification,
warmup record, and best-effort resource telemetry (null-not-zero)."""
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from engine import stability  # noqa: E402
from engine.metrics import ScenarioMetrics, RequestRecord  # noqa: E402


class TestRunOrder(unittest.TestCase):
    def test_litellm_forced_last(self):
        got = stability.order_gateways(["litellm", "nexus", "bifrost"])
        self.assertEqual(got[-1], "litellm")

    def test_default_order_ranks_nexus_first(self):
        got = stability.order_gateways(["bifrost", "litellm", "nexus"])
        self.assertEqual(got[0], "nexus")
        self.assertEqual(got[-1], "litellm")

    def test_unknown_gateway_kept_but_before_litellm(self):
        got = stability.order_gateways(["litellm", "mystery", "nexus"])
        self.assertLess(got.index("mystery"), got.index("litellm"))

    def test_custom_run_order_honored(self):
        got = stability.order_gateways(["nexus", "bifrost"], run_order=["bifrost", "nexus-hooks-on"])
        self.assertEqual(got[0], "bifrost")


class TestAnomalyClassification(unittest.TestCase):
    def _metric(self, p50, p95, failed=0, timeouts=0):
        m = ScenarioMetrics("litellm", "S-01", "cache-disabled")
        # 80/20 split so numpy's p50 lands at `p50` and p95 at `p95`
        # (a 50/50 split interpolates p50 to the midpoint — not what we want).
        m._ttft_samples = [float(p50)] * 80 + [float(p95)] * 20
        m.total_requests = 100
        m.failed = failed
        m.connection_timeouts = timeouts
        return m

    def test_clean_run_not_flagged(self):
        m = self._metric(300, 500, failed=0)  # low ratio, no errors
        stability.classify_anomaly(m)
        self.assertIsNone(m.anomaly_status)

    def test_high_tail_WITH_errors_is_anomalous(self):
        m = self._metric(300, 2000, failed=5)  # ratio ~6.7 + errors
        stability.classify_anomaly(m)
        self.assertEqual(m.anomaly_status, "anomalous")
        self.assertEqual(m.infrastructure_status, "unknown")

    def test_high_tail_WITHOUT_errors_not_flagged(self):
        # a big tail but zero errors → not called anomalous (evidence rule)
        m = self._metric(300, 2000, failed=0, timeouts=0)
        stability.classify_anomaly(m)
        self.assertIsNone(m.anomaly_status)


class TestWarmupRecord(unittest.TestCase):
    def test_warmup_record_shape(self):
        w = stability.warmup_record(30)
        self.assertEqual(w, {"duration_s": 30, "completed": True, "excluded_from_metrics": True})


class TestResourceSampler(unittest.TestCase):
    def test_no_container_is_unavailable_all_null(self):
        with stability.ResourceSampler(None) as s:
            pass
        obs = s.observations()
        self.assertEqual(obs["source_status"], "unavailable")
        self.assertIsNone(obs["cpu_peak_pct"])
        self.assertIsNone(obs["memory_peak_mb"])
        self.assertIsNone(obs["container_restart_count"])

    def test_observations_fold_in_upstream_counts_from_metric(self):
        m = ScenarioMetrics("litellm", "S-01", "cache-disabled")
        m.http_5xx, m.connection_timeouts, m.stream_timeouts, m.http_4xx = 3, 2, 1, 4
        with stability.ResourceSampler(None) as s:
            pass
        obs = s.observations(m)
        self.assertEqual(obs["upstream_5xx_count"], 3)
        self.assertEqual(obs["timeout_count"], 3)   # 2 conn + 1 stream
        self.assertEqual(obs["upstream_4xx_count"], 4)

    def test_mem_parse_binary_units(self):
        self.assertAlmostEqual(stability._parse_mem_mb("512MiB"), 512.0)
        self.assertAlmostEqual(stability._parse_mem_mb("1.5GiB"), 1536.0)
        self.assertIsNone(stability._parse_mem_mb("garbage"))


if __name__ == "__main__":
    unittest.main()
