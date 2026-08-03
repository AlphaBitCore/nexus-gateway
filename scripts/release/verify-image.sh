#!/usr/bin/env bash
# verify-image.sh — release gate for a Nexus buildbase image.
#
# Measures the instruction-set baseline of libhs.a, the Vectorscan archive that
# every service binary links. FAT_RUNTIME=OFF fixes libhs's SIMD instruction set
# at compile time, so a "baseline" build that actually contains AVX2 would SIGILL
# on pre-AVX2 CPUs. The baseline claim has to be measured, not assumed.
#
# Why the archive and not the linked service binary: Go's standard library ships
# hand-written SIMD assembly for its crypto routines, compiled into every amd64
# binary and selected at runtime through CPUID checks. That code is safe on old
# CPUs precisely because it is runtime-gated, but objdump cannot tell it apart
# from libhs's compile-time-fixed AVX2. Scanning the linked binary would reject
# correct baseline images while proving nothing about the case that matters.
# libhs.a is where the instruction-set decision is actually made, and it is the
# artifact the design spike validated.
#
# This gate covers portability only. Its companion is the Vectorscan firing
# self-test, which runs inside the service build and proves the linked engine
# actually scans.
#
# Usage: verify-image.sh <buildbase-image-ref> <baseline|avx2>
set -euo pipefail

IMAGE="${1:?usage: verify-image.sh <buildbase-image-ref> <baseline|avx2>}"
EXPECT="${2:?usage: verify-image.sh <buildbase-image-ref> <baseline|avx2>}"

# A ref starting with "-" (a typo'd flag, an empty/glob-expansion accident,
# etc.) is parsed by `docker run` below as an option rather than the image
# argument, producing a confusing "unknown flag" error from docker instead
# of a clear usage error from this script. Reject that shape up front.
case "$IMAGE" in
  -*) echo "ERROR: image ref '$IMAGE' starts with '-' and would be parsed as a docker flag, not an image reference." >&2; exit 2 ;;
esac

case "$EXPECT" in
  baseline|avx2) ;;
  *) echo "ERROR: second argument must be 'baseline' or 'avx2', got '$EXPECT'" >&2; exit 2 ;;
esac

echo "==> [verify] $IMAGE (expecting $EXPECT)"

# Everything runs inside the buildbase: it holds both libhs.a and the objdump
# that reads it, so there is nothing to copy out and no failure mode where a
# missing path is mistaken for a clean result.
docker run --rm --entrypoint /bin/bash "$IMAGE" -c '
set -euo pipefail
EXPECT="'"$EXPECT"'"

archive="$(ls /usr/local/lib/libhs.a /usr/local/lib64/libhs.a 2>/dev/null | head -1 || true)"
if [ -z "$archive" ]; then
  echo "ERROR: libhs.a not found under /usr/local/lib or /usr/local/lib64" >&2
  exit 1
fi

# Capture objdump separately from the counting pipeline. Folding them together
# would let an objdump failure surface as a count of zero, which reads as a
# clean baseline — the exact "assumed, not measured" outcome this gate exists
# to prevent.
if ! disasm="$(objdump -d "$archive" 2>&1)"; then
  echo "ERROR: objdump failed to disassemble $archive" >&2
  printf "%s\n" "$disasm" | tail -5 >&2
  exit 1
fi

arch="$(dpkg --print-architecture)"

# arm64 has exactly one configuration, armv8-a. Reinterpreting an avx2 request
# as a baseline check would return "OK" for a question that was never asked —
# the same shape of failure as a gate that passes without measuring anything.
if [ "$arch" = "arm64" ] && [ "$EXPECT" = "avx2" ]; then
  # No apostrophes: this whole block is a single-quoted argument to `docker run
  # -c` on the host, so a quote here closes and reopens that string, and anything
  # between two of them would be expanded by the HOST shell before docker sees it.
  echo "ERROR: arm64 has no avx2 variant; only the baseline configuration is valid on this architecture." >&2
  exit 2
fi

case "$arch" in
  amd64)
    # 256-bit register operands are the structural signal: every AVX2
    # instruction that widens work to 256 bits names a %ymm register, so this
    # catches the family without depending on an enumeration staying complete
    # across compiler and Vectorscan versions. The named mnemonics that follow
    # are the ones the Vectorscan byte-scanning matchers (Shufti, Truffle, Teddy)
    # actually emit, kept as a readable second signal.
    #
    # vzeroupper is deliberately absent: it is VEX-encoded but not AVX2-specific
    # and can appear in ABI transition sequences, so it would be the one pattern
    # here capable of failing a correct baseline archive.
    pattern="%ymm[0-9]+|\b(vpbroadcast|vpermd|vperm2i128|vinserti128|vextracti128|vpgather|vpcmpeqb|vpmovmskb|vpblendvb|vpalignr|vptest|vpcmpgt|vpminub|vpmaxub|vpsll|vpsrl|vmovdqu|vpxor|vpand|vpor|vpaddb|vpshufb)[a-z0-9]*\b"
    family="AVX2"
    ;;
  arm64)
    # SVE/SVE2 mnemonics and Z-register operands. armv8-a NEON is the baseline
    # every arm64 v8-A CPU has, so only SVE would narrow portability.
    pattern="\b(ptrue|whilelo|whilelt|z[0-9]+\.[bhsd])\b"
    family="SVE"
    ;;
  *)
    echo "ERROR: unsupported architecture $arch" >&2
    exit 1
    ;;
esac

# `grep -c` exits 1 on a zero count. Under `set -e` + `pipefail` that would abort
# here at exactly the moment a zero count is the PASS condition, so this guard is
# load-bearing. It is safe to swallow now only because objdump already succeeded
# above.
count="$(printf "%s\n" "$disasm" | grep -cE "$pattern" || true)"

# That same guard also swallows grep exiting 2 — an invalid regex after a future
# edit to $pattern, or grep dying on the multi-MB disassembly — which leaves
# $count empty instead of a number. An empty $count does not fail the checks
# below: [ "" -ne 0 ] is a bash usage error (status 2), not a false condition,
# and a non-zero status in an `if` condition is exempt from `set -e`, so the
# script would fall through to a PASS line for a measurement that never
# happened. Anything that is not a count fails closed here.
case "$count" in
  ""|*[!0-9]*)
    echo "ERROR: instruction count came back as \"$count\", not a number — the pattern or the disassembly is wrong, so nothing was measured." >&2
    exit 1
    ;;
esac

if [ "$arch" = "arm64" ]; then
  # Only baseline reaches here; the avx2 request was rejected above. The single
  # meaningful check is that SVE did not creep in.
  if [ "$count" -ne 0 ]; then
    echo "ERROR: $archive contains $count $family instructions; the armv8-a baseline must not require SVE." >&2
    exit 1
  fi
  echo "    $(basename "$archive") on $arch: $count $family instructions (armv8-a baseline OK)"
  exit 0
fi

case "$EXPECT" in
  baseline)
    if [ "$count" -ne 0 ]; then
      echo "ERROR: $archive contains $count $family instructions but was built as a baseline image." >&2
      echo "       A baseline image must run on pre-AVX2 CPUs; publishing this would SIGILL." >&2
      exit 1
    fi
    echo "    $(basename "$archive") on $arch: $count $family instructions (baseline OK)"
    ;;
  avx2)
    if [ "$count" -eq 0 ]; then
      echo "ERROR: $archive contains no $family instructions but was built as the avx2 variant." >&2
      echo "       The -avx2 tag would be identical to baseline; the AVX2 build flag did not apply." >&2
      exit 1
    fi
    echo "    $(basename "$archive") on $arch: $count $family instructions (avx2 OK)"
    ;;
esac
'

echo "==> [verify] done"
