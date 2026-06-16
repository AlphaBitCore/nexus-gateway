"""
OpenAI-compatible chat completions adapter.

Assumption: the target gateway exposes POST /v1/chat/completions with the
OpenAI Chat Completions request/response shape. This is the default wire format
for Nexus, LiteLLM, and Bifrost when they operate in OpenAI-proxy mode.

If your gateway deviates — different endpoint path, different response envelope,
streaming-only, etc. — subclass this and override the relevant methods.
"""

from __future__ import annotations

from typing import Any

from src.adapters.base import GatewayAdapter
from src.config import BenchmarkConfig


class OpenAICompatAdapter(GatewayAdapter):
    """
    Standard OpenAI Chat Completions wire format.

    Request shape:
      POST /v1/chat/completions
      {"model": str, "messages": [...], "max_tokens": int, ...}

    Response shape:
      {"choices": [{"message": {"role": "assistant", "content": str}}], ...}
    """

    def __init__(self, config: BenchmarkConfig) -> None:
        self._url = config.base_url.rstrip("/") + config.endpoint
        self._base_headers = {
            "Content-Type": "application/json",
            **config.auth.resolved_headers(),
        }
        self._payload_template = config.payload

    def build_request(self, prompt: str, payload_template: Any) -> dict[str, Any]:
        body: dict[str, Any] = {
            "model": payload_template.model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": payload_template.max_tokens,
            "temperature": payload_template.temperature,
            "stream": payload_template.stream,
            **payload_template.extra,
        }
        return {
            "method": "POST",
            "url": self._url,
            "headers": self._base_headers,
            "json": body,
        }

    def validate_response(
        self, status_code: int, body: dict[str, Any]
    ) -> tuple[bool, str]:
        if status_code != 200:
            return False, f"HTTP {status_code}"
        if "choices" not in body:
            return False, "missing 'choices' key"
        choices = body.get("choices") or []
        if not choices:
            return False, "empty 'choices' array"
        content = self.extract_content(body)
        if not content or not content.strip():
            return False, "empty content"
        return True, "ok"

    def extract_content(self, body: dict[str, Any]) -> str:
        try:
            return body["choices"][0]["message"]["content"]
        except (KeyError, IndexError, TypeError):
            return ""
