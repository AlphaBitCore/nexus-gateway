"""Abstract base for all gateway adapters."""
from __future__ import annotations

from abc import ABC, abstractmethod
from typing import Any


class BaseGatewayAdapter(ABC):
    """
    All adapters share the OpenAI-compatible /v1/chat/completions shape.
    New gateways added by subclassing and overriding these three methods.
    """

    def __init__(self, base_url: str, api_key: str | None, model: str,
                 max_tokens: int = 256, temperature: float = 0.0, stream: bool = True,
                 extra_headers: dict[str, str] | None = None):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.max_tokens = max_tokens
        self.temperature = temperature
        self.stream = stream
        self.extra_headers = extra_headers or {}

    def build_request(self, prompt: str) -> tuple[str, dict[str, str], dict[str, Any]]:
        """Return (url, headers, body) for the given prompt."""
        url = f"{self.base_url}/v1/chat/completions"
        headers = self._auth_headers()
        body = {
            "model": self.model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": self.max_tokens,
            "temperature": self.temperature,
            "stream": self.stream,
        }
        body.update(self._extra_body())
        return url, headers, body

    def _auth_headers(self) -> dict[str, str]:
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        headers.update(self.extra_headers)
        return headers

    @abstractmethod
    def _extra_body(self) -> dict[str, Any]:
        """Gateway-specific request body additions."""
        ...

    @classmethod
    def from_config(cls, config) -> "BaseGatewayAdapter":
        raise NotImplementedError
