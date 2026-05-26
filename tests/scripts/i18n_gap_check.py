#!/usr/bin/env python3
"""
i18n gap checker for Nexus Gateway frontend bundles.

Bundles covered:
  - control-plane-ui  (packages/control-plane-ui)            ns: pages | common | nav | shared
  - agent-ui          (packages/agent/ui/frontend)           ns: dashboard | shared
The `shared` namespace lives in packages/ui-shared and is consumed by both.

Per bundle the script finds:
  1. Keys used in source but missing from EN  (UI shows raw key strings).
  2. EN keys missing from ES or ZH            (translation gaps).
  3. Orphan keys in ES/ZH that are not in EN  (stale translations).
  4. EN keys not used in source               (potentially stale).
  5. Dynamic t() calls (template literals)    (manual review).
  6. Hardcoded English strings in `.tsx`      (JSX text + user-facing
     attributes outside of t() / <Trans>).

Bare `t('key')` calls are resolved against the file's
`useTranslation('ns')` namespace, falling back to the bundle's defaultNS.

Output: Markdown report written to /tmp/i18n-gap-<UTC-timestamp>.md
        and printed to stdout.

Usage:
    python3 tests/scripts/i18n_gap_check.py [--repo-root <path>]
"""

import argparse
import json
import re
import sys
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

LOCALES = ["en", "es", "zh"]


@dataclass
class Bundle:
    """One frontend application + its namespace -> locale-dir mapping."""
    name: str
    src_dirs: list[Path]
    default_ns: str
    # ns -> directory that contains <locale>/<ns>.json
    namespace_locales: dict[str, Path]
    # Optional: path that mirrors src/i18n/locales for HTTP backend fetches.
    public_locales_sync: Path | None = None


def build_bundles(repo_root: Path) -> list[Bundle]:
    cp_ui = repo_root / "packages" / "control-plane-ui"
    agent_ui = repo_root / "packages" / "agent" / "ui" / "frontend"
    shared = repo_root / "packages" / "ui-shared"

    return [
        Bundle(
            name="control-plane-ui",
            src_dirs=[cp_ui / "src"],
            default_ns="common",
            namespace_locales={
                "pages": cp_ui / "src" / "i18n" / "locales",
                "common": cp_ui / "src" / "i18n" / "locales",
                "nav": cp_ui / "src" / "i18n" / "locales",
                "shared": shared / "src" / "i18n",
            },
            public_locales_sync=cp_ui / "public" / "locales",
        ),
        Bundle(
            name="agent-ui",
            src_dirs=[agent_ui / "src"],
            default_ns="dashboard",
            namespace_locales={
                "dashboard": agent_ui / "src" / "i18n" / "locales",
                "shared": shared / "src" / "i18n",
            },
            public_locales_sync=None,
        ),
    ]


# ---------------------------------------------------------------------------
# Regexes
# ---------------------------------------------------------------------------

# Any static t() call: t('foo') / t("foo:bar.baz") / t("foo", { … })
_TCALL_RE = re.compile(r"""\bt\(\s*["']([^"'`]+)["']""")

# Dynamic t() calls (template literals) -- flagged, not resolved.
_DYNAMIC_RE = re.compile(r"""\bt\(\s*`([^`]+)`""")

# useTranslation('ns') / useTranslation(["ns1", "ns2"]).
_USE_TRANS_RE = re.compile(
    r"""useTranslation\(\s*(?:\[([^\]]+)\]|["']([a-z][\w-]*)["'])"""
)
_NS_LITERAL_RE = re.compile(r"""["']([a-z][\w-]*)["']""")

# JSX text content. Looks for `>TEXT<` on a single line, where TEXT
# contains at least one letter and at least one space (multi-word) or a
# sentence-ending punctuation. Excludes JS-expression markers ({} () []) so
# TypeScript type assertions like `as Promise<T>` don't get caught as text.
_JSX_TEXT_RE = re.compile(
    r""">\s*([A-Za-z][^<>{}()\[\]\n\r]{1,200}?)\s*<"""
)

# Allow-list of single-word JSX text that should still be flagged. These are
# common imperative-verb / action-label words that legitimately need t() even
# though they are 1 word. Anything else single-word is too noisy.
_SINGLE_WORD_ACTION_LABELS = frozenset({
    "Cancel", "Continue", "Confirm", "Submit", "Save", "Delete", "Remove",
    "Reload", "Refresh", "Retry", "Reset", "Close", "Dismiss", "Done",
    "Back", "Next", "Previous", "Skip", "Send", "Edit", "Update", "Create",
    "Add", "Apply", "Discard", "Approve", "Deny", "Enable", "Disable",
    "Pause", "Resume", "Start", "Stop", "Restart", "Quit", "Sign in",
    "Log in", "Log out", "Sign out", "Loading", "Saving", "Deleting",
})

# JSX attribute literals (single OR double quoted). Limited to a curated set
# of user-facing attribute names so we don't drown in className="...", id="...",
# data-testid="...", route="..." etc.
_USER_FACING_ATTRS = (
    "title",
    "placeholder",
    "aria-label",
    "aria-description",
    "aria-placeholder",
    "alt",
    "label",
    "description",
    "subtitle",
    "tooltip",
    "caption",
    "header",
    "heading",
    "subheading",
    "headerText",
    "footerText",
    "emptyText",
    "emptyMessage",
    "loadingText",
    "loadingMessage",
    "errorText",
    "errorMessage",
    "successText",
    "successMessage",
    "confirmText",
    "cancelText",
    "submitText",
    "summary",
    "helperText",
    "hint",
    "message",
    "noOptionsMessage",
)
_ATTR_RE = re.compile(
    r"""\b(""" + "|".join(re.escape(a) for a in _USER_FACING_ATTRS) + r""")\s*=\s*["']([^"'\n]{2,200})["']"""
)

# Heuristic filter for "looks like user-visible English text".
_HAS_LETTER = re.compile(r"[A-Za-z]")
_LOOKS_LIKE_CSS_TOKENS = re.compile(r"^[a-z0-9:_\-\s/\.]+$")
_LOOKS_LIKE_URL = re.compile(r"^(https?://|mailto:|tel:|#|/[A-Za-z0-9]|[A-Za-z0-9_]+://)")
_LOOKS_LIKE_PATH = re.compile(r"^[./]+[A-Za-z0-9._/-]+$")
_LOOKS_LIKE_TOKEN = re.compile(r"^[A-Z][A-Z0-9_-]+$")  # MACRO_NAME, ENV_VAR

# JS / TS operator substrings that indicate the captured text is actually
# code, not user-facing copy. If any of these appear, drop the match.
_CODE_OPERATORS = (
    "&&", "||", "??", "?.", "=>", "===", "!==", "==", "!=",
    "<=", ">=", "++", "--", "**", "::",
)
# Member-access pattern: a lowercase letter, then '.', then a lowercase
# letter (e.g. `r.primaryIp`). Real labels rarely look like this.
_MEMBER_ACCESS_RE = re.compile(r"[a-z]\.[a-z]")

# Files to skip for hardcoded-string scanning.
_HARDCODE_SKIP_DIR_NAMES = {
    "node_modules", "dist", "build", ".turbo", ".next", ".vite",
    "test", "tests", "__tests__", "__mocks__", "stories",
}
_HARDCODE_SKIP_FILE_SUFFIXES = (
    ".test.ts", ".test.tsx", ".spec.ts", ".spec.tsx",
    ".stories.ts", ".stories.tsx", ".d.ts",
)

# Files that are i18n infrastructure themselves -- their literal strings are
# locale labels / config, not UI copy.
_I18N_INFRA_PATH_PARTS = (
    "/i18n/",
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def flatten_json(obj, prefix: str = "") -> dict[str, str]:
    result: dict[str, str] = {}
    if not isinstance(obj, dict):
        return result
    for k, v in obj.items():
        full_key = f"{prefix}.{k}" if prefix else k
        if isinstance(v, dict):
            result.update(flatten_json(v, full_key))
        else:
            result[full_key] = str(v)
    return result


def load_locale(locales_dir: Path, locale: str, namespace: str) -> dict[str, str]:
    path = locales_dir / locale / f"{namespace}.json"
    if not path.exists():
        return {}
    try:
        with open(path, encoding="utf-8") as f:
            return flatten_json(json.load(f))
    except Exception as exc:
        print(f"WARN: failed to load {path}: {exc}", file=sys.stderr)
        return {}


def iter_source_files(src_dir: Path) -> list[Path]:
    out: list[Path] = []
    for path in src_dir.rglob("*"):
        if not path.is_file():
            continue
        if path.suffix not in (".ts", ".tsx"):
            continue
        parts = set(path.parts)
        if parts & _HARDCODE_SKIP_DIR_NAMES:
            continue
        if any(str(path).endswith(suf) for suf in _HARDCODE_SKIP_FILE_SUFFIXES):
            continue
        out.append(path)
    return sorted(out)


def file_useTranslation_namespaces(text: str) -> list[str]:
    """Return ordered list of namespaces referenced by useTranslation calls in this file."""
    namespaces: list[str] = []
    for m in _USE_TRANS_RE.finditer(text):
        array_body, single = m.group(1), m.group(2)
        if single:
            namespaces.append(single)
        elif array_body:
            for nsm in _NS_LITERAL_RE.finditer(array_body):
                namespaces.append(nsm.group(1))
    # Preserve order but unique.
    seen: set[str] = set()
    out: list[str] = []
    for ns in namespaces:
        if ns not in seen:
            seen.add(ns)
            out.append(ns)
    return out


@dataclass
class UsageRecord:
    """A key found in source. `candidate_ns` is the list of namespaces this
    key might resolve to (1 entry for explicit `ns:key`, 1+ for bare keys)."""
    key: str
    candidate_ns: tuple[str, ...]
    file: str  # repo-relative path
    line: int


def extract_keys(
    bundle: Bundle,
    repo_root: Path,
) -> tuple[list[UsageRecord], list[tuple[str, int, str]]]:
    records: list[UsageRecord] = []
    dynamic_hits: list[tuple[str, int, str]] = []

    for src_dir in bundle.src_dirs:
        if not src_dir.exists():
            continue
        for path in iter_source_files(src_dir):
            try:
                text = path.read_text(encoding="utf-8")
            except Exception:
                continue

            file_namespaces = file_useTranslation_namespaces(text)
            candidate_ns_for_bare = tuple(file_namespaces or [bundle.default_ns])

            rel = str(path.relative_to(repo_root))
            for lineno, line in enumerate(text.splitlines(), 1):
                stripped = line.lstrip()
                # Skip comment lines so doc-comments like /** t('nav:...') */
                # don't register as real usages.
                if stripped.startswith(("//", "*", "/*", "/**")):
                    continue
                for m in _TCALL_RE.finditer(line):
                    raw = m.group(1)
                    if ":" in raw:
                        ns, _, key = raw.partition(":")
                        if not key:
                            continue  # `t('ns:'+x)` -- dynamic, not a real key
                        if ns in bundle.namespace_locales:
                            records.append(UsageRecord(key, (ns,), rel, lineno))
                    else:
                        # Bare key -- resolve against file's useTranslation
                        # namespaces, filtering to namespaces this bundle owns.
                        candidates = tuple(
                            ns for ns in candidate_ns_for_bare
                            if ns in bundle.namespace_locales
                        )
                        if candidates:
                            records.append(UsageRecord(raw, candidates, rel, lineno))
                for m in _DYNAMIC_RE.finditer(line):
                    body = m.group(1)
                    if ":" in body or "${" in body:
                        dynamic_hits.append((rel, lineno, body))

    return records, dynamic_hits


# ---------------------------------------------------------------------------
# Hardcoded-string scan
# ---------------------------------------------------------------------------

@dataclass
class HardcodedHit:
    file: str
    line: int
    kind: str    # "jsx-text" | "attr:<name>"
    snippet: str


def _looks_like_english_copy(s: str, *, min_words: int) -> bool:
    s = s.strip()
    if not s or len(s) > 200:
        return False
    if not _HAS_LETTER.search(s):
        return False
    if _LOOKS_LIKE_URL.match(s):
        return False
    if _LOOKS_LIKE_PATH.match(s) and " " not in s:
        return False
    if _LOOKS_LIKE_TOKEN.match(s):
        return False
    if any(op in s for op in _CODE_OPERATORS):
        return False
    if _MEMBER_ACCESS_RE.search(s):
        return False
    # Stand-alone TypeScript snippets like `as Promise`, `new Array`,
    # `typeof Foo`, `keyof Bar`. Real labels rarely start with those.
    first_token = s.split(" ", 1)[0]
    if first_token in {"as", "new", "typeof", "keyof", "instanceof", "in", "of"} and len(s.split()) <= 3:
        return False
    if _LOOKS_LIKE_CSS_TOKENS.match(s) and (" " in s or "-" in s) and not any(
        ch in s for ch in ".!?:,"
    ):
        # Likely className / tailwind / token list.
        return False
    word_count = len(s.split())
    if word_count < min_words:
        # Single-word literals: only flag if they look like a sentence/label
        # (starts with uppercase letter and has only letters/spaces).
        if min_words > 1:
            return False
        if not re.match(r"^[A-Z][A-Za-z]{1,}$", s):
            return False
    return True


def scan_hardcoded_strings(bundle: Bundle, repo_root: Path) -> list[HardcodedHit]:
    hits: list[HardcodedHit] = []
    for src_dir in bundle.src_dirs:
        if not src_dir.exists():
            continue
        for path in iter_source_files(src_dir):
            if path.suffix != ".tsx":
                continue
            rel_str = str(path)
            if any(part in rel_str for part in _I18N_INFRA_PATH_PARTS):
                continue
            try:
                text = path.read_text(encoding="utf-8")
            except Exception:
                continue
            rel = str(path.relative_to(repo_root))
            for lineno, line in enumerate(text.splitlines(), 1):
                stripped = line.lstrip()
                if stripped.startswith(("//", "*", "/*")):
                    continue
                # JSX text content (multi-word) + single-word action labels.
                for m in _JSX_TEXT_RE.finditer(line):
                    raw = m.group(1).strip()
                    # Skip lines that are clearly code like `props.x.y` or `{var}`.
                    if "{" in raw or "}" in raw:
                        continue
                    if _looks_like_english_copy(raw, min_words=2):
                        hits.append(HardcodedHit(rel, lineno, "jsx-text", raw))
                    elif raw in _SINGLE_WORD_ACTION_LABELS:
                        hits.append(HardcodedHit(rel, lineno, "jsx-action-label", raw))
                # User-facing attribute literals
                for m in _ATTR_RE.finditer(line):
                    attr = m.group(1)
                    raw = m.group(2).strip()
                    if "{" in raw:
                        continue
                    if _looks_like_english_copy(raw, min_words=1):
                        hits.append(HardcodedHit(rel, lineno, f"attr:{attr}", raw))
    return hits


# ---------------------------------------------------------------------------
# Bundle analysis
# ---------------------------------------------------------------------------

@dataclass
class BundleAnalysis:
    bundle: Bundle
    # ns -> set of used keys
    used: dict[str, set[str]] = field(default_factory=dict)
    # ns -> first-seen (file, line) per key
    first_seen: dict[str, dict[str, tuple[str, int]]] = field(default_factory=dict)
    # ns -> locale -> {flat_key: value}
    locale_data: dict[str, dict[str, dict[str, str]]] = field(default_factory=dict)
    dynamic_hits: list[tuple[str, int, str]] = field(default_factory=list)
    # Keys missing from EN entirely (no namespace resolved). For bare keys,
    # the report shows all candidate namespaces and the source location.
    bare_unresolved: list[UsageRecord] = field(default_factory=list)
    hardcoded: list[HardcodedHit] = field(default_factory=list)


def analyze(bundle: Bundle, repo_root: Path) -> BundleAnalysis:
    a = BundleAnalysis(bundle=bundle)
    for ns in bundle.namespace_locales:
        a.used[ns] = set()
        a.first_seen[ns] = {}
        a.locale_data[ns] = {}
        for locale in LOCALES:
            a.locale_data[ns][locale] = load_locale(
                bundle.namespace_locales[ns], locale, ns
            )

    records, dynamic_hits = extract_keys(bundle, repo_root)
    a.dynamic_hits = dynamic_hits

    for rec in records:
        # Try to resolve against EN of each candidate namespace.
        resolved_ns: str | None = None
        for ns in rec.candidate_ns:
            if rec.key in a.locale_data[ns]["en"]:
                resolved_ns = ns
                break
        if resolved_ns is None:
            # Unresolved: still record it under the first candidate
            # so Section 1 reports it; also keep file location.
            resolved_ns = rec.candidate_ns[0]
            if len(rec.candidate_ns) > 1:
                a.bare_unresolved.append(rec)
        a.used[resolved_ns].add(rec.key)
        a.first_seen[resolved_ns].setdefault(rec.key, (rec.file, rec.line))

    a.hardcoded = scan_hardcoded_strings(bundle, repo_root)
    return a


# ---------------------------------------------------------------------------
# Report rendering
# ---------------------------------------------------------------------------

def md_table(headers: list[str], rows: list[list[str]]) -> str:
    lines = [
        "| " + " | ".join(headers) + " |",
        "| " + " | ".join(["---"] * len(headers)) + " |",
    ]
    for row in rows:
        lines.append("| " + " | ".join(row) + " |")
    return "\n".join(lines)


def render_bundle_section(a: BundleAnalysis, lines: list[str]) -> dict[str, int]:
    bundle = a.bundle
    totals = {
        "missing_en": 0,
        "missing_non_en": 0,
        "orphan": 0,
        "stale": 0,
        "dynamic": len(a.dynamic_hits),
        "hardcoded": len(a.hardcoded),
        "used": sum(len(v) for v in a.used.values()),
    }

    lines.append("---")
    lines.append("")
    lines.append(f"# Bundle: `{bundle.name}`")
    lines.append("")
    lines.append(f"- Source dirs: {', '.join(str(p) for p in bundle.src_dirs)}")
    lines.append(f"- Default namespace: `{bundle.default_ns}`")
    lines.append(f"- Namespaces: {', '.join(sorted(bundle.namespace_locales))}")
    lines.append("")

    # Section 1: Missing from EN
    missing_en: dict[str, list[str]] = {}
    for ns, keys in a.used.items():
        en_keys = set(a.locale_data[ns]["en"].keys())
        missing = sorted(keys - en_keys)
        if missing:
            missing_en[ns] = missing
    totals["missing_en"] = sum(len(v) for v in missing_en.values())

    if missing_en:
        lines.append(f"## 🔴 Section 1: Keys used in source but missing from EN ({totals['missing_en']})")
        lines.append("")
        lines.append("These are the highest-priority gaps -- the UI renders raw key strings.")
        lines.append("")
        for ns, keys in sorted(missing_en.items()):
            lines.append(f"### `{ns}` namespace ({len(keys)} missing)")
            lines.append("")
            rows = []
            for k in keys:
                loc = a.first_seen.get(ns, {}).get(k)
                where = f"{loc[0]}:{loc[1]}" if loc else "(unknown)"
                rows.append([f"`{ns}:{k}`", f"`{where}`"])
            lines.append(md_table(["Key", "First seen"], rows))
            lines.append("")
    else:
        lines.append("## ✅ Section 1: No keys missing from EN")
        lines.append("")

    # Section 2: EN keys missing from ES or ZH
    missing_non_en: dict[str, dict[str, list[str]]] = {}
    for ns in a.used:
        en_keys = set(a.locale_data[ns]["en"].keys())
        for locale in ("es", "zh"):
            other_keys = set(a.locale_data[ns][locale].keys())
            missing = sorted(en_keys - other_keys)
            if missing:
                missing_non_en.setdefault(ns, {})[locale] = missing
    totals["missing_non_en"] = sum(
        len(keys) for d in missing_non_en.values() for keys in d.values()
    )

    if missing_non_en:
        lines.append(f"## 🟡 Section 2: EN keys missing from ES or ZH ({totals['missing_non_en']})")
        lines.append("")
        for ns, locmap in sorted(missing_non_en.items()):
            for locale, keys in sorted(locmap.items()):
                lines.append(f"### `{ns}` -- missing from `{locale}` ({len(keys)} keys)")
                lines.append("")
                lines.append("```")
                for k in keys:
                    lines.append(f"  {k}")
                lines.append("```")
                lines.append("")
    else:
        lines.append("## ✅ Section 2: All EN keys present in ES and ZH")
        lines.append("")

    # Section 3: Orphans in ES/ZH
    orphans: dict[str, dict[str, list[str]]] = {}
    for ns in a.used:
        en_keys = set(a.locale_data[ns]["en"].keys())
        for locale in ("es", "zh"):
            other_keys = set(a.locale_data[ns][locale].keys())
            orphan = sorted(other_keys - en_keys)
            if orphan:
                orphans.setdefault(ns, {})[locale] = orphan
    totals["orphan"] = sum(len(keys) for d in orphans.values() for keys in d.values())

    if orphans:
        lines.append(f"## 🟠 Section 3: Orphan keys in ES/ZH not in EN ({totals['orphan']})")
        lines.append("")
        for ns, locmap in sorted(orphans.items()):
            for locale, keys in sorted(locmap.items()):
                lines.append(f"### `{ns}` -- orphan in `{locale}` ({len(keys)} keys)")
                lines.append("")
                lines.append("```")
                for k in keys:
                    lines.append(f"  {k}")
                lines.append("```")
                lines.append("")

    # Section 4: Stale EN keys
    stale: dict[str, list[str]] = {}
    for ns in a.used:
        en_keys = set(a.locale_data[ns]["en"].keys())
        used_keys = a.used[ns]
        s = sorted(en_keys - used_keys)
        if s:
            stale[ns] = s
    totals["stale"] = sum(len(v) for v in stale.values())

    lines.append(f"## ⚪ Section 4: Potentially stale EN keys (not found in source scans) -- {totals['stale']}")
    lines.append("")
    lines.append("> Dynamic keys (e.g. `t('pages:foo.'+x)`) won't appear in source scans. Cross-check Section 5 before deleting.")
    lines.append("")
    if stale:
        for ns, keys in sorted(stale.items()):
            lines.append(f"<details><summary><code>{ns}</code> -- {len(keys)} keys</summary>")
            lines.append("")
            lines.append("```")
            for k in keys:
                lines.append(f"  {k}")
            lines.append("```")
            lines.append("</details>")
            lines.append("")

    # Section 5: dynamic t() calls
    if a.dynamic_hits:
        lines.append(f"## 🔵 Section 5: Dynamic t() calls -- manual review ({len(a.dynamic_hits)})")
        lines.append("")
        rows = [[f"`{f}`", str(ln), f"`{raw}`"] for f, ln, raw in a.dynamic_hits[:100]]
        if len(a.dynamic_hits) > 100:
            rows.append(["...", "...", f"...and {len(a.dynamic_hits) - 100} more"])
        lines.append(md_table(["File", "Line", "Pattern"], rows))
        lines.append("")

    # Section 6: Hardcoded English strings (new)
    if a.hardcoded:
        lines.append(f"## 🟣 Section 6: Hardcoded English strings in `.tsx` -- manual review ({len(a.hardcoded)})")
        lines.append("")
        lines.append("These are JSX text content or user-facing attribute literals that bypass `t()`. Replace with `t('ns:...')` and add to all three locales. Heuristic — review before bulk-fixing.")
        lines.append("")
        rows = [
            [f"`{h.file}`", str(h.line), f"`{h.kind}`", f"`{h.snippet[:120]}`"]
            for h in a.hardcoded[:200]
        ]
        if len(a.hardcoded) > 200:
            rows.append(["...", "...", "...", f"...and {len(a.hardcoded) - 200} more"])
        lines.append(md_table(["File", "Line", "Kind", "Text"], rows))
        lines.append("")

    # Per-namespace key counts
    lines.append("## Key counts per namespace")
    lines.append("")
    rows = []
    for ns in sorted(a.used.keys()):
        rows.append([
            f"`{ns}`",
            str(len(a.used[ns])),
            str(len(a.locale_data[ns]["en"])),
            str(len(a.locale_data[ns]["es"])),
            str(len(a.locale_data[ns]["zh"])),
        ])
    lines.append(md_table(["Namespace", "Source keys", "EN", "ES", "ZH"], rows))
    lines.append("")

    return totals


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main() -> None:
    parser = argparse.ArgumentParser(description="i18n gap checker")
    parser.add_argument("--repo-root", default=None,
                        help="Path to repo root (default: auto-detect from script location)")
    args = parser.parse_args()

    if args.repo_root:
        repo_root = Path(args.repo_root).resolve()
    else:
        script_dir = Path(__file__).resolve().parent
        repo_root = script_dir.parent.parent

    bundles = build_bundles(repo_root)

    # Filter out bundles whose src tree is missing (graceful for partial checkouts).
    bundles = [b for b in bundles if any(d.exists() for d in b.src_dirs)]
    if not bundles:
        print("ERROR: no frontend bundles found.", file=sys.stderr)
        sys.exit(1)

    print(f"Scanning {len(bundles)} bundle(s): {', '.join(b.name for b in bundles)} ...")

    analyses = [analyze(b, repo_root) for b in bundles]

    now = datetime.now(timezone.utc)
    ts = now.strftime("%Y%m%dT%H%M%SZ")
    report_path = Path(f"/tmp/i18n-gap-{ts}.md")

    lines: list[str] = []
    lines.append(f"# i18n Gap Report -- {now.strftime('%Y-%m-%d %H:%M UTC')}")
    lines.append("")
    lines.append("## Summary")
    lines.append("")
    header = ["Bundle", "Source keys", "Missing EN", "Missing ES/ZH", "Orphan ES/ZH", "Stale EN", "Dynamic t()", "Hardcoded EN"]
    rows = []
    aggregate_totals: dict[str, int] = {k: 0 for k in ["used", "missing_en", "missing_non_en", "orphan", "stale", "dynamic", "hardcoded"]}
    per_bundle_totals: list[tuple[str, dict[str, int]]] = []

    # Render bundle bodies into a separate list so we can put summary first.
    body_lines: list[str] = []
    for a in analyses:
        totals = render_bundle_section(a, body_lines)
        per_bundle_totals.append((a.bundle.name, totals))
        for k in aggregate_totals:
            aggregate_totals[k] += totals[k]

    for name, t in per_bundle_totals:
        rows.append([
            f"`{name}`",
            str(t["used"]),
            f"**{t['missing_en']}**" if t["missing_en"] else "0",
            f"**{t['missing_non_en']}**" if t["missing_non_en"] else "0",
            str(t["orphan"]),
            str(t["stale"]),
            str(t["dynamic"]),
            f"**{t['hardcoded']}**" if t["hardcoded"] else "0",
        ])
    rows.append([
        "**total**",
        str(aggregate_totals["used"]),
        f"**{aggregate_totals['missing_en']}**",
        f"**{aggregate_totals['missing_non_en']}**",
        str(aggregate_totals["orphan"]),
        str(aggregate_totals["stale"]),
        str(aggregate_totals["dynamic"]),
        f"**{aggregate_totals['hardcoded']}**",
    ])
    lines.append(md_table(header, rows))
    lines.append("")
    lines.append("Priorities: Section 1 (missing EN) and Section 6 (hardcoded EN) are user-visible regressions. Section 2 (missing ES/ZH) is the translation gap. Section 3 / 4 are cleanup.")
    lines.append("")
    lines.extend(body_lines)

    report = "\n".join(lines)
    report_path.write_text(report, encoding="utf-8")
    print(report)
    print(f"\n--- Report written to {report_path} ---")


if __name__ == "__main__":
    main()
