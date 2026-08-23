#!/usr/bin/env python3
"""Routing sweep: every rule accounted for, and smart routing proven per class.

Two legs, because the two questions are different:

  Rules leg (simulate) — every routing rule in the deployment, enabled or not,
  gets a probe request synthesized from its own match conditions and pushed
  through POST /api/admin/routing-rules/simulate (the CP forwarder to the
  gateway's real resolver). An enabled rule must appear in the resolution; a
  disabled rule must not — a disabled rule matching is as much a defect as an
  enabled rule missing. Nothing is enabled or edited to make a test pass:
  production config is read, never written.

  Smart leg (live) — model:auto requests through the public gateway, one per
  constraint class (plain text, image, image+file, video, reasoning, and audio
  on the STT endpoint). The judgment is not "200": the routed model, read from
  the response, must carry the capabilities the request required, per the
  admin catalog. A 200 answered by a model that cannot read the attachment is
  a routing failure wearing a success code.

Every live request carries an X-Request-ID (recorded per row) so the
traffic_event reconciliation pass can attribute rows exactly.

NDJSON resume + --report, same conventions as the other matrices.
"""

import argparse
import base64
import json
import os
import subprocess
import sys
import time
import uuid


def curl(url, method="GET", headers=None, body=None, timeout=120):
    cmd = ["curl", "-sS", "-o", "-", "-w", "\n%{http_code}", "-X", method,
           "--max-time", str(timeout), url]
    for k, v in (headers or {}).items():
        cmd += ["-H", f"{k}: {v}"]
    if body is not None:
        cmd += ["-H", "Content-Type: application/json", "-d", json.dumps(body)]
    p = subprocess.run(cmd, capture_output=True, text=True)
    out = p.stdout
    if "\n" not in out:
        return 0, out or p.stderr
    payload, code = out.rsplit("\n", 1)
    try:
        return int(code), payload
    except ValueError:
        return 0, out


def jload(payload, default=None):
    try:
        return json.loads(payload)
    except Exception:
        return default


# ---------------------------------------------------------------- rules leg

def synthesize_probe(rule, models_by_id):
    """A simulate body that this rule's own match conditions say it should
    capture. Returns (body, note) or (None, why-not)."""
    mc = rule.get("matchConditions") or {}
    lits = mc.get("requestedModelLiterals") or []
    if lits:
        return {"modelId": lits[0], "endpointType": "chat"}, f"literal {lits[0]!r}"
    ids = mc.get("models") or []
    if ids:
        m = models_by_id.get(ids[0])
        if not m:
            return None, f"match names model id {ids[0]} which the catalog no longer has"
        return {"modelId": m["code"], "endpointType": "chat"}, f"model {m['code']}"
    if not mc:
        # catch-all: any named model reaches it (when enabled and nothing
        # higher-priority captures the request first)
        any_m = next(iter(models_by_id.values()), None)
        if not any_m:
            return None, "no models in catalog to probe a catch-all with"
        return {"modelId": any_m["code"], "endpointType": "chat"}, f"catch-all via {any_m['code']}"
    return None, f"unsupported match shape: {json.dumps(mc)[:120]}"


def rule_in_resolution(sim, rule):
    """Where, if anywhere, the resolution mentions this rule."""
    rid, rname = rule.get("id", ""), rule.get("name", "")
    blob = json.dumps(sim)
    hits = []
    if rid and rid in blob:
        hits.append("id")
    if rname and rname in blob:
        hits.append("name")
    return hits


def run_rules_leg(args, cp, done, out):
    code, payload = curl(f"{args.cp_url}/api/admin/routing-rules", headers=cp)
    rules = (jload(payload, {}) or {}).get("data") or jload(payload, [])
    if code != 200 or not isinstance(rules, list):
        sys.exit(f"could not list routing rules: HTTP {code} {payload[:200]}")
    code, payload = curl(f"{args.cp_url}/api/admin/models/flat?limit=1000", headers=cp)
    models = (jload(payload, {}) or {}).get("data") or []
    models_by_id = {m["id"]: m for m in models if m.get("enabled")}

    print(f"{len(rules)} rules, {len(models_by_id)} enabled models")
    for rule in rules:
        key = ("rule", rule["id"])
        if key in done:
            continue
        probe, note = synthesize_probe(rule, models_by_id)
        row = {"kind": "rule", "rule_id": rule["id"], "rule_name": rule.get("name"),
               "enabled": rule.get("enabled"), "priority": rule.get("priority"),
               "probe": note}
        if probe is None:
            row["verdict"] = "UNPROBEABLE"
            row["why"] = note
        else:
            code, payload = curl(f"{args.cp_url}/api/admin/routing-rules/simulate",
                                 method="POST", headers=cp, body=probe)
            sim = jload(payload, {})
            hits = rule_in_resolution(sim, rule) if code == 200 else []
            if code != 200:
                row["verdict"] = "SIM_ERROR"
                row["why"] = f"HTTP {code} {payload[:200]}"
            elif rule.get("enabled"):
                row["verdict"] = "MATCHED" if hits else "ENABLED_BUT_ABSENT"
                row["mentioned_as"] = hits
                row["substituted"] = sim.get("substituted")
                row["targets"] = [t.get("modelCode") for t in (sim.get("targets") or [])][:5]
            else:
                row["verdict"] = "DISABLED_INERT" if not hits else "DISABLED_YET_MATCHED"
                row["mentioned_as"] = hits
        out.write(json.dumps({"key": list(key), **row}, ensure_ascii=False) + "\n")
        out.flush()
        print(f"  rule {rule.get('name')}: {row['verdict']}")


# ---------------------------------------------------------------- smart leg

def b64file(root, name):
    with open(os.path.join(root, "tests/fixtures/media", name), "rb") as f:
        return base64.b64encode(f.read()).decode()


def smart_cases(root):
    img = b64file(root, "ocr-19452.png")
    pdf = b64file(root, "doc.pdf")
    vid = b64file(root, "clip.mp4")
    text = lambda t: {"type": "text", "text": t}
    image = {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{img}"}}
    filep = {"type": "file", "file": {"filename": "doc.pdf",
                                      "file_data": f"data:application/pdf;base64,{pdf}"}}
    video = {"type": "video_url", "video_url": {"url": f"data:video/mp4;base64,{vid}"}}
    return [
        ("plain_text", {"messages": [{"role": "user", "content": "Reply with the word ready."}]},
         set()),
        ("image", {"messages": [{"role": "user", "content": [
            image, text("What number is in this image? Digits only.")]}]},
         {"image"}),
        ("image_and_file", {"messages": [{"role": "user", "content": [
            image, filep, text("Name the number in the image and the number in the document.")]}]},
         {"image", "file"}),
        ("video", {"messages": [{"role": "user", "content": [
            video, text("What number appears in this video? Digits only.")]}]},
         {"video"}),
        ("reasoning", {"messages": [{"role": "user", "content":
            "A farmer has 17 sheep. All but 9 run away. He buys twice as many as "
            "remained. How many now? Just the number."}],
            "reasoning_effort": "low"},
         set()),  # constraint checked on features, not modalities
    ]


def run_smart_leg(args, cp, done, out):
    vk = open(args.vk_file).read().strip()
    code, payload = curl(f"{args.cp_url}/api/admin/models/flat?limit=1000", headers=cp)
    models = (jload(payload, {}) or {}).get("data") or []
    caps = {m["code"]: m for m in models}
    # the served model comes back as the provider's wire id; index those too
    for m in models:
        caps.setdefault(m.get("providerModelId") or "", m)

    for name, body_part, needs in smart_cases(args.root):
        key = ("smart", name)
        if key in done:
            continue
        rid = f"{args.run_id}-smart-{name}-{uuid.uuid4().hex[:8]}"
        body = {"model": "auto", "max_tokens": 512, **body_part}
        t0 = time.time()
        code, payload = curl(f"{args.gw}/v1/chat/completions", method="POST",
                             headers={"Authorization": "Bearer " + vk,
                                      "X-Request-ID": rid},
                             body=body, timeout=args.timeout)
        row = {"kind": "smart", "case": name, "request_id": rid, "status": code,
               "latency_s": round(time.time() - t0, 1)}
        d = jload(payload, {})
        if code != 200:
            row["verdict"] = "REFUSED"
            row["error"] = json.dumps((d or {}).get("error", payload))[:300]
        else:
            served = (d.get("model") or "").strip()
            m = caps.get(served) or next(
                (v for k, v in caps.items() if k and served.startswith(k)), None)
            row["served_by"] = served
            content = ((d.get("choices") or [{}])[0].get("message") or {}).get("content") or ""
            row["content_head"] = content[:100]
            if m is None:
                row["verdict"] = "SERVED_BY_UNKNOWN_MODEL"
            else:
                missing = needs - set(m.get("inputModalities") or [])
                if name == "reasoning" and "reasoning" not in (m.get("features") or []):
                    missing = missing | {"feature:reasoning"}
                row["verdict"] = "ROUTED_CAPABLE" if not missing else "ROUTED_BLIND"
                if missing:
                    row["missing"] = sorted(missing)
        out.write(json.dumps({"key": list(key), **row}, ensure_ascii=False) + "\n")
        out.flush()
        print(f"  smart {name}: {row['verdict']} ({row.get('served_by', row.get('error', ''))[:60]})")

    # audio on the STT endpoint: auto must land on an stt-capable model
    key = ("smart", "stt_audio")
    if key not in done:
        rid = f"{args.run_id}-smart-stt-{uuid.uuid4().hex[:8]}"
        audio = os.path.join(args.root, "tests/fixtures/media/speech.mp3")
        p = subprocess.run(
            ["curl", "-sS", "-o", "-", "-w", "\n%{http_code}",
             f"{args.gw}/v1/audio/transcriptions",
             "-H", "Authorization: Bearer " + vk,
             "-H", "X-Request-ID: " + rid,
             "-F", "model=auto", "-F", f"file=@{audio};type=audio/mpeg",
             "--max-time", str(args.timeout)],
            capture_output=True, text=True)
        payload, _, codestr = p.stdout.rpartition("\n")
        code = int(codestr) if codestr.isdigit() else 0
        d = jload(payload, {})
        row = {"kind": "smart", "case": "stt_audio", "request_id": rid, "status": code}
        if code == 200:
            row["verdict"] = "TRANSCRIBED" if (d or {}).get("text") else "NO_TEXT"
            row["text_head"] = ((d or {}).get("text") or "")[:100]
        else:
            row["verdict"] = "REFUSED"
            row["error"] = payload[:300]
        out2 = open(args.out, "a")
        out2.write(json.dumps({"key": list(key), **row}, ensure_ascii=False) + "\n")
        out2.close()
        print(f"  smart stt_audio: {row['verdict']}")


# ---------------------------------------------------------------- reporting

def load_done(path):
    done = {}
    if path and os.path.exists(path):
        with open(path) as f:
            for line in f:
                r = jload(line)
                if r and "key" in r:
                    done[tuple(r["key"])] = r
    return done


def report(done):
    rules = [(k, r) for k, r in sorted(done.items()) if r.get("kind") == "rule"]
    smart = [(k, r) for k, r in sorted(done.items()) if r.get("kind") == "smart"]
    print(f"=== rules: {len(rules)} ===")
    for _, r in rules:
        state = "enabled" if r.get("enabled") else "disabled"
        print(f"  {r.get('rule_name'):<24} p{r.get('priority')} {state:<9} {r.get('verdict')} "
              f"{r.get('why', '')[:100]}")
    print(f"=== smart classes: {len(smart)} ===")
    for _, r in smart:
        print(f"  {r.get('case'):<16} {r.get('verdict'):<20} served_by={r.get('served_by', '-')} "
              f"{('missing=' + ','.join(r.get('missing', []))) if r.get('missing') else ''}")
    bad = [r for _, r in rules + smart if r.get("verdict") not in
           ("MATCHED", "DISABLED_INERT", "ROUTED_CAPABLE", "TRANSCRIBED")]
    print(f"=== needing triage: {len(bad)} ===")
    for r in bad:
        print(f"  {r.get('rule_name') or r.get('case')}: {r.get('verdict')} "
              f"{(r.get('why') or r.get('error') or '')[:160]}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--gw", default=os.environ.get("NEXUS_AI_GW_URL", ""))
    ap.add_argument("--cp-url", default=os.environ.get("NEXUS_CP_URL", ""))
    ap.add_argument("--cp-token", default=os.environ.get("NEXUS_CP_TOKEN", ""))
    ap.add_argument("--vk-file", default=os.environ.get("NEXUS_VK_FILE", ""))
    ap.add_argument("--root", default=".")
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--report", action="store_true")
    ap.add_argument("--run-id", default="rtx")
    args = ap.parse_args()

    done = load_done(args.out)
    if args.report:
        report(done)
        return
    if not args.cp_url or not args.cp_token:
        sys.exit("--cp-url and --cp-token required (cp_token from tests/lib/auth.sh)")
    if not args.gw or not args.vk_file:
        sys.exit("--gw and --vk-file required")
    cp = {"Authorization": "Bearer " + args.cp_token}

    with open(args.out, "a") as out:
        run_rules_leg(args, cp, done, out)
        run_smart_leg(args, cp, done, out)
    report(load_done(args.out))


if __name__ == "__main__":
    main()
