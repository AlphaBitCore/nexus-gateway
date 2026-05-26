#!/usr/bin/env python3
"""
Count non-comment source lines per top-level directory under packages/ using
cloc (https://github.com/AlDanial/cloc). cloc's "code" metric excludes blank
lines and comment lines for each supported language.

By default only "programming / UI source" languages are summed (see
DEFAULT_COUNTED_LANGS). Use --all-languages to use cloc's SUM.code across every
detected language in each package (includes JSON, YAML, HTML, etc.).

Excludes directory trees: node_modules, dist, build, coverage, .git
Excludes Go debug binaries under cmd/: filenames matching __debug_bin*
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

# Application-oriented whitelist (cloc language names).
DEFAULT_COUNTED_LANGS = frozenset(
    {
        "Go",
        "TypeScript",
        "CSS",
        "JavaScript",
        "Swift",
        "C#",
        "D",
        "XAML",
        "WiX source",
        "WiX include",
        "MSBuild script",
        "Bourne Shell",
        "PowerShell",
    }
)


def find_cloc() -> str | None:
    env = os.environ.get("CLOC")
    if env and Path(env).is_file():
        return env
    w = shutil.which("cloc")
    if w:
        return w
    for p in ("/opt/homebrew/bin/cloc", "/usr/local/bin/cloc"):
        if Path(p).is_file():
            return p
    return None


def run_cloc(cloc: str, pkg: Path) -> dict:
    cmd = [
        cloc,
        str(pkg),
        "--exclude-dir=node_modules,dist,build,coverage,.git",
        "--not-match-f=__debug_bin",
        "--quiet",
        "--json",
    ]
    out = subprocess.check_output(cmd, text=True, stderr=subprocess.DEVNULL)
    return json.loads(out)


def sum_metrics(
    data: dict, counted_langs: frozenset[str] | None
) -> tuple[int, int, int]:
    """Returns (code, comment, blank)."""
    if counted_langs is None:
        s = data.get("SUM") or {}
        return int(s.get("code", 0)), int(s.get("comment", 0)), int(s.get("blank", 0))
    code = comment = blank = 0
    for key, val in data.items():
        if key in ("header", "SUM") or not isinstance(val, dict):
            continue
        if key not in counted_langs:
            continue
        code += int(val.get("code", 0))
        comment += int(val.get("comment", 0))
        blank += int(val.get("blank", 0))
    return code, comment, blank


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument(
        "--all-languages",
        action="store_true",
        help="Sum cloc SUM.code (includes JSON, YAML, Markdown, HTML, etc.)",
    )
    parser.add_argument(
        "--show-comments",
        action="store_true",
        help="Also print comment and blank line totals (cloc definitions)",
    )
    args = parser.parse_args()

    cloc = find_cloc()
    if not cloc:
        print(
            "cloc not found. Install cloc (e.g. `brew install cloc`) or set CLOC=/path/to/cloc",
            file=sys.stderr,
        )
        return 1

    root = Path(__file__).resolve().parent.parent
    packages = sorted(p for p in (root / "packages").iterdir() if p.is_dir())
    counted = None if args.all_languages else DEFAULT_COUNTED_LANGS

    rows: list[tuple[str, int, int, int]] = []
    tot_code = tot_comment = tot_blank = 0
    for pkg in packages:
        data = run_cloc(cloc, pkg)
        c, co, b = sum_metrics(data, counted)
        rows.append((pkg.name, c, co, b))
        tot_code += c
        tot_comment += co
        tot_blank += b

    w_pkg = max(len("Package"), max(len(r[0]) for r in rows))
    if args.show_comments:
        hdr = f"{'Package':<{w_pkg}}  {'Code':>10}  {'Comment':>10}  {'Blank':>10}"
        sep = f"{'-' * w_pkg}  {'-' * 10}  {'-' * 10}  {'-' * 10}"
        print(hdr)
        print(sep)
        for name, c, co, b in rows:
            print(f"{name:<{w_pkg}}  {c:>10}  {co:>10}  {b:>10}")
        print(sep)
        print(f"{'TOTAL':<{w_pkg}}  {tot_code:>10}  {tot_comment:>10}  {tot_blank:>10}")
    else:
        hdr = f"{'Package':<{w_pkg}}  {'Code lines':>12}"
        sep = f"{'-' * w_pkg}  {'-' * 12}"
        print(hdr)
        print(sep)
        for name, c, _, _ in rows:
            print(f"{name:<{w_pkg}}  {c:>12}")
        print(sep)
        print(f"{'TOTAL':<{w_pkg}}  {tot_code:>12}")

    mode = "all languages (SUM.code)" if args.all_languages else "whitelisted languages only"
    print()
    print(f"Metric: cloc `code` lines ({mode}); comments and blank lines excluded from Code.")
    if not args.all_languages:
        print(f"Counted languages: {', '.join(sorted(DEFAULT_COUNTED_LANGS))}.")
        print("Tip: use --all-languages to include JSON, YAML, Markdown, HTML, Dockerfile, etc.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
