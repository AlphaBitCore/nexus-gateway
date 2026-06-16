#!/usr/bin/env bash
# run_full_suite.sh — One-command full comparison suite runner
# Usage: ./run_full_suite.sh [output_dir]
set -euo pipefail

OUTPUT="${1:-./results/run-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$OUTPUT"

cd "$(dirname "$0")"

echo "=========================================="
echo " Nexus Gateway Benchmark v2 — Full Suite"
echo " Output: $OUTPUT"
echo " Mode: cache-disabled (fair comparison)"
echo "=========================================="
echo ""

# 1. Pre-flight parity check
echo "[1/4] Pre-flight config parity validation..."
python cli.py validate-config --mode cache-disabled --gateways nexus,litellm,bifrost

# 2. Run comparison suite (S-01, S-02, S-03, S-04, S-06)
echo ""
echo "[2/4] Running comparison suite (S-01 through S-06, cache DISABLED)..."
python cli.py run-suite \
  --mode cache-disabled \
  --gateways nexus,litellm,bifrost \
  --scenarios s01,s02,s03,s04,s06 \
  --output "$OUTPUT"

# 3. Nexus-only cache feature test
echo ""
echo "[3/4] Running Nexus cache feature test (S-08, cache ENABLED, Nexus only)..."
python cli.py run \
  --scenario s08 \
  --gateway nexus \
  --mode cache-enabled \
  --output "$OUTPUT/cache-feature"

# 4. Generate final markdown report
echo ""
echo "[4/4] Generating final markdown report..."
python cli.py report \
  --results-dir "$OUTPUT" \
  --format markdown

echo ""
echo "=========================================="
echo " Suite complete!"
echo " Results: $OUTPUT/"
echo "=========================================="
