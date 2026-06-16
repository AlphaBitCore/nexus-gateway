"""
Configuration models for the benchmark harness.

Uses Pydantic v2 for validation and type safety. Config is loaded from a YAML
or JSON file and strongly-typed before any work begins — fail fast on bad config.

Design note: auth secrets are read from environment variables (prefix the value
with $ in the YAML, e.g. api_key: "$NEXUS_API_KEY"). This keeps secrets out of
committed YAML files, matching Nexus repo policy.
"""

from __future__ import annotations

import hashlib
import json
import os
import platform
import subprocess
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Any, Optional

import yaml
from pydantic import BaseModel, Field, field_validator


class TestProfile(str, Enum):
    SMOKE = "smoke"
    LATENCY = "latency"
    CONCURRENCY = "concurrency"
    THROUGHPUT = "throughput"
    SOAK = "soak"
    CONSISTENCY = "consistency"


class RetryConfig(BaseModel):
    enabled: bool = True
    max_attempts: int = 3
    backoff_seconds: float = 0.5


class CachingConfig(BaseModel):
    enabled: bool = False
    # Document caching assumptions so cross-run comparisons are fair.
    # If one gateway run has caching on and another off, results are not comparable.
    note: str = ""


class AuthConfig(BaseModel):
    api_key: Optional[str] = None
    # Arbitrary extra headers (e.g. x-nexus-virtual-key). Values starting with $
    # are resolved from environment variables at runtime.
    headers: dict[str, str] = Field(default_factory=dict)

    def resolved_headers(self) -> dict[str, str]:
        """Resolve environment variable references in header values."""
        resolved: dict[str, str] = {}
        for k, v in self.headers.items():
            resolved[k] = os.environ.get(v[1:], v) if v.startswith("$") else v
        if self.api_key:
            key = self.api_key
            if key.startswith("$"):
                key = os.environ.get(key[1:], key)
            resolved["Authorization"] = f"Bearer {key}"
        return resolved


class PayloadTemplate(BaseModel):
    model: str
    max_tokens: int = 256
    temperature: float = 0.0
    stream: bool = False
    # Arbitrary extra fields forwarded verbatim into the request body.
    # Use this for gateway-specific extensions without changing the adapter.
    extra: dict[str, Any] = Field(default_factory=dict)


class BenchmarkConfig(BaseModel):
    gateway_name: str
    base_url: str
    endpoint: str = "/v1/chat/completions"
    auth: AuthConfig = Field(default_factory=AuthConfig)
    payload: PayloadTemplate

    profile: TestProfile = TestProfile.LATENCY

    # For CONCURRENCY profile: one benchmark pass per level.
    # Other profiles use concurrency_levels[0] only.
    concurrency_levels: list[int] = Field(default_factory=lambda: [1, 5, 10])

    total_requests: int = 100
    timeout_seconds: float = 30.0
    warmup_requests: int = 5

    retry: RetryConfig = Field(default_factory=RetryConfig)
    caching: CachingConfig = Field(default_factory=CachingConfig)

    # Either supply a JSONL prompts file or a single static prompt.
    prompts_file: Optional[str] = None
    static_prompt: str = "Say hello in one sentence."

    output_dir: str = "results"

    # Abort (with warning) if the live failure rate exceeds this percentage.
    failure_threshold_pct: float = 25.0

    @field_validator("concurrency_levels")
    @classmethod
    def must_be_positive(cls, v: list[int]) -> list[int]:
        if not all(c > 0 for c in v):
            raise ValueError("All concurrency levels must be > 0")
        return v

    def fingerprint(self) -> str:
        """Stable 12-char hash of config used to tag result artifacts for comparison."""
        data = self.model_dump(exclude={"output_dir"})
        return hashlib.sha256(
            json.dumps(data, sort_keys=True, default=str).encode()
        ).hexdigest()[:12]


def load_config(path: str | Path) -> BenchmarkConfig:
    """Load and validate a YAML or JSON config file, raising on first error."""
    path = Path(path)
    if not path.exists():
        raise FileNotFoundError(f"Config not found: {path}")

    with open(path) as f:
        raw = yaml.safe_load(f) if path.suffix in (".yaml", ".yml") else json.load(f)

    return BenchmarkConfig(**raw)


@dataclass
class EnvironmentInfo:
    """Snapshot of host environment for reproducibility metadata."""

    gateway_name: str
    host: str = field(default_factory=platform.node)
    os: str = field(default_factory=platform.platform)
    cpu: str = field(default_factory=platform.processor)
    python_version: str = field(default_factory=platform.python_version)
    cpu_count: int = field(default_factory=lambda: os.cpu_count() or 0)
    git_commit: str = field(default_factory=_get_git_commit)
    timestamp: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "gateway_name": self.gateway_name,
            "host": self.host,
            "os": self.os,
            "cpu": self.cpu,
            "python_version": self.python_version,
            "cpu_count": self.cpu_count,
            "git_commit": self.git_commit,
            "timestamp": self.timestamp,
        }


def _get_git_commit() -> str:
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"],
            stderr=subprocess.DEVNULL,
            text=True,
        ).strip()
    except Exception:
        return "unknown"
