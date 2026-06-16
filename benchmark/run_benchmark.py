#!/usr/bin/env python3
"""
Nexus Gateway Benchmark Harness — entry point.

Usage:
    python run_benchmark.py --config config/nexus_smoke.yaml
    python run_benchmark.py --config config/nexus_concurrency.yaml
    python run_benchmark.py --config config/nexus_soak.yaml
"""
from src.cli import main

if __name__ == "__main__":
    main()
