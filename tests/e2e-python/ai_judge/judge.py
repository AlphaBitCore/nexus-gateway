"""
ai_judge.judge — Kimi 128k as a test oracle, accessed *through* our own
AI Gateway via a Nexus virtual key.

Why a single judge wrapper:
- Every AI-judge call has the same shape: "here is the question, here is
  the evidence, return a strict JSON verdict". Centralising that contract
  here means individual tests stay focused on the policy under test.
- The judge runs against `moonshot-v1-128k` by default — Kimi's 128k window
  is wide enough to fit large evidence bundles (raw audit rows, full
  request + response bodies) without manual truncation.
- All judge calls are dogfooding: they exercise /v1/chat/completions on
  the local AI Gateway, so a regression in our own gateway surfaces as
  an AI-judge failure during the test run.

The judge contract:
- Caller passes a `question` (the policy assertion under test) and an
  `evidence` dict (anything relevant: prompts, responses, audit rows).
- Judge sends a system prompt that demands strict JSON
  ({verdict, confidence, reasoning, ...}) and parses the first valid JSON
  object out of the response.
- temperature=0 + JSON-only system prompt give us reproducible verdicts
  for the same evidence.
"""

from __future__ import annotations

import json
import logging
import re
import time
from dataclasses import dataclass, field
from typing import Any

import httpx
from openai import OpenAI

logger = logging.getLogger(__name__)


def _make_local_client(base_url: str) -> httpx.Client:
    """Build an httpx.Client that ignores any system proxy for our base URL.

    Many dev workstations have HTTP_PROXY / HTTPS_PROXY pointing at a local
    proxy (e.g. for routing dev traffic through a corporate egress). Without
    this bypass, requests to http://localhost:3050 get rerouted through the
    proxy and come back as 502. curl doesn't pick up these env vars by
    default, which is why a curl smoke test passes while a Python SDK call
    against the same URL fails — and exactly the trap we want to insulate
    every Python test from.
    """
    return httpx.Client(timeout=httpx.Timeout(60.0), trust_env=False)


_SYSTEM_PROMPT = """\
You are an automated test oracle for the Nexus Gateway end-to-end test
suite. Your job is to read a *question* about an event in the system and
the *evidence* gathered for that event, and return a strict JSON verdict.

Output rules — failure to follow these means the test fails:
1. Output ONLY a single JSON object. No prose before or after.
2. Required keys:
   - "verdict": "pass" | "fail"
   - "confidence": float in [0, 1]
   - "reasoning": short explanation (≤ 3 sentences)
3. Optional keys: any additional fields the question explicitly asked for.
4. Use "pass" only when the evidence clearly supports the assertion in
   the question. Use "fail" if the evidence contradicts it OR is
   insufficient. Never hallucinate facts that are not in the evidence.

When in doubt, prefer "fail" with low confidence — false-positive passes
are worse than false-negative fails for a test oracle.
"""


@dataclass
class JudgeVerdict:
    verdict: str  # "pass" | "fail"
    confidence: float
    reasoning: str
    raw: dict[str, Any] = field(default_factory=dict)
    model: str = ""
    latency_ms: int = 0

    @property
    def passed(self) -> bool:
        return self.verdict == "pass"


class Judge:
    """Thin wrapper around the Kimi 128k chat completion API.

    Constructed with the test config: base_url points at the local AI
    Gateway's /v1/, api_key is a Nexus VK with allowedModels covering the
    judge model. The OpenAI SDK is used because the AI Gateway exposes an
    OpenAI-compatible surface; this also makes the wrapper trivial to
    repoint at any other compatible endpoint for cross-checks.
    """

    def __init__(self, *, base_url: str, api_key: str, model: str) -> None:
        self._client = OpenAI(
            base_url=base_url,
            api_key=api_key,
            http_client=_make_local_client(base_url),
        )
        self._model = model

    @property
    def model(self) -> str:
        return self._model

    def evaluate(self, question: str, evidence: dict[str, Any]) -> JudgeVerdict:
        """Run one judging call.

        The evidence dict is JSON-serialised verbatim into the user message,
        so callers should keep it small enough to fit Kimi 128k's window
        (we have plenty of margin in practice; truncate only if you're
        passing megabytes of bytes).
        """
        evidence_json = json.dumps(evidence, ensure_ascii=False, indent=2, default=str)
        user = (
            f"Question: {question}\n\n"
            f"Evidence (JSON):\n{evidence_json}\n\n"
            "Return your verdict as a single JSON object as specified."
        )
        started = time.time()
        resp = self._client.chat.completions.create(
            model=self._model,
            messages=[
                {"role": "system", "content": _SYSTEM_PROMPT},
                {"role": "user", "content": user},
            ],
            temperature=0,
            max_tokens=512,
        )
        latency_ms = int((time.time() - started) * 1000)
        text = resp.choices[0].message.content or ""
        parsed = _extract_json(text)
        if parsed is None:
            raise AssertionError(
                f"Judge returned non-JSON output (latency={latency_ms}ms): {text!r}"
            )
        verdict = parsed.get("verdict")
        if verdict not in {"pass", "fail"}:
            raise AssertionError(
                f"Judge verdict must be 'pass' or 'fail', got {verdict!r} (raw={parsed})"
            )
        try:
            confidence = float(parsed.get("confidence", 0.0))
        except (TypeError, ValueError):
            confidence = 0.0
        reasoning = str(parsed.get("reasoning", ""))
        v = JudgeVerdict(
            verdict=verdict,
            confidence=confidence,
            reasoning=reasoning,
            raw=parsed,
            model=self._model,
            latency_ms=latency_ms,
        )
        logger.info(
            "judge verdict=%s confidence=%.2f model=%s latency_ms=%d",
            v.verdict, v.confidence, v.model, v.latency_ms,
        )
        return v


_JSON_OBJECT = re.compile(r"\{[\s\S]*\}")


def _extract_json(text: str) -> dict[str, Any] | None:
    """Best-effort extract the first top-level JSON object.

    Even with a strict system prompt, Kimi will occasionally wrap the
    JSON in markdown fences. Strip ```json``` if present, then either
    parse directly or fall back to the first {...} block.
    """
    cleaned = text.strip()
    if cleaned.startswith("```"):
        cleaned = re.sub(r"^```(?:json)?\s*", "", cleaned)
        cleaned = re.sub(r"\s*```$", "", cleaned)
    try:
        return json.loads(cleaned)
    except json.JSONDecodeError:
        pass
    match = _JSON_OBJECT.search(cleaned)
    if not match:
        return None
    try:
        return json.loads(match.group(0))
    except json.JSONDecodeError:
        return None
