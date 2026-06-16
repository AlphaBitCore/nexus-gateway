"""
Metrics collection for the v2 benchmark.

Tracks TTFT (time-to-first-token), E2E (total stream time), stream-broken rate,
cache hit rate, HTTP error categorization, and throughput.
All percentiles use numpy for accuracy.
"""
from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Optional

import numpy as np


@dataclass
class RequestRecord:
    """Raw data captured for a single request."""
    request_id: int
    prompt_index: int
    ttft_ms: Optional[float]          # None if stream never started or non-streaming
    e2e_ms: float                     # Wall time from send to last byte / error
    status_code: Optional[int]
    success: bool
    stream_broken: bool               # SSE connection dropped before [DONE]
    http_4xx: bool
    http_5xx: bool
    connection_timeout: bool
    stream_timeout: bool
    json_parse_error: bool
    error_msg: Optional[str]
    cache_hit: Optional[bool]         # None if gateway doesn't report cache state
    tokens_generated: Optional[int]
    is_warmup: bool = False


@dataclass
class ScenarioMetrics:
    """Aggregated metrics for one scenario + gateway run."""
    gateway_name: str
    scenario_id: str
    mode: str                          # "cache-disabled" | "cache-enabled"
    total_requests: int = 0
    successful: int = 0
    failed: int = 0
    http_4xx: int = 0
    http_5xx: int = 0
    stream_broken: int = 0
    connection_timeouts: int = 0
    stream_timeouts: int = 0
    json_errors: int = 0
    cache_hits: int = 0
    cache_misses: int = 0
    wall_time_seconds: float = 0.0

    # Raw lists for percentile computation (warmup excluded)
    _ttft_samples: list[float] = field(default_factory=list, repr=False)
    _e2e_samples: list[float] = field(default_factory=list, repr=False)
    _cache_hit_ttft: list[float] = field(default_factory=list, repr=False)
    _cache_miss_ttft: list[float] = field(default_factory=list, repr=False)

    def add(self, r: RequestRecord) -> None:
        if r.is_warmup:
            return
        self.total_requests += 1
        if r.success:
            self.successful += 1
            if r.ttft_ms is not None:
                self._ttft_samples.append(r.ttft_ms)
                if r.cache_hit is True:
                    self._cache_hit_ttft.append(r.ttft_ms)
                elif r.cache_hit is False:
                    self._cache_miss_ttft.append(r.ttft_ms)
            self._e2e_samples.append(r.e2e_ms)
        else:
            self.failed += 1
        if r.http_4xx:
            self.http_4xx += 1
        if r.http_5xx:
            self.http_5xx += 1
        if r.stream_broken:
            self.stream_broken += 1
        if r.connection_timeout:
            self.connection_timeouts += 1
        if r.stream_timeout:
            self.stream_timeouts += 1
        if r.json_parse_error:
            self.json_errors += 1
        if r.cache_hit is True:
            self.cache_hits += 1
        elif r.cache_hit is False:
            self.cache_misses += 1

    def _pct(self, samples: list[float], p: int) -> Optional[float]:
        if not samples:
            return None
        return float(np.percentile(samples, p))

    def _stddev(self, samples: list[float]) -> Optional[float]:
        if len(samples) < 2:
            return None
        return float(np.std(samples))

    @property
    def ttft_p50(self) -> Optional[float]: return self._pct(self._ttft_samples, 50)
    @property
    def ttft_p95(self) -> Optional[float]: return self._pct(self._ttft_samples, 95)
    @property
    def ttft_p99(self) -> Optional[float]: return self._pct(self._ttft_samples, 99)
    @property
    def ttft_avg(self) -> Optional[float]:
        return float(np.mean(self._ttft_samples)) if self._ttft_samples else None
    @property
    def ttft_stddev(self) -> Optional[float]: return self._stddev(self._ttft_samples)
    @property
    def e2e_p50(self) -> Optional[float]: return self._pct(self._e2e_samples, 50)
    @property
    def e2e_p95(self) -> Optional[float]: return self._pct(self._e2e_samples, 95)
    @property
    def e2e_p99(self) -> Optional[float]: return self._pct(self._e2e_samples, 99)
    @property
    def e2e_avg(self) -> Optional[float]:
        return float(np.mean(self._e2e_samples)) if self._e2e_samples else None
    @property
    def rps(self) -> float:
        return self.total_requests / self.wall_time_seconds if self.wall_time_seconds > 0 else 0.0
    @property
    def http_failure_rate(self) -> float:
        return (self.failed / self.total_requests * 100) if self.total_requests > 0 else 0.0
    @property
    def stream_broken_rate(self) -> float:
        return (self.stream_broken / self.total_requests * 100) if self.total_requests > 0 else 0.0
    @property
    def cache_hit_rate(self) -> Optional[float]:
        total_cacheable = self.cache_hits + self.cache_misses
        if total_cacheable == 0:
            return None
        return self.cache_hits / total_cacheable * 100
    @property
    def cache_hit_ttft_p95(self) -> Optional[float]: return self._pct(self._cache_hit_ttft, 95)
    @property
    def cache_miss_ttft_p95(self) -> Optional[float]: return self._pct(self._cache_miss_ttft, 95)
    @property
    def ttft_gain_p95(self) -> Optional[float]:
        miss = self.cache_miss_ttft_p95
        hit = self.cache_hit_ttft_p95
        if miss is not None and hit is not None:
            return miss - hit
        return None

    def to_dict(self) -> dict:
        def _r(v: Optional[float]) -> Optional[float]:
            return round(v, 2) if v is not None else None

        return {
            "gateway": self.gateway_name,
            "scenario": self.scenario_id,
            "mode": self.mode,
            "total_requests": self.total_requests,
            "successful": self.successful,
            "failed": self.failed,
            "http_4xx": self.http_4xx,
            "http_5xx": self.http_5xx,
            "stream_broken": self.stream_broken,
            "connection_timeouts": self.connection_timeouts,
            "stream_timeouts": self.stream_timeouts,
            "json_errors": self.json_errors,
            "ttft_avg_ms": _r(self.ttft_avg),
            "ttft_p50_ms": _r(self.ttft_p50),
            "ttft_p95_ms": _r(self.ttft_p95),
            "ttft_p99_ms": _r(self.ttft_p99),
            "ttft_stddev_ms": _r(self.ttft_stddev),
            "e2e_avg_ms": _r(self.e2e_avg),
            "e2e_p50_ms": _r(self.e2e_p50),
            "e2e_p95_ms": _r(self.e2e_p95),
            "e2e_p99_ms": _r(self.e2e_p99),
            "rps": round(self.rps, 3),
            "http_failure_rate_pct": round(self.http_failure_rate, 2),
            "stream_broken_rate_pct": round(self.stream_broken_rate, 2),
            "cache_hit_rate_pct": _r(self.cache_hit_rate),
            "cache_hit_ttft_p95_ms": _r(self.cache_hit_ttft_p95),
            "cache_miss_ttft_p95_ms": _r(self.cache_miss_ttft_p95),
            "ttft_gain_p95_ms": _r(self.ttft_gain_p95),
            "wall_time_seconds": round(self.wall_time_seconds, 3),
        }
