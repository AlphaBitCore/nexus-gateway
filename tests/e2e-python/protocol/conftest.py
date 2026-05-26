"""
Phase 5 protocol-compat fixtures.

We construct fresh httpx.Client + SDK instances per test (not session-wide)
because some SDKs (notably anthropic) hold connections alive past the
fixture teardown otherwise. trust_env=False is mandatory: a workstation
HTTP_PROXY at 127.0.0.1:10080 silently rewrites localhost calls into
opaque 502s otherwise — see Phase 4's judge.py for the same fix.
"""

from __future__ import annotations

import httpx
import pytest

# Concrete model IDs the dev DB seeds + the local providers expose. If a
# fresh dev DB ships different IDs, list_models() will surface a clearer
# error than a hardcoded id mismatch buried in an SDK call.
OPENAI_TEST_MODEL = "moonshot-v1-8k"      # Moonshot speaks OpenAI wire
ANTHROPIC_TEST_MODEL = "claude-haiku-4-5-20251001"  # cheapest Claude on the dev DB


@pytest.fixture()
def openai_client(nexus_env):
    """openai.OpenAI pointed at our /v1, with the dev VK and proxy bypass."""
    from openai import OpenAI

    http = httpx.Client(timeout=30.0, trust_env=False)
    client = OpenAI(
        base_url=nexus_env["NEXUS_AI_GW_URL"] + "/v1",
        api_key=nexus_env["NEXUS_TEST_VK"],
        http_client=http,
    )
    yield client
    http.close()


@pytest.fixture()
def anthropic_client(nexus_env):
    """anthropic.Anthropic pointed at our gateway, with proxy bypass.

    The SDK appends /v1/messages itself, so base_url is the host root.
    The dev gateway treats the Nexus VK as both auth AND the upstream
    Anthropic credential indirector — no separate API key dance needed.
    """
    from anthropic import Anthropic

    http = httpx.Client(timeout=30.0, trust_env=False)
    client = Anthropic(
        base_url=nexus_env["NEXUS_AI_GW_URL"],
        api_key=nexus_env["NEXUS_TEST_VK"],
        http_client=http,
    )
    yield client
    http.close()
