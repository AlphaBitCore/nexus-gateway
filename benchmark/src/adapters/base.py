"""
Abstract gateway adapter.

Each concrete adapter knows how to:
  1. Build an HTTP request dict from a prompt + PayloadTemplate.
  2. Validate the HTTP response structurally.
  3. Extract the text content from a valid response.

This abstraction lets the benchmark engine stay gateway-agnostic. Adding
support for a new gateway shape (e.g. a non-OpenAI wire format) only requires
a new subclass here — the engine, metrics, and reporting layers are unchanged.
"""

from __future__ import annotations

import abc
from typing import Any


class GatewayAdapter(abc.ABC):

    @abc.abstractmethod
    def build_request(self, prompt: str, payload_template: Any) -> dict[str, Any]:
        """
        Return kwargs ready for httpx.AsyncClient.request():
          {"method": str, "url": str, "headers": dict, "json": dict}
        """
        ...

    @abc.abstractmethod
    def validate_response(
        self, status_code: int, body: dict[str, Any]
    ) -> tuple[bool, str]:
        """Return (is_valid, reason_string)."""
        ...

    @abc.abstractmethod
    def extract_content(self, body: dict[str, Any]) -> str:
        """Extract the text content from a successful response body."""
        ...
