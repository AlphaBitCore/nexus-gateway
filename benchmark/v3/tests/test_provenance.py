"""benchmark/v3 — provenance capture tests (stdlib unittest, offline via injected sources).

    python tests/test_provenance.py
"""
from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path

V3_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(V3_ROOT))

from shared.provenance.provenance import build_provenance, looks_like_commit  # noqa: E402

PROV_SCHEMA_PATH = V3_ROOT / "shared" / "schemas" / "provenance.schema.json"

try:
    import jsonschema
    HAVE_JSONSCHEMA = True
except ModuleNotFoundError:
    HAVE_JSONSCHEMA = False


def fake_sources(cpu="AMD EPYC 7R13", env=None):
    return {"cpu_model": lambda: cpu, "env": lambda: (env or {})}


def run_context():
    return {
        "captured_at": "2026-07-17T00:00:00Z",
        "gateway": {"name": "nexus", "version": "0.9.0", "image_digest": "sha256:" + "a" * 64, "commit": "abc1234"},
        "nexus_commit": "abc1234",
        "launch_type": "fargate",
        "cpu_vcpus": 4,
        "memory_mb": 8192,
        "disk": {"type": "gp3", "iops": 12000, "throughput_mibps": 1000},
        "loadtest_version": "nexus-loadtest@87b26f0",
        "offered_load": {"model": "open_loop", "target_rps": 50, "warmup_seconds": 60, "duration_seconds": 300},
        "upstream": {"name": "nexus-mock-provider", "expected_response_id": "chatcmpl-llm-mock"},
        "services_running": ["nexus-ai-gateway", "nexus-mock-provider"],
    }


class BuildProvenance(unittest.TestCase):
    def test_captures_injected_cpu_and_context(self):
        prov = build_provenance(fake_sources(cpu="Custom CPU X"), run_context())
        self.assertEqual(prov["schema"], "provenance/v1")
        self.assertEqual(prov["cpu_model"], "Custom CPU X")
        self.assertEqual(prov["gateway"]["image_digest"], "sha256:" + "a" * 64)
        self.assertEqual(prov["services_running"], ["nexus-ai-gateway", "nexus-mock-provider"])
        self.assertEqual(prov["disk"]["iops"], 12000)

    def test_region_read_from_env_when_not_in_context(self):
        prov = build_provenance(fake_sources(env={"AWS_REGION": "us-east-1"}), run_context())
        self.assertEqual(prov["region"], "us-east-1")

    def test_unknown_values_are_null_not_guessed(self):
        ctx = run_context()
        ctx.pop("memory_mb")
        prov = build_provenance(fake_sources(), ctx)
        self.assertIsNone(prov["memory_mb"])  # null, never a fabricated 0

    def test_commit_sanity_checker(self):
        self.assertTrue(looks_like_commit("abc1234"))
        self.assertFalse(looks_like_commit("not-a-sha!"))
        self.assertFalse(looks_like_commit(None))

    @unittest.skipUnless(HAVE_JSONSCHEMA, "jsonschema not installed")
    def test_output_validates_against_provenance_schema(self):
        schema = json.loads(PROV_SCHEMA_PATH.read_text(encoding="utf-8"))
        prov = build_provenance(fake_sources(), run_context())
        errors = list(jsonschema.Draft7Validator(schema).iter_errors(prov))
        self.assertEqual(errors, [], errors)


if __name__ == "__main__":
    unittest.main(verbosity=2)
