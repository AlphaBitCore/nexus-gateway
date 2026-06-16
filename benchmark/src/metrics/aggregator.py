"""
Metrics aggregation.

Collects raw per-request results and computes summary statistics.
Pure Python — no numpy required. Percentile uses linear interpolation
(the same algorithm as numpy.percentile with interpolation='linear').
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Optional


@dataclass
class RequestResult:
    """Raw data captured for a single HTTP request attempt."""

    prompt_index: int
    latency_ms: float
    status_code: Optional[int]
    success: bool
    timed_out: bool
    exception: Optional[str]
    validation_ok: bool
    validation_reason: str
    content_length: int  # character count of the response text


@dataclass
class MetricsSummary:
    """Aggregated statistics for one benchmark pass."""

    concurrency: int
    total_requests: int
    successful: int
    failed: int
    timed_out: int
    non_200: int
    exceptions: int
    validation_failures: int
    avg_latency_ms: float
    p50_ms: float
    p95_ms: float
    p99_ms: float
    min_ms: float
    max_ms: float
    rps: float
    wall_time_seconds: float
    avg_content_length: float

    def to_dict(self) -> dict:
        return {
            "concurrency": self.concurrency,
            "total_requests": self.total_requests,
            "successful": self.successful,
            "failed": self.failed,
            "timed_out": self.timed_out,
            "non_200": self.non_200,
            "exceptions": self.exceptions,
            "validation_failures": self.validation_failures,
            "avg_latency_ms": round(self.avg_latency_ms, 2),
            "p50_ms": round(self.p50_ms, 2),
            "p95_ms": round(self.p95_ms, 2),
            "p99_ms": round(self.p99_ms, 2),
            "min_ms": round(self.min_ms, 2),
            "max_ms": round(self.max_ms, 2),
            "rps": round(self.rps, 3),
            "wall_time_seconds": round(self.wall_time_seconds, 3),
            "avg_content_length": round(self.avg_content_length, 1),
        }


class MetricsAggregator:
    """
    Accumulates RequestResult objects then produces a MetricsSummary.

    Latency percentiles are computed only over successful requests so that
    timeout/error latencies (which represent a timeout ceiling, not real
    service latency) don't skew p95/p99.
    """

    def __init__(self, concurrency: int) -> None:
        self.concurrency = concurrency
        self._results: list[RequestResult] = []

    def add(self, result: RequestResult) -> None:
        self._results.append(result)

    def failure_rate(self) -> float:
        if not self._results:
            return 0.0
        failed = sum(1 for r in self._results if not r.success)
        return (failed / len(self._results)) * 100.0

    def summarize(self, wall_time_seconds: float) -> MetricsSummary:
        results = self._results
        n = len(results)
        if n == 0:
            raise ValueError("No results collected — cannot summarize")

        successful = [r for r in results if r.success]
        latencies = sorted(r.latency_ms for r in successful)
        if not latencies:
            latencies = [0.0]

        return MetricsSummary(
            concurrency=self.concurrency,
            total_requests=n,
            successful=len(successful),
            failed=sum(1 for r in results if not r.success),
            timed_out=sum(1 for r in results if r.timed_out),
            non_200=sum(
                1 for r in results if r.status_code is not None and r.status_code != 200
            ),
            exceptions=sum(1 for r in results if r.exception is not None),
            validation_failures=sum(1 for r in results if not r.validation_ok),
            avg_latency_ms=_mean(latencies),
            p50_ms=_percentile(latencies, 50),
            p95_ms=_percentile(latencies, 95),
            p99_ms=_percentile(latencies, 99),
            min_ms=latencies[0],
            max_ms=latencies[-1],
            rps=n / wall_time_seconds if wall_time_seconds > 0 else 0.0,
            wall_time_seconds=wall_time_seconds,
            avg_content_length=_mean(
                [r.content_length for r in successful]
            ) if successful else 0.0,
        )


def _mean(values: list[float]) -> float:
    return sum(values) / len(values) if values else 0.0


def _percentile(sorted_values: list[float], p: int) -> float:
    """Linear interpolation percentile — matches numpy default behavior."""
    n = len(sorted_values)
    if n == 0:
        return 0.0
    if n == 1:
        return sorted_values[0]
    rank = (p / 100.0) * (n - 1)
    lo = math.floor(rank)
    hi = math.ceil(rank)
    if lo == hi:
        return sorted_values[lo]
    return sorted_values[lo] + (rank - lo) * (sorted_values[hi] - sorted_values[lo])
