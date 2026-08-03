#!/usr/bin/env python3
"""Deterministic reconciler for sync-provider-pricing.

The LLM's only job is to WebFetch a provider's official pricing page and emit a
normalized desired-state JSON (schema below). THIS script does every mechanical
step deterministically — diff, edit the single-source catalog + regenerate its
derived files, and emit transactional prod UPDATE SQL — so the reconcile never drifts.

Source of truth: `tools/db-migrate/model-catalog.json`. `apply` edits THAT file and
regenerates `Model.json` + `provider-templates/*.json` + `index.json` from it — never
edit those derived files directly (CI `check:model-catalog` fails on drift).

Desired-state JSON (one file per provider, produced by the LLM from the official page):
{
  "provider": "anthropic",
  "source_url": "https://platform.claude.com/docs/en/about-claude/pricing",
  "fetched": "2026-06-03",
  "models": [
    {
      "code": "claude-opus-4-8",          # REQUIRED, matches Model.code
      "input": 5.0,                         # $/MTok base input  (REQUIRED for chat)
      "output": 25.0,                       # $/MTok output
      "cache_write": 6.25,                  # $/MTok standard (short) cache write; 0 if none
      "cache_read": 0.5,                    # $/MTok cache hit/read; null for non-chat
      "audio_input": 32.0,                  # $/MTok audio-in (realtime models only)
      "audio_output": 64.0,                 # $/MTok audio-out (realtime models only)
      "cached_audio_read": 0.4,             # $/MTok cached audio-in read (realtime; null = no discount)
      "status": "active",                   # active | deprecated | disabled
      "replaced_by": null,                  # successor code if deprecated/retired
      "deprecation_date": null              # ISO date if deprecated
    }
  ]
}
Only keys present are compared/applied; omit a key to leave that field untouched.

Subcommands:
  diff       <desired.json>              show drift: desired vs the generated template + fixture
  apply      <desired.json>              edit model-catalog.json (money fields + seed status)
                                         then regenerate Model.json + provider-templates
  prod-sql   <desired.json> [--out f]    emit BEGIN/UPDATE.../COMMIT for the prod Model table

Paths default to repo-relative; override with --repo. Money compared at 1e-9 tolerance.
"""
import argparse
import json
import os
import sys
from decimal import Decimal

TEMPLATE_FIELDS = {  # desired-key -> template-JSON key
    "input": "inputPricePerMillion",
    "output": "outputPricePerMillion",
    "cache_write": "cachedInputWritePricePerMillion",
    "cache_read": "cachedInputReadPricePerMillion",
    # Realtime models: audio-token rates, USD per 1M tokens.
    "audio_input": "audioInputPricePerMillion",
    "audio_output": "audioOutputPricePerMillion",
    "cached_audio_read": "cachedAudioInputReadPricePerMillion",
}
# fixture + prod carry all 7 money fields.
FIXTURE_MONEY = {
    "input": "inputPricePerMillion",
    "output": "outputPricePerMillion",
    "cache_write": "cachedInputWritePricePerMillion",
    "cache_read": "cachedInputReadPricePerMillion",
    "audio_input": "audioInputPricePerMillion",
    "audio_output": "audioOutputPricePerMillion",
    "cached_audio_read": "cachedAudioInputReadPricePerMillion",
}
PROD_FIELDS = {
    "input": "inputPricePerMillion",
    "output": "outputPricePerMillion",
    "cache_write": "cachedInputWritePricePerMillion",
    "cache_read": "cachedInputReadPricePerMillion",
    "audio_input": "audioInputPricePerMillion",
    "audio_output": "audioOutputPricePerMillion",
    "cached_audio_read": "cachedAudioInputReadPricePerMillion",
}


def repo_paths(repo):
    return {
        "template_dir": os.path.join(repo, "packages/control-plane-ui/public/provider-templates"),
        "fixture": os.path.join(repo, "tools/db-migrate/seed/fixtures/Model.json"),
        "catalog": os.path.join(repo, "tools/db-migrate/model-catalog.json"),
        "gen": os.path.join(repo, "tools/db-migrate/gen-model-catalog.mjs"),
    }


def load_desired(path):
    with open(path) as f:
        d = json.load(f)
    if "provider" not in d or "models" not in d:
        sys.exit("desired JSON must have 'provider' and 'models'")
    by_code = {}
    for m in d["models"]:
        if "code" not in m:
            sys.exit(f"model entry missing 'code': {m}")
        by_code[m["code"]] = m
    return d["provider"], by_code, d


def money_eq(a, b):
    if a is None or b is None:
        return a is None and b is None
    return abs(Decimal(str(a)) - Decimal(str(b))) < Decimal("1e-9")


def fmt(v):
    return "·" if v is None else f"{Decimal(str(v)):g}"


def load_fixture(fixture_path):
    """Load Model.json fixture; return (rows_list, by_code_dict).
    by_code maps code -> row object (same reference as rows_list entry)."""
    if not os.path.exists(fixture_path):
        return [], {}
    with open(fixture_path) as f:
        rows = json.load(f)
    by_code = {r["code"]: r for r in rows}
    return rows, by_code


def num_literal(v):
    # Emit a decimal literal suitable for prod UPDATE SQL (30-dp style, trailing zeros stripped).
    return f"{Decimal(str(v)):.30f}".rstrip("0").rstrip(".") if v is not None else "NULL"


# ─── commands ─────────────────────────────────────────────────────────────────
def cmd_diff(args):
    provider, desired, meta = load_desired(args.desired)
    p = repo_paths(args.repo)
    tpath = os.path.join(p["template_dir"], f"{provider}.json")
    tpl = json.load(open(tpath)) if os.path.exists(tpath) else {"models": []}
    tpl_by = {m["code"]: m for m in tpl.get("models", [])}
    _, fixture_by = load_fixture(p["fixture"])
    # Only show fixture rows for codes present in desired.
    fixture_matched = {code: fixture_by[code] for code in desired if code in fixture_by}

    # Approval-ready header: every change must be confirmable against this source.
    print(f"=== APPROVAL DIFF: {provider} ===")
    print(f"  source : {meta.get('source_url', '?')}")
    print(f"  fetched: {meta.get('fetched', '?')}")
    print(f"  scope  : desired={len(desired)} | template={len(tpl_by)} | fixture-rows-matched={len(fixture_matched)}")
    print(f"  ACTION : present each ↓ change WITH the source link above and get explicit user approval BEFORE apply/prod-sql.")
    drift = 0
    for code, want in desired.items():
        t = tpl_by.get(code)
        marks = []
        for k, tk in TEMPLATE_FIELDS.items():
            if k in want and (t is None or not money_eq(want[k], t.get(tk))):
                marks.append(f"{k}: {fmt(None if t is None else t.get(tk))}→{fmt(want[k])}")
        if t is None:
            marks.append("NOT in template (add?)")
        s = fixture_by.get(code)
        for k, fk in FIXTURE_MONEY.items():
            if k in want and s is not None and not money_eq(want[k], s.get(fk)):
                marks.append(f"fixture.{k}: {fmt(s.get(fk))}→{fmt(want[k])}")
        # status is a seed-only field: it lives on the Model.json (fixture) row, not
        # the wizard template. Compare against the fixture so the preview is accurate.
        if "status" in want and s is not None and s.get("status", "active") != want["status"]:
            marks.append(f"status: {s.get('status','active')}→{want['status']}")
        if marks:
            drift += 1
            print(f"  {code}: " + " | ".join(marks))
        else:
            print(f"  {code}: ok")
    extra = set(tpl_by) - set(desired)
    if extra:
        print(f"  -- in template but NOT on official page (deprecate?): {sorted(extra)}")
    print(f"=== {drift} model(s) drifted ===")
    return 0


def cmd_apply(args):
    provider, desired, _ = load_desired(args.desired)
    p = repo_paths(args.repo)
    with open(p["catalog"], encoding="utf-8") as f:
        catalog = json.load(f)
    block = next((b for b in catalog.get("providers", []) if b.get("key") == provider), None)
    if block is None:
        sys.exit(f"apply: provider '{provider}' not found in {os.path.relpath(p['catalog'], args.repo)}")
    by_code = {m["code"]: m for m in block.get("models", [])}
    changed, missing = 0, []
    for code, want in desired.items():
        m = by_code.get(code)
        if m is None:
            missing.append(code)
            continue
        # Money fields are shared on the model entry — one edit feeds both the
        # generated Model.json row and the wizard template.
        for k, fk in FIXTURE_MONEY.items():
            if k in want and not money_eq(want[k], m.get(fk)):
                m[fk] = want[k]
                changed += 1
        # status is a seed-only field (Model.json); wizard templates carry none.
        if "status" in want and isinstance(m.get("seed"), dict) and m["seed"].get("status") != want["status"]:
            m["seed"]["status"] = want["status"]
            changed += 1
    with open(p["catalog"], "w", encoding="utf-8") as f:
        json.dump(catalog, f, indent=2, ensure_ascii=False)
        f.write("\n")
    print(f"  {os.path.relpath(p['catalog'], args.repo)}: {changed} field(s) updated")
    if missing:
        print(f"  NOTE: {len(missing)} desired model(s) not in the catalog — add them there first: {missing}")
    _regenerate(p, args.repo)
    return 0


def _regenerate(p, repo):
    """Regenerate Model.json + provider-templates/*.json + index.json from the catalog."""
    import subprocess
    r = subprocess.run(["node", p["gen"]], cwd=repo, capture_output=True, text=True)
    if r.stdout:
        sys.stdout.write(r.stdout)
    if r.returncode != 0:
        sys.stderr.write(r.stderr)
        sys.exit(f"apply: gen-model-catalog.mjs failed (exit {r.returncode})")


def cmd_prod_sql(args):
    provider, desired, meta = load_desired(args.desired)
    out = ["BEGIN;",
           f"-- sync-provider-pricing: {provider} from {meta.get('source_url','?')} ({meta.get('fetched','?')})"]
    for code, want in desired.items():
        sets = []
        for k, pf in PROD_FIELDS.items():
            if k in want:
                v = "NULL" if want[k] is None else num_literal(want[k])
                sets.append(f'"{pf}" = {v}')
        if "status" in want:
            sets.append(f"\"status\" = '{want['status']}'")
            sets.append(f"\"lifecycle\" = '{'deprecated' if want['status']=='deprecated' else 'ga'}'")
        if want.get("replaced_by"):
            sets.append(f"\"replacedBy\" = '{want['replaced_by']}'")
        if want.get("deprecation_date"):
            sets.append(f"\"deprecationDate\" = '{want['deprecation_date']}'")
        if not sets:
            continue
        sets.append('"updatedAt" = now()')
        out.append(
            f'UPDATE public."Model" SET {", ".join(sets)} '
            f"WHERE code = '{code}' "
            f'AND "providerId" = (SELECT id FROM public."Provider" WHERE name = \'{provider}\');')
    out.append("COMMIT;")
    sql = "\n".join(out) + "\n"
    if args.out:
        open(args.out, "w").write(sql)
        print(f"wrote {args.out}")
    else:
        sys.stdout.write(sql)
    return 0


def main():
    ap = argparse.ArgumentParser(description="Deterministic provider-pricing reconciler")
    ap.add_argument("--repo", default=os.getcwd(), help="repo root (default: cwd)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("diff", "apply"):
        s = sub.add_parser(name); s.add_argument("desired")
    s = sub.add_parser("prod-sql"); s.add_argument("desired"); s.add_argument("--out", default="")
    args = ap.parse_args()
    return {"diff": cmd_diff, "apply": cmd_apply, "prod-sql": cmd_prod_sql}[args.cmd](args)


if __name__ == "__main__":
    sys.exit(main())
