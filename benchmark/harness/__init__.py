"""Nexus Gateway benchmark harness.

A small, dependency-light async harness for comparing OpenAI-compatible
gateways (Nexus, Bifrost, LiteLLM) on TTFT, end-to-end latency, error rates,
stream integrity, and cache behaviour.
"""

__all__ = [
    "config",
    "datasets",
    "metrics",
    "client",
    "scenarios",
    "reporting",
    "meta",
]
