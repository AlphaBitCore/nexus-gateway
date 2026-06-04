#!/usr/bin/env python3
"""Deterministic reconciler for sync-provider-pricing.

The LLM's only job is to WebFetch a provider's official pricing page and emit a
normalized desired-state JSON (schema below). THIS script does every mechanical
step deterministically — diff, edit the template JSON, edit seed-baseline.sql,
and emit transactional prod UPDATE SQL — so the reconcile never drifts.

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
      "status": "active",                   # active | deprecated | disabled
      "replaced_by": null,                  # successor code if deprecated/retired
      "deprecation_date": null              # ISO date if deprecated
    }
  ]
}
Only keys present are compared/applied; omit a key to leave that field untouched.

Subcommands:
  diff       <desired.json>              show drift: desired vs template JSON vs seed-baseline
  apply      <desired.json>              edit template JSON (+ index.json modelCount) and
                                         seed-baseline.sql in place (input/output/status)
  prod-sql   <desired.json> [--out f]    emit BEGIN/UPDATE.../COMMIT for the prod Model table

Paths default to repo-relative; override with --repo. Money compared at 1e-9 tolerance.
"""
import argparse
import json
import os
import re
import sys
from decimal import Decimal

TEMPLATE_FIELDS = {  # desired-key -> template-JSON key
    "input": "inputPricePerMillion",
    "output": "outputPricePerMillion",
    "cache_write": "cachedInputWritePricePerMillion",
    "cache_read": "cachedInputReadPricePerMillion",
}
# seed-baseline + prod only carry input/output among the money fields.
SEED_MONEY = {"input": "inputPricePerMillion", "output": "outputPricePerMillion"}
PROD_FIELDS = {
    "input": "inputPricePerMillion",
    "output": "outputPricePerMillion",
    "cache_write": "cachedInputWritePricePerMillion",
    "cache_read": "cachedInputReadPricePerMillion",
}


def repo_paths(repo):
    return {
        "template_dir": os.path.join(repo, "packages/control-plane-ui/public/provider-templates"),
        "dist_dir": os.path.join(repo, "packages/control-plane-ui/dist/provider-templates"),
        "seed": os.path.join(repo, "tools/db-migrate/seed/data/seed-baseline.sql"),
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


# ─── SQL VALUES-tuple tokenizer (for seed-baseline.sql Model INSERT rows) ──────
def split_sql_tuple(s):
    """Split one SQL VALUES tuple body (no outer parens) into raw token strings,
    respecting single-quoted strings ('' escape) and '{...}' array literals."""
    out, buf, i, n, depth, inq = [], [], 0, len(s), 0, False
    while i < n:
        c = s[i]
        if inq:
            buf.append(c)
            if c == "'":
                if i + 1 < n and s[i + 1] == "'":
                    buf.append("'"); i += 2; continue
                inq = False
            i += 1; continue
        if c == "'":
            inq = True; buf.append(c); i += 1; continue
        if c == "," and depth == 0:
            out.append("".join(buf).strip()); buf = []; i += 1; continue
        if c in "([{":
            depth += 1
        elif c in ")]}":
            depth -= 1
        buf.append(c); i += 1
    if buf:
        out.append("".join(buf).strip())
    return out


def parse_insert(line):
    """Return (cols[], raw_tokens[], val_start, val_end) for a Model INSERT line,
    or None. val_start/end bound the tuple body inside VALUES(...)."""
    m = re.search(r'INSERT INTO public\."Model"\s*\(([^)]*)\)\s*VALUES\s*\(', line)
    if not m:
        return None
    cols = [c.strip().strip('"') for c in m.group(1).split(",")]
    vs = m.end()
    # find matching close paren of the VALUES tuple (string/array aware)
    i, depth, inq = vs, 1, False
    while i < len(line):
        c = line[i]
        if inq:
            if c == "'":
                if i + 1 < len(line) and line[i + 1] == "'":
                    i += 2; continue
                inq = False
        elif c == "'":
            inq = True
        elif c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                break
        i += 1
    body = line[vs:i]
    toks = split_sql_tuple(body)
    if len(toks) != len(cols):
        return None
    return cols, toks, vs, i


def num_literal(v):
    # match seed-baseline's 30-dp decimal style for money columns
    return f"{Decimal(str(v)):.30f}".rstrip("0").rstrip(".") if v is not None else "NULL"


# ─── commands ─────────────────────────────────────────────────────────────────
def cmd_diff(args):
    provider, desired, meta = load_desired(args.desired)
    p = repo_paths(args.repo)
    tpath = os.path.join(p["template_dir"], f"{provider}.json")
    tpl = json.load(open(tpath)) if os.path.exists(tpath) else {"models": []}
    tpl_by = {m["code"]: m for m in tpl.get("models", [])}
    seed_by = seed_models(p["seed"], provider, args.repo)

    # Approval-ready header: every change must be confirmable against this source.
    print(f"=== APPROVAL DIFF: {provider} ===")
    print(f"  source : {meta.get('source_url', '?')}")
    print(f"  fetched: {meta.get('fetched', '?')}")
    print(f"  scope  : desired={len(desired)} | template={len(tpl_by)} | seed-rows-matched={len(set(seed_by) & set(desired))}")
    print(f"  ACTION : present each ↓ change WITH the source link above and get explicit user approval BEFORE apply/prod-sql.")
    drift = 0
    for code, want in desired.items():
        t = tpl_by.get(code)
        marks = []
        for k, tk in TEMPLATE_FIELDS.items():
            if k in want and (t is None or not money_eq(want[k], t.get(tk))):
                marks.append(f"{k}: {fmt(None if t is None else t.get(tk))}→{fmt(want[k])}")
        if "status" in want and t is not None and want["status"] != t.get("status", "active"):
            marks.append(f"status: {t.get('status','active')}→{want['status']}")
        if t is None:
            marks.append("NOT in template (add?)")
        s = seed_by.get(code)
        for k, sk in SEED_MONEY.items():
            if k in want and s is not None and not money_eq(want[k], s.get(sk)):
                marks.append(f"seed.{k}: {fmt(s.get(sk))}→{fmt(want[k])}")
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


def seed_models(seed_path, provider, repo):
    """Map code -> {inputPricePerMillion, outputPricePerMillion, status, _line} for
    this provider's Model INSERT rows. Provider matched via providerId is opaque in
    the dump, so we key on code membership in the desired set at call sites; here we
    return ALL Model rows by code and let callers filter."""
    out = {}
    if not os.path.exists(seed_path):
        return out
    for ln, line in enumerate(open(seed_path)):
        if 'INSERT INTO public."Model"' not in line:
            continue
        parsed = parse_insert(line)
        if not parsed:
            continue
        cols, toks, _, _ = parsed
        col_idx = {c: i for i, c in enumerate(cols)}
        if "code" not in col_idx:
            continue
        code = toks[col_idx["code"]].strip().strip("'")
        rec = {"_line": ln}
        for k, sk in SEED_MONEY.items():
            if sk in col_idx:
                raw = toks[col_idx[sk]]
                rec[sk] = None if raw.upper() == "NULL" else float(raw)
        if "status" in col_idx:
            rec["status"] = toks[col_idx["status"]].strip().strip("'")
        out[code] = rec
    return out


def cmd_apply(args):
    provider, desired, _ = load_desired(args.desired)
    p = repo_paths(args.repo)
    # 1) template JSON (+ dist mirror + index.json modelCount)
    for d in (p["template_dir"], p["dist_dir"]):
        tpath = os.path.join(d, f"{provider}.json")
        if not os.path.exists(tpath):
            continue
        tpl = json.load(open(tpath))
        changed = 0
        by = {m["code"]: m for m in tpl.get("models", [])}
        for code, want in desired.items():
            m = by.get(code)
            if m is None:
                continue
            for k, tk in TEMPLATE_FIELDS.items():
                if k in want and not money_eq(want[k], m.get(tk)):
                    m[tk] = want[k]; changed += 1
            if "status" in want and want["status"] != m.get("status", "active"):
                m["status"] = want["status"]; changed += 1
        with open(tpath, "w") as f:
            json.dump(tpl, f, indent=2, ensure_ascii=False)
            f.write("\n")
        print(f"  template {os.path.relpath(tpath, args.repo)}: {changed} field(s) updated")
    _bump_index(p, provider, desired, args.repo)
    # 2) seed-baseline.sql input/output/status (in place, line-precise)
    _apply_seed(p["seed"], desired, args.repo)
    return 0


def _bump_index(p, provider, desired, repo):
    for d in (p["template_dir"], p["dist_dir"]):
        ipath = os.path.join(d, "index.json")
        if not os.path.exists(ipath):
            continue
        idx = json.load(open(ipath))
        tpath = os.path.join(d, f"{provider}.json")
        cnt = len(json.load(open(tpath)).get("models", [])) if os.path.exists(tpath) else None
        for t in idx.get("templates", []):
            if t.get("name") == provider and cnt is not None and t.get("modelCount") != cnt:
                t["modelCount"] = cnt
        with open(ipath, "w") as f:
            json.dump(idx, f, indent=2, ensure_ascii=False)
            f.write("\n")


def _apply_seed(seed_path, desired, repo):
    if not os.path.exists(seed_path):
        return
    lines = open(seed_path).read().split("\n")
    changed = 0
    for i, line in enumerate(lines):
        if 'INSERT INTO public."Model"' not in line:
            continue
        parsed = parse_insert(line)
        if not parsed:
            continue
        cols, toks, vs, ve = parsed
        col_idx = {c: j for j, c in enumerate(cols)}
        if "code" not in col_idx:
            continue
        code = toks[col_idx["code"]].strip().strip("'")
        want = desired.get(code)
        if not want:
            continue
        new_toks = list(toks)
        touched = False
        for k, sk in SEED_MONEY.items():
            if k in want and sk in col_idx:
                cur = toks[col_idx[sk]]
                curv = None if cur.upper() == "NULL" else float(cur)
                if not money_eq(want[k], curv):
                    new_toks[col_idx[sk]] = num_literal(want[k]); touched = True
        if "status" in want and "status" in col_idx:
            if toks[col_idx["status"]].strip().strip("'") != want["status"]:
                new_toks[col_idx["status"]] = f"'{want['status']}'"; touched = True
        if touched:
            lines[i] = line[:vs] + ", ".join(new_toks) + line[ve:]
            changed += 1
    if changed:
        with open(seed_path, "w") as f:
            f.write("\n".join(lines))
    print(f"  seed-baseline.sql: {changed} Model row(s) updated")


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
