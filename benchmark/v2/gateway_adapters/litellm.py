"""LiteLLM proxy adapter."""
from __future__ import annotations
from typing import Any
from gateway_adapters.base import BaseGatewayAdapter
from engine.models import GatewayFullConfig


class LiteLLMAdapter(BaseGatewayAdapter):
    def _extra_body(self) -> dict[str, Any]:
        # LiteLLM passes through standard OpenAI body — no extras needed
        return {}

    @classmethod
    def from_config(cls, config: GatewayFullConfig, model: str = "gpt-4o") -> "LiteLLMAdapter":
        return cls(
            base_url=config.gateway.base_url,
            api_key=config.gateway.api_key,
            model=model,
            stream=config.request.stream,
        )
