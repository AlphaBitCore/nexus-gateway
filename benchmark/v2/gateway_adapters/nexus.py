"""Nexus Gateway adapter."""
from __future__ import annotations
from typing import Any
from gateway_adapters.base import BaseGatewayAdapter
from engine.models import GatewayFullConfig


class NexusAdapter(BaseGatewayAdapter):
    def __init__(self, *args, virtual_key: str | None = None,
                 cache_enabled: bool = False, **kwargs):
        super().__init__(*args, **kwargs)
        self.virtual_key = virtual_key
        self.cache_enabled = cache_enabled

    def _extra_body(self) -> dict[str, Any]:
        body: dict[str, Any] = {}
        if not self.cache_enabled:
            # Explicitly disable caching at the request level for Mode A
            body["nexus"] = {"cache": {"enabled": False}}
        return body

    def _auth_headers(self) -> dict[str, str]:
        headers = super()._auth_headers()
        if self.virtual_key:
            headers["x-nexus-virtual-key"] = self.virtual_key
        return headers

    @classmethod
    def from_config(cls, config: GatewayFullConfig, model: str = "gpt-4o") -> "NexusAdapter":
        return cls(
            base_url=config.gateway.base_url,
            api_key=config.gateway.api_key,
            model=model,
            stream=config.request.stream,
            cache_enabled=config.features.caching_enabled,
        )
