"""Bifrost gateway adapter."""
from __future__ import annotations
from typing import Any
from gateway_adapters.base import BaseGatewayAdapter
from engine.models import GatewayFullConfig


class BifrostAdapter(BaseGatewayAdapter):
    def _extra_body(self) -> dict[str, Any]:
        return {}

    @classmethod
    def from_config(cls, config: GatewayFullConfig, model: str = "gpt-4o") -> "BifrostAdapter":
        return cls(
            base_url=config.gateway.base_url,
            api_key=config.gateway.api_key,
            model=model,
            stream=config.request.stream,
        )
