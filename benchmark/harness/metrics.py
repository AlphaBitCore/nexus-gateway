"""Per-request records and aggregate statistics."""
from __future__ import annotations

import math
from dataclasses import dataclass, field, asdict
from typing import Optional


@dataclass
class RequestRecord:
    """One row of raw benchmark data."""
    gateway: str
    scenario: str
    vu: int
    iteration: int
    prompt_id: str
    # Timing (seconds). ttft is None when no token/first byte was received.
    ttft: Optional[float] = None
    e2e: Optional[float] = None
    status_code: Optional[int] = None
    stream_broken: bool = False
    request_error: bool = False
    cache_hit: bool = False
    cache_detected: bool = False          # was a cache header present at all?
    # Optional classification used by the cache scenario (w03b).
    cache_class: str = ""                  # exact | prefix | random | ""
    completion_tokens: Optional[int] = None
    error_message: str = ""

    def to_row(self) -> dict:
        return asdict(self)


def _percentile(values: list[float], q: float) -> Optional[float]:
    """Nearest-rank percentile (q in [0, 100]). None for empty input."""
    if not values:
        return None
    s = sorted(values)
    if len(s) == 1:
        return s[0]
    rank = math.ceil(q / 100.0 * len(s))
    rank = min(max(rank, 1), len(s))
    return s[rank - 1]


def _rate(numerator: int, denominator: int) -> float:
    return (100.0 * numerator / denominator) if denominator else 0.0


def aggregate(records: list[RequestRecord], duration_s: float) -> dict:
    """Compute aggregate stats over a list of records.

    Latency percentiles are computed over *successful* requests only
    (status 200, no request error), since failed requests have no meaningful
    TTFT/E2E. Rates are computed over all attempted requests.
    """
    total = len(records)
    successful = [
        r for r in records
        if not r.request_error and r.status_code == 200
    ]
    ttfts = [r.ttft for r in successful if r.ttft is not None]
    e2es = [r.e2e for r in successful if r.e2e is not None]

    http_failures = sum(
        1 for r in records
        if r.status_code is not None and r.status_code >= 400
    )
    request_errors = sum(1 for r in records if r.request_error)
    stream_breaks = sum(1 for r in records if r.stream_broken)
    cache_detected = [r for r in records if r.cache_detected]
    cache_hits = sum(1 for r in cache_detected if r.cache_hit)

    return {
        "total_requests": total,
        "successful_requests": len(successful),
        "duration_s": round(duration_s, 3),
        "rps": round(total / duration_s, 3) if duration_s > 0 else None,
        "ttft_p50": _percentile(ttfts, 50),
        "ttft_p95": _percentile(ttfts, 95),
        "ttft_p99": _percentile(ttfts, 99),
        "e2e_p50": _percentile(e2es, 50),
        "e2e_p95": _percentile(e2es, 95),
        "e2e_p99": _percentile(e2es, 99),
        "http_failure_rate_pct": round(_rate(http_failures, total), 3),
        "request_error_rate_pct": round(_rate(request_errors, total), 3),
        "stream_interruption_rate_pct": round(_rate(stream_breaks, total), 3),
        # Cache hit rate is over requests where a cache header was actually
        # observed; None if the gateway never reported cache status.
        "cache_hit_rate_pct": (
            round(_rate(cache_hits, len(cache_detected)), 3)
            if cache_detected else None
        ),
        "cache_observed_requests": len(cache_detected),
    }


def aggregate_by_cache_class(records: list[RequestRecord]) -> dict:
    """Cache hit rate + TTFT (hit vs miss) per cache_class — for scenario w03b."""
    out: dict = {}
    classes = sorted({r.cache_class for r in records if r.cache_class})
    for cls in classes:
        rows = [r for r in records if r.cache_class == cls]
        observed = [r for r in rows if r.cache_detected]
        hits = [r for r in observed if r.cache_hit]
        misses = [r for r in observed if not r.cache_hit]
        hit_ttfts = [r.ttft for r in hits if r.ttft is not None]
        miss_ttfts = [r.ttft for r in misses if r.ttft is not None]
        out[cls] = {
            "requests": len(rows),
            "cache_observed": len(observed),
            "cache_hit_rate_pct": (
                round(_rate(len(hits), len(observed)), 3) if observed else None
            ),
            "ttft_hit_p50": _percentile(hit_ttfts, 50),
            "ttft_miss_p50": _percentile(miss_ttfts, 50),
        }
    return out
