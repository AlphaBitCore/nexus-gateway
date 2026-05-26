"""
End-to-end AI-judge test for the PII Detector hook (E5 in the flow catalog).

Flow under test:
1. Send a chat completion containing an SSN through the AI Gateway with a
   real Nexus VK.
2. Expect the gateway to reject upstream traffic with HTTP 403 and a body
   that names the PII type detected.
3. Wait for the corresponding traffic_event audit row to land in Postgres.
4. Hand the gateway response, the original prompt, and the audit row to
   the Kimi 128k judge with the assertion: "Did the system correctly
   detect PII in the user's prompt and reject the request, with a clear
   user-facing reason?"

This is the smallest gateway-policy test that genuinely needs an AI oracle
— a deterministic test could only check "status was 403", but the judge
also evaluates whether the rejection reason was meaningful and whether the
hook actually keyed off PII (vs. some unrelated rule). It is also the
flow most likely to break silently: a regression that downgrades 403 to
"approve, just classify" would still produce a 200 with content, and
deterministic tests would have to hardcode every keyword to catch it.
"""

from __future__ import annotations

import re
import time

import httpx
import psycopg
import pytest

pytestmark = pytest.mark.ai_judge

# Fake but well-formed PII designed to trip the SSN + email detectors.
# 123-45-6789 is the "always-invalid" SSN sentinel — safe even if a future
# regression starts logging prompts.
_PROMPT = (
    "Please remember the following customer record for our internal "
    "reference: name John Doe, SSN 123-45-6789, email jdoe@example.com. "
    "Summarise it in one sentence."
)
_KNOWN_PII_TYPES = ["SSN", "EMAIL"]


# AI-judge dogfooding hazard: the judge call itself flows through our own
# AI Gateway, which means raw PII in evidence triggers the same detector
# we're trying to test. We replace concrete PII tokens with placeholder
# markers before serialising — the judge knows from the question what
# markers mean, so the policy reasoning is unchanged. Keep this list small
# and conservative; we want the judge to reject ambiguity, not pattern-
# match its way to a verdict.
_SSN_RE = re.compile(r"\b\d{3}-\d{2}-\d{4}\b")
_EMAIL_RE = re.compile(r"\b[\w.+-]+@[\w-]+\.[\w.-]+\b")


def _sanitize_for_judge(text: str) -> str:
    """Replace PII patterns with type markers so judge calls don't self-trip."""
    text = _SSN_RE.sub("<SSN-MARKER>", text)
    text = _EMAIL_RE.sub("<EMAIL-MARKER>", text)
    return text


def _send_chat(base_url: str, vk: str, prompt: str) -> httpx.Response:
    # trust_env=False bypasses any system HTTP_PROXY that would otherwise
    # reroute localhost calls through a dev proxy and surface as opaque
    # 502s. See ai_judge.judge._make_local_client for the same fix.
    with httpx.Client(trust_env=False, timeout=30.0) as client:
        return client.post(
            f"{base_url}/v1/chat/completions",
            headers={
                "Authorization": f"Bearer {vk}",
                "Content-Type": "application/json",
            },
            json={
                "model": "moonshot-v1-8k",
                "messages": [{"role": "user", "content": prompt}],
                "max_tokens": 32,
                "temperature": 0,
            },
        )


def _wait_for_rejected_audit_row(
    conn: psycopg.Connection, *, deadline_seconds: float = 45.0
) -> dict | None:
    """Poll traffic_event for the most recent PII-rejected ai-gateway row.

    The audit pipeline is async (gateway → MQ → Hub consumer → DB) with
    typical latency around 10 s in local dev and worst-case spikes up to
    ~30 s under MQ batching. We cap the wait at 45 s and search a 90 s
    window so a slightly delayed write still matches against this run.
    Filter by status_code=403 + REJECT_HARD so a concurrent successful
    chat call (e.g. from another test in the same suite) does not race
    in and produce a false match.
    """
    deadline = time.time() + deadline_seconds
    while time.time() < deadline:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT id, status_code, request_hook_decision,
                       request_hooks_pipeline, model_name
                FROM traffic_event
                WHERE source = 'ai-gateway'
                  AND path   = '/v1/chat/completions'
                  AND status_code = 403
                  AND request_hook_decision = 'REJECT_HARD'
                  AND "timestamp" > NOW() - INTERVAL '90 seconds'
                ORDER BY "timestamp" DESC
                LIMIT 1
                """
            )
            row = cur.fetchone()
        conn.commit()
        if row:
            return {
                "id": row[0],
                "status_code": row[1],
                "request_hook_decision": row[2],
                "request_hooks_pipeline": row[3],
                "model_name": row[4],
            }
        time.sleep(1.0)
    return None


def test_pii_in_prompt_is_rejected_and_judge_agrees(nexus_env, nexus_judge, nexus_db):
    # 1. Drive the gateway end to end.
    response = _send_chat(
        nexus_env["NEXUS_AI_GW_URL"], nexus_env["NEXUS_TEST_VK"], _PROMPT,
    )

    # 2. Cheap deterministic assertion first: it MUST reject. If this fails
    #    the AI-judge step is meaningless, so bail early.
    assert response.status_code == 403, (
        f"PII detector did not reject: HTTP {response.status_code} body={response.text[:200]}"
    )
    body = response.text  # text not json — error envelope format may vary
    # Sanity: the rejection body should at least mention PII or SSN.
    assert "pii" in body.lower() or "ssn" in body.lower(), (
        f"rejection body does not mention PII/SSN: {body[:200]}"
    )

    # 3. Wait for the audit row. Filter is `status_code=403 AND
    #    request_hook_decision='REJECT_HARD'` so a concurrent 200 from
    #    another test does not race in — see _wait_for_rejected_audit_row.
    audit = _wait_for_rejected_audit_row(nexus_db)
    assert audit is not None, (
        "no PII-rejected traffic_event row (status=403, REJECT_HARD) "
        "appeared within 30s — audit pipeline behind?"
    )

    # 4. AI-judge: hand the (sanitised) prompt, response, and audit row to
    #    Kimi. Sanitisation replaces concrete PII tokens with markers so
    #    the judge call itself does not trip the gateway's PII detector
    #    — see _sanitize_for_judge. We tell the judge what markers mean so
    #    its policy reasoning is unaffected.
    verdict = nexus_judge.evaluate(
        question=(
            "An AI Gateway received a user prompt that originally contained "
            "real instances of the following PII types: " + ", ".join(_KNOWN_PII_TYPES) + ". "
            "Those instances have been replaced in this evidence with markers "
            "of the form <SSN-MARKER> and <EMAIL-MARKER> so this oracle call "
            "does not re-trigger the detector. The gateway is supposed to "
            "detect at least one of those PII types and reject the request "
            "before it reaches the upstream model. Given the (sanitised) "
            "prompt, the gateway's HTTP response, and the audit row from the "
            "traffic log, did the gateway: (a) correctly identify PII in the "
            "original prompt, and (b) reject the request with a user-visible "
            "reason that names the PII type detected? Both conditions must "
            "hold for verdict=pass."
        ),
        evidence={
            "prompt_sanitized": _sanitize_for_judge(_PROMPT),
            "pii_types_present_in_original": _KNOWN_PII_TYPES,
            "gateway_http_status": response.status_code,
            "gateway_response_body": _sanitize_for_judge(body),
            "audit_row": audit,
        },
    )

    # The judge writes its own reasoning into the verdict; surface it on
    # failure so the next operator can see *why* Kimi disagreed without
    # re-running anything.
    assert verdict.passed, (
        f"AI-judge said FAIL (confidence={verdict.confidence:.2f}): "
        f"{verdict.reasoning}\nFull verdict raw: {verdict.raw}"
    )
    assert verdict.confidence >= 0.6, (
        f"AI-judge confidence too low ({verdict.confidence:.2f}): {verdict.reasoning}"
    )
