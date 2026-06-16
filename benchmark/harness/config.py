"""Gateway configuration loading.

A config file captures everything needed to talk to one gateway. The API key
itself never lives in the file; the file names an environment variable
(``api_key_env``) and the harness resolves it at load time.
"""
from __future__ import annotations

import os
from dataclasses import dataclass, asdict
from pathlib import Path
from typing import Optional

import yaml

CONFIGS_DIR = Path(__file__).resolve().parent.parent / "configs"

# Recognised provider prefixes, stripped when comparing models across gateways
# (Bifrost requires ``openai/gpt-4o-mini`` while LiteLLM/Nexus use bare ids).
_PROVIDER_PREFIXES = ("openai/", "anthropic/", "gemini/", "azure/", "bedrock/")

_REQUIRED_FIELDS = (
    "gateway_name",
    "version",
    "base_url",
    "api_key_env",
    "model",
    "provider",
    "cache_mode",
    "request_timeout",
    "max_tokens",
    "stream",
)


@dataclass
class GatewayConfig:
    gateway_name: str
    version: str
    base_url: str
    api_key_env: str
    model: str
    provider: str
    cache_mode: str          # "enabled" | "disabled"
    request_timeout: float
    max_tokens: int
    stream: bool
    # Resolved at load time, never serialised back out.
    api_key: Optional[str] = None

    @property
    def chat_completions_url(self) -> str:
        return self.base_url.rstrip("/") + "/v1/chat/completions"

    @property
    def normalized_model(self) -> str:
        """Model id with any provider prefix removed, for parity comparison."""
        m = self.model
        for p in _PROVIDER_PREFIXES:
            if m.startswith(p):
                return m[len(p):]
        return m

    def to_public_dict(self) -> dict:
        """Config as a dict with the secret api_key redacted (for metadata)."""
        d = asdict(self)
        d.pop("api_key", None)
        d["api_key_present"] = bool(self.api_key)
        return d


def config_path_for(gateway: str) -> Path:
    return CONFIGS_DIR / f"{gateway}.yaml"


def load_config(gateway: str, *, require_key: bool = False) -> GatewayConfig:
    """Load and validate a gateway config, resolving its API key from env.

    ``require_key=False`` lets dry-runs / preflight work without secrets set;
    actual request paths pass ``require_key=True``.
    """
    path = config_path_for(gateway)
    if not path.exists():
        raise FileNotFoundError(f"No config for gateway '{gateway}' at {path}")

    with path.open() as f:
        raw = yaml.safe_load(f) or {}

    missing = [k for k in _REQUIRED_FIELDS if k not in raw]
    if missing:
        raise ValueError(f"{path.name} missing required fields: {', '.join(missing)}")

    if raw["cache_mode"] not in ("enabled", "disabled"):
        raise ValueError(
            f"{path.name}: cache_mode must be 'enabled' or 'disabled', got {raw['cache_mode']!r}"
        )

    api_key = os.environ.get(raw["api_key_env"])
    if require_key and not api_key:
        raise EnvironmentError(
            f"Env var {raw['api_key_env']} (api key for gateway "
            f"'{raw['gateway_name']}') is not set."
        )

    return GatewayConfig(
        gateway_name=raw["gateway_name"],
        version=str(raw["version"]),
        base_url=raw["base_url"],
        api_key_env=raw["api_key_env"],
        model=raw["model"],
        provider=raw["provider"],
        cache_mode=raw["cache_mode"],
        request_timeout=float(raw["request_timeout"]),
        max_tokens=int(raw["max_tokens"]),
        stream=bool(raw["stream"]),
        api_key=api_key,
    )


def all_gateways() -> list[str]:
    """Gateway names for which a config file exists."""
    return sorted(p.stem for p in CONFIGS_DIR.glob("*.yaml"))
