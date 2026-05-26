"""
Smoke test for the Judge wrapper itself.

These tests do not exercise gateway policy — they only confirm that:
- The Kimi 128k model is reachable through our Nexus VK.
- The judge returns a parseable JSON verdict for trivially-true and
  trivially-false assertions.

If this module is red, every other AI-judge test will be red for the same
reason; running it first gives operators a clean failure to point at.
"""

from __future__ import annotations

import pytest

pytestmark = pytest.mark.ai_judge


def test_judge_passes_on_trivially_true_evidence(nexus_judge):
    verdict = nexus_judge.evaluate(
        question="Did the user request return HTTP 200?",
        evidence={"http_status": 200, "body": '{"ok":true}'},
    )
    assert verdict.passed, (
        f"expected pass, got {verdict.verdict} ({verdict.reasoning})"
    )
    assert verdict.confidence > 0.5


def test_judge_fails_on_trivially_false_evidence(nexus_judge):
    verdict = nexus_judge.evaluate(
        question="Did the user request return HTTP 200?",
        evidence={"http_status": 500, "body": '{"error":"boom"}'},
    )
    assert not verdict.passed, (
        f"expected fail, got {verdict.verdict} ({verdict.reasoning})"
    )
