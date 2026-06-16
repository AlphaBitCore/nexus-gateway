"""
Pydantic v2 config and result models for the benchmark framework.
All gateway configs are validated here before any work begins — fail fast.

Env loading: if benchmark/v2/.env.local exists it is loaded automatically
before any config model is instantiated.  This is the standard local dev
path — no manual `source .env.local` required.  Export-level env vars always
take precedence over values in the file (python-dotenv non-override semantics).
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Optional

import yaml
from dotenv import load_dotenv
from pydantic import BaseModel, Field, field_validator, model_validator

# Auto-load benchmark/v2/.env.local when present.  override=False means
# already-exported shell vars win (safe for CI / prod invocations).
_env_local = Path(__file__).parent.parent / ".env.local"
if _env_local.exists():
    load_dotenv(_env_local, override=False)


class GatewayConfig(BaseModel):
    name: str
    version: str = "unknown"
    base_url: str
    api_key: Optional[str] = None

    @field_validator("base_url")
    @classmethod
    def resolve_env(cls, v: str) -> str:
        if v.startswith("${") and v.endswith("}"):
            inner = v[2:-1]
            default = None
            if ":-" in inner:
                key, default = inner.split(":-", 1)
            else:
                key = inner
            return os.environ.get(key, default or v)
        return v

    @field_validator("api_key")
    @classmethod
    def resolve_api_key(cls, v: Optional[str]) -> Optional[str]:
        if not v:
            return v
        # Handle ${VAR} and ${VAR:-default} forms (used in gateway YAML files).
        if v.startswith("${") and v.endswith("}"):
            inner = v[2:-1]
            if ":-" in inner:
                key, default = inner.split(":-", 1)
            else:
                key, default = inner, v
            return os.environ.get(key, default)
        # Handle bare $VAR form (legacy benchmark/v1 style).
        if v.startswith("$"):
            return os.environ.get(v[1:], v)
        return v


class RequestConfig(BaseModel):
    timeout_seconds: float = 60.0
    max_retries: int = 0
    stream: bool = True


class FeaturesConfig(BaseModel):
    caching_enabled: bool = False


class BenchmarkRunConfig(BaseModel):
    warmup_duration_seconds: int = 30
    virtual_users: int = 20
    test_duration_seconds: int = 300


class AdminConfig(BaseModel):
    admin_base_url: str = ""
    admin_api_key: str = ""

    @field_validator("admin_base_url", "admin_api_key")
    @classmethod
    def resolve_env(cls, v: str) -> str:
        if v.startswith("${") and v.endswith("}"):
            inner = v[2:-1]
            default = None
            if ":-" in inner:
                key, default = inner.split(":-", 1)
            else:
                key = inner
            return os.environ.get(key, default or v)
        return v


class GatewayFullConfig(BaseModel):
    gateway: GatewayConfig
    request: RequestConfig = Field(default_factory=RequestConfig)
    features: FeaturesConfig = Field(default_factory=FeaturesConfig)
    benchmark: BenchmarkRunConfig = Field(default_factory=BenchmarkRunConfig)
    admin: AdminConfig = Field(default_factory=AdminConfig)

    def fingerprint(self) -> str:
        data = self.model_dump()
        return hashlib.sha256(
            json.dumps(data, sort_keys=True, default=str).encode()
        ).hexdigest()[:12]

    @classmethod
    def from_yaml(cls, path: str) -> "GatewayFullConfig":
        with open(path) as f:
            raw = yaml.safe_load(f)
        return cls(**raw)
