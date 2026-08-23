#!/usr/bin/env python3
"""chat-modality-matrix — what every chat model actually accepts, measured.

The catalog says a model takes `image`. It does not say whether it takes an
image by URL, whether it takes a HEIC, whether the document it accepts is a PDF
or markdown — and those turned out to be four different answers on the same
providers. Two of the largest accept DISJOINT document types.

This sweeps the question rather than guessing it. One row per
(model, case), and the declared capability beside the measured one so a catalog
row that lies is visible in both directions.

OUTCOMES. Two would not be enough; six is the minimum that keeps distinct things
distinct, because Phase C has to assign a cause to every non-READ cell and a
cause it cannot recover from the row is a cause it has to re-run for.

    READ                the anchor came back — the model used the attachment
    ACCEPTED_NOT_READ   200, and the anchor is absent. The dangerous one: it
                        looks like success
    ACCEPTED            200 on a cell with no anchor. Acceptance, NOT evidence
                        of reading, and reported separately for that reason
    TRUNCATED           200 with empty content and finish_reason "length" — a
                        statement about the token budget, not the attachment.
                        Measured at 20 cells under a 256-token ceiling, almost
                        all of them on one reasoning model, which is why the
                        default is 1024: a cell lost to the budget is a cell
                        not measured, and the extra output costs a few dollars
                        across the whole sweep
    REFUSED             4xx, tagged with WHOSE refusal: CATALOG (our routing
                        guard, from the row under test), OURS (our codec), or
                        VENDOR (their parser reached the caller, itself a defect)
    INFRA               429 / 5xx / timeout / no connection. NOT written to the
                        results file, so a resume retries instead of recording a
                        transient blip as a permanent capability fact

WHY AN ANCHOR. A model asked "what is in this document" will answer plausibly
whether or not it received one. Each fixture carries a value that cannot be
guessed — 19452 in the image, 38617 in the PDF, 52903 in the markdown, 61074 in
the JSON, 74128 in the video, the spoken phrase "regression fixture" — so
ACCEPTED_NOT_READ stays separable from READ. Anchors are matched on word
boundaries: an earlier version matched substrings and scored "an image is
required" as having read a red square.

WHY A NEGATIVE CONTROL. Once per modality the model appeared to read, the same
question with no attachment. If the anchor comes back anyway, every READ for
that modality is void. A control that did not get a 200 is INCONCLUSIVE, never
clean — a check that could not have shown a leak has not shown its absence.

WHAT THIS CANNOT SEE. It measures through the gateway, so a REFUSED/CATALOG cell
says our own routing guard answered before the wire did. That is a real thing a
caller experiences and a real catalog defect, but it is not a measurement of the
model. Those cells are reported as UNMEASURED and land on the probe-3 worklist.

COST. Real money, though not much of it: the sweep is a few hundred calls at a
few hundred tokens each. Use --dry-run first; it prints the schedule and the
call-count range without spending anything.

    python3 tests/scripts/chat-modality-matrix.py --out m.ndjson --dry-run
    python3 tests/scripts/chat-modality-matrix.py --out m.ndjson --max-calls 600
    python3 tests/scripts/chat-modality-matrix.py --out m.ndjson --report
"""
import argparse
import base64
import hashlib
import json
import os
import re
import subprocess
import sys
import time

FIXTURES = "tests/fixtures/media"

# Bumped whenever build() changes what goes on the wire, and whenever the case
# table gains a cell whose absence would otherwise read as "measured and fine". It rides in the case
# fingerprint so a resumed run re-measures rather than blending rows produced by
# two different request shapes — the fixture bytes alone cannot see a change to
# the body around them.
REQUEST_SHAPE = 3

# Each case: what to send, and the anchor that proves the model read it.
# `needs` names a case that must have SUCCEEDED first — a model that cannot take
# an image at all is not asked about HEIC. It gates on FORMAT, never on carrier:
# image_url_https tests a different axis and is deliberately ungated.
CASES = [
    # id                    modality  file             mime                       anchor   needs
    ("text",                "text",   None,            None,                      None,    None),
    ("image_inline_png",    "image",  "ocr-19452.png", "image/png",               "19452", None),
    ("image_inline_jpg",    "image",  "ocr-19452.jpg", "image/jpeg",              "19452", "image_inline_png"),
    ("image_inline_gif",    "image",  "ocr-19452.gif", "image/gif",               "19452", "image_inline_png"),
    ("image_inline_webp",   "image",  "ocr-19452.webp", "image/webp",             "19452", "image_inline_png"),
    ("image_inline_bmp",    "image",  "ocr-19452.bmp",  "image/bmp",              "19452", "image_inline_png"),
    ("image_inline_tiff",   "image",  "ocr-19452.tiff", "image/tiff",             "19452", "image_inline_png"),
    ("image_inline_heic",   "image",  "image.heic",    "image/heic",              None,    "image_inline_png"),
    ("image_inline_svg",    "image",  "hostile.svg",   "image/svg+xml",           None,    "image_inline_png"),
    ("image_url_https",     "image",  None,            None,                      "19452", None),
    ("doc_markdown",        "file",   "doc.md",        "text/markdown",           "52903", None),
    ("doc_plaintext",       "file",   "doc.txt",       "text/plain",              "52903", None),
    ("doc_pdf",             "file",   "doc.pdf",       "application/pdf",         "38617", None),
    ("doc_json",            "file",   "doc.json",      "application/json",        "61074", "doc_markdown"),
    # What a browser sends for an unrecognised extension. The canonical case of
    # "we spelled it wrong" rather than "the wire cannot" — the bytes are the
    # markdown the wire already read under another name.
    ("doc_octet_stream",    "file",   "doc.md",        "application/octet-stream", "52903", "doc_markdown"),
    ("audio_wav",           "audio",  "speech.wav",    "audio/wav",               "regression fixture", None),
    ("audio_mp3",           "audio",  "speech.mp3",    "audio/mpeg",              "regression fixture", "audio_wav"),
    ("video_mp4",           "video",  "clip.mp4",      "video/mp4",               "74128|412", None),
]

# The one cell whose fixture we cannot serve ourselves. Left unconfigured on
# purpose: the previous default pointed at a third-party thumbnail that answers
# 400 today, so every model paid for a call that resolved to an HTML error page
# and the results split between a false "vendor refusal" and a false "we drop
# URL images". A cell that cannot produce evidence is not run.
IMAGE_URL_ENV = "NEXUS_MATRIX_IMAGE_URL"

PROMPTS = {
    "text":  "Reply with only: ok",
    "image": "Reply with only the number shown in this image, digits only.",
    "file":  "What is the reference number in the attached document? Digits only.",
    "audio": "Transcribe the attached audio exactly. Reply with the transcript only.",
    "video": "Reply with only the number shown in this video, digits only.",
}

# Our routing guard refuses from the catalog row under test, before the request
# reaches a wire. Its verdict is a fact about the catalog, never about the model.
CATALOG_CODES = {
    "MODEL_INPUT_MODALITY_UNSUPPORTED",
    "MODEL_REQUIRED_MODALITY_MISSING",
    "MODEL_MODALITY_MISMATCH",
}

TRANSIENT = {0, 408, 409, 425, 429, 500, 502, 503, 504}

# The path the product actually uses, and the reason any of this matters: with
# `auto` the caller does not choose the model and cannot know what it accepts.
# Every other row here addresses a model by name, which takes the explicit-model
# passthrough — a different path from the resolver, and one whose own code
# records that the two drifted apart once already.
#
# Swept as a synthetic model so it runs the same cases through the same
# classifier. It declares nothing, so it is excluded from the catalog lists:
# "auto reads a PDF but does not declare file" would be a claim about a router,
# not about a row anyone can fix.
AUTO_MODEL = {"id": "auto", "type": "chat", "inputModalities": [],
              "outputModalities": [], "requiredModalities": [], "owned_by": "nexus-routing"}


def fixture_bytes(root, name):
    with open(os.path.join(root, FIXTURES, name), "rb") as f:
        return f.read()


def data_url(root, name, mime):
    return "data:" + mime + ";base64," + base64.b64encode(fixture_bytes(root, name)).decode()


def case_fingerprint(case, root, max_tokens, image_url):
    """Identifies the case DEFINITION, not just its name.

    The resume key includes this because every repair to this harness changes a
    fixture, a media type, an anchor or the token budget — and without it a
    resumed run silently blends rows produced by the old instrument with rows
    produced by the new one, into a single report that describes neither.
    """
    cid, modality, fname, mime, anchor, _needs = case
    h = hashlib.sha256()
    for piece in (cid, modality, str(mime), str(anchor), PROMPTS[modality], str(max_tokens),
                  str(REQUEST_SHAPE)):
        h.update(piece.encode() + b"\x00")
    if cid == "image_url_https":
        h.update(image_url.encode())
    elif fname:
        h.update(fixture_bytes(root, fname))
    return h.hexdigest()[:12]


def build(model, case, root, max_tokens, image_url, lean=False):
    cid, modality, fname, mime, _anchor, _needs = case
    text = {"type": "text", "text": PROMPTS[modality]}
    if modality == "text":
        content = PROMPTS["text"]
    elif cid == "image_url_https":
        content = [{"type": "image_url", "image_url": {"url": image_url}}, text]
    elif modality == "image":
        content = [{"type": "image_url", "image_url": {"url": data_url(root, fname, mime)}}, text]
    elif modality == "file":
        content = [{"type": "file",
                    "file": {"filename": fname, "file_data": data_url(root, fname, mime)}}, text]
    elif modality == "audio":
        raw = base64.b64encode(fixture_bytes(root, fname)).decode()
        content = [text, {"type": "input_audio",
                          "input_audio": {"data": raw, "format": fname.rsplit(".", 1)[1]}}]
    else:  # video
        content = [{"type": "video_url", "video_url": {"url": data_url(root, fname, mime)}}, text]
    # Nothing beyond the content array. An earlier version also sent
    # modalities:["text"] so an audio-output model would put its answer in
    # `content` — and eleven models, the whole gpt-5.x and o-series set, answered
    # "Unknown parameter: 'modalities'" to the LIVENESS cell, which discarded
    # every one of their remaining cells. The parameter was belt-and-braces: the
    # reader already falls back to message.audio.transcript, so it bought nothing
    # and cost the measurement of eleven models.
    #
    # The rule it cost us: a probe adds nothing to the request that the request
    # does not need. Every field we send is a field a provider can refuse, and a
    # refusal we provoked is indistinguishable from a capability the model lacks.
    body = {"model": model, "messages": [{"role": "user", "content": content}]}
    if not lean:
        body["max_tokens"] = max_tokens
        body["stream"] = False
    return body


class Answer:
    """Everything about one call that a later triage could need.

    Deliberately more than the verdict. The refusal text alone cannot say whether
    a refusal was ours, and error.code, finish_reason and usage are unrecoverable
    once the run is over — which would mean re-paying for the whole sweep to
    answer a question the first run could have recorded for free.
    """

    def __init__(self, http=0, text="", err="", code="", etype="",
                 finish="", usage=None, transport="", lean_rescued=False):
        self.http, self.text, self.err = http, text, err
        self.code, self.etype, self.finish = code, etype, finish
        self.usage, self.transport = usage or {}, transport
        # Set when the FULL request was refused and a lean one carrying the same
        # attachment succeeded — the refusal was ours, not the wire's.
        self.lean_rescued = lean_rescued


def call(gw, vk, body, timeout, request_id):
    p = subprocess.run(
        ["curl", "-sS", "-X", "POST", gw + "/v1/chat/completions",
         "-H", "Authorization: Bearer " + vk, "-H", "content-type: application/json",
         # x-request-id, NOT x-nexus-request-id. The gateway stores the CLIENT's
         # id from x-request-id into traffic_event.external_request_id; the
         # x-nexus-request-id header is the gateway's own id and is only echoed
         # back. Sending the wrong one produced a column that was empty on every
         # row while the header came back looking correct — a join that would
         # have silently returned nothing, verified before it was relied on.
         "-H", "x-request-id: " + request_id,
         "-d", json.dumps(body), "-o", "-", "-w", "\n%{http_code}",
         "--max-time", str(timeout)],
        capture_output=True, text=True)
    if p.returncode != 0 and not p.stdout:
        # curl itself failed: no connection, DNS, timeout. Not a statement about
        # the model, and previously recorded as one with an empty reason string.
        return Answer(transport=(p.stderr or "curl exit %d" % p.returncode).strip()[:200])
    out = p.stdout.rsplit("\n", 1)
    if len(out) != 2:
        return Answer(transport="no response body")
    raw, code_s = out
    http = int(code_s) if code_s.isdigit() else 0
    try:
        d = json.loads(raw)
    except Exception:
        # An LB's HTML 502, or a proxy page. Not JSON, so not the API answering.
        return Answer(http=http, transport="non-JSON body: " + raw.strip()[:160])
    err = d.get("error")
    if isinstance(err, dict):
        # A 200 carrying an error object is an error, whatever the status said.
        return Answer(http=http if http != 200 else 400,
                      err=str(err.get("message") or "")[:2000],
                      code=str(err.get("code") or ""), etype=str(err.get("type") or ""))
    if http != 200:
        return Answer(http=http, err=raw.strip()[:2000])
    choice = (d.get("choices") or [{}])[0] or {}
    msg = choice.get("message") or {}
    text = msg.get("content")
    if not text:
        audio = msg.get("audio") or {}
        text = audio.get("transcript") or ""
    return Answer(http=200, text=text or "", finish=str(choice.get("finish_reason") or ""),
                  usage=d.get("usage") or {})



def anchor_hit(anchor, text):
    """Any one of the case's anchors counts as reading. A multi-frame video
    carries a different number per frame, and models legitimately sample
    different frames — the first single-anchor version marked three models
    ACCEPTED_NOT_READ for answering with a number that IS in the footage."""
    return any(re.search(r"\b" + re.escape(a) + r"\b", text, re.I)
               for a in str(anchor).split("|") if a)

def verdict(a, anchor):
    """Six outcomes. Which one is chosen decides what Phase C does next."""
    if a.transport or a.http in TRANSIENT:
        return "INFRA", ""
    if a.http != 200:
        if a.code in CATALOG_CODES:
            return "REFUSED", "CATALOG"
        if a.etype == "proxy_error" or a.err.startswith("nexus:"):
            return "REFUSED", "OURS"
        return "REFUSED", "VENDOR"
    if a.lean_rescued:
        # The full request was refused and the same attachment went through on a
        # lean one. The wire can do this; something WE sent stopped it.
        return "READ" if (anchor and anchor_hit(anchor, a.text)) \
            else "ACCEPTED_NOT_READ", "OURS_PARAM"
    if not a.text.strip():
        # Empty content is about the budget or the response shape, not the
        # attachment. 61 of the catalog's chat models reason unconditionally and
        # would otherwise fill the ACCEPTED_NOT_READ list on their own.
        return ("TRUNCATED" if a.finish == "length" else "EMPTY"), ""
    if anchor is None:
        return "ACCEPTED", ""
    if anchor_hit(anchor, a.text):
        return "READ", ""
    return "ACCEPTED_NOT_READ", ""


SUCCEEDED = ("READ", "ACCEPTED", "ACCEPTED_NOT_READ", "TRUNCATED", "EMPTY")


def schedule(model, done, image_url, optimistic=False):
    """The cells this model would run, given what is already recorded.

    Separate from the sweep so --dry-run can price the run without placing a
    call — a sweep with no ceiling and no preview scales with whatever
    /v1/models happens to return.

    `optimistic` decides how an UNRUN prerequisite is treated, and the two
    answers are the two ends of the price. Pessimistically a gated cell never
    becomes eligible, which is the floor; optimistically every prerequisite
    succeeds and the whole format matrix opens up, which is the ceiling. Pricing
    only the floor is worse than not pricing at all — it is the number that
    would let the run quietly cost several times what was approved.
    """
    settled, todo = {}, []
    for case in CASES:
        cid, _mod, _f, _m, _a, needs = case
        if (model["id"], cid) in done:
            settled[cid] = done[(model["id"], cid)]["verdict"]
            continue
        if cid == "image_url_https" and not image_url:
            settled[cid] = "SKIPPED"
            continue
        if needs and settled.get(needs) not in SUCCEEDED:
            if not (optimistic and settled.get(needs) == "?"):
                settled[cid] = "SKIPPED"
                continue
        if cid != "text" and settled.get(liveness_case(model)) == "REFUSED":
            settled[cid] = "SKIPPED"
            continue
        todo.append(case)
        settled[cid] = "?"
    return todo


def liveness_case(model):
    """Which cell decides whether this model can answer at all.

    Not always the text cell. A model with a non-empty requiredModalities floor
    refuses a text-only request BY DESIGN — gpt-audio-mini is type=chat and still
    will not serve plain text — so using text as the liveness probe discards
    every audio-first model before its audio cell is ever reached, which is
    exactly the population the audio row exists to characterise.
    """
    req = [m.lower() for m in (model.get("requiredModalities") or [])]
    if "audio" in req:
        return "audio_wav"
    if "image" in req:
        return "image_inline_png"
    return "text"


def load_done(path, current_fp=None):
    """Rows keyed by (model, case), discarding any produced by a DIFFERENT
    definition of that case.

    Without this a resumed run blends rows from the old instrument with rows
    from the new one into a single report that describes neither — and every
    repair to this harness changes a fixture, a media type, an anchor or the
    token budget. Dropped rows are counted and printed rather than silently
    discarded: a bound on what was reused is still a bound.

    Rows with no fingerprint at all (skips, controls) are kept, because their
    verdict does not depend on the fixture bytes.
    """
    done, header, stale = {}, None, {}
    if not os.path.exists(path):
        return done, header
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        r = json.loads(line)
        if r.get("record") == "run-header":
            header = r
            continue
        if current_fp and r.get("modality") == "control" \
                and r.get("control_shape") != REQUEST_SHAPE:
            stale[r["case"]] = stale.get(r["case"], 0) + 1
            continue
        fp = r.get("fp")
        if current_fp and fp and current_fp.get(r["case"]) not in (None, fp):
            stale[r["case"]] = stale.get(r["case"], 0) + 1
            continue
        done[(r["model"], r["case"])] = r
    if stale:
        print("discarding rows measured with an older definition of the case, they will re-run:")
        for case, n in sorted(stale.items()):
            print(f"  {case:<22} {n}")
    return done, header


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="NDJSON results file; the run resumes from it")
    ap.add_argument("--gw", default=os.environ.get("NEXUS_AI_GW_URL", ""))
    ap.add_argument("--vk-file", default=os.environ.get("NEXUS_VK_FILE", ""))
    ap.add_argument("--models", default="", help="comma-separated subset; default is every chat model")
    ap.add_argument("--root", default=".", help="repo root, for locating fixtures")
    ap.add_argument("--timeout", type=int, default=120)
    ap.add_argument("--max-tokens", type=int, default=1024,
                    help="generous on purpose: a reasoning model spends its budget on reasoning "
                         "before emitting any content, and an empty answer is indistinguishable "
                         "from a dropped attachment")
    ap.add_argument("--image-url", default=os.environ.get(IMAGE_URL_ENV, ""),
                    help="a reachable URL serving ocr-19452.png; without it the URL cell is skipped")
    ap.add_argument("--max-calls", type=int, default=1200,
                    help="abort rather than overspend if the catalog is bigger than expected")
    ap.add_argument("--dry-run", action="store_true", help="price the run and exit")
    ap.add_argument("--report", action="store_true", help="summarise an existing file and exit")
    ap.add_argument("--baseline", default="",
                    help="an earlier results file to diff against; a cell that regressed, or one "
                         "our catalog now refuses that the baseline measured as READ, is reported "
                         "as a disagreement rather than lost")
    ap.add_argument("--no-auto", action="store_true",
                    help="skip the model:auto pass; it is the path the product uses by default")
    ap.add_argument("--run-id", default="mmx")
    args = ap.parse_args()

    current_fp = {c[0]: case_fingerprint(c, args.root, args.max_tokens, args.image_url)
                  for c in CASES}
    done, _header = load_done(args.out, current_fp)
    # NOT `base`: the sweep loop below reuses that name for the per-row record
    # template, so passing it to report() handed the diff a single row as its
    # baseline. Every lookup missed, every row was skipped, and the section
    # printed 0 regressed / 0 recovered over a pair of runs with 125 real
    # differences — the exact number the exit condition turns on.
    baseline_rows = load_done(args.baseline)[0] if args.baseline else {}
    if args.report:
        report(done, _header, baseline_rows)
        return

    if not args.gw or not args.vk_file:
        sys.exit("--gw and --vk-file are required (or NEXUS_AI_GW_URL / NEXUS_VK_FILE)")
    vk = open(args.vk_file).read().strip()

    p = subprocess.run(["curl", "-sS", args.gw + "/v1/models",
                        "-H", "Authorization: Bearer " + vk], capture_output=True, text=True)
    try:
        catalog = json.loads(p.stdout).get("data", [])
    except Exception:
        sys.exit("could not read " + args.gw + "/v1/models — " + (p.stdout or p.stderr)[:300])
    if not catalog:
        sys.exit("/v1/models returned no models; a sweep over nothing would report clean")

    # Chat-SERVED, which is not the same as type == "chat". The `audio` type is
    # deprecated — it was minted for any id containing the word, and the models
    # that carry it are served on chat completions. Filtering on type alone
    # drops them silently, and they are the models the audio row exists for.
    wanted = [m.strip() for m in args.models.split(",") if m.strip()]
    models = [m for m in catalog
              if ((m["id"] in wanted) if wanted else (m.get("type") in ("chat", "audio")))]
    if wanted:
        missing = sorted(set(wanted) - {m["id"] for m in models})
        if missing:
            sys.exit("not in the catalog: " + ", ".join(missing))
    if not args.no_auto and not wanted:
        models.append(AUTO_MODEL)
    if not args.image_url:
        print("NOTE: no --image-url, so the URL-carrier cell is skipped for every model. "
              "Serve tests/fixtures/media/ocr-19452.png somewhere reachable to measure it.")

    planned = {m["id"]: schedule(m, done, args.image_url) for m in models}
    ceiling = {m["id"]: schedule(m, done, args.image_url, optimistic=True) for m in models}
    floor_n = sum(len(v) for v in planned.values())
    max_n = sum(len(v) for v in ceiling.values())
    # Controls are one per modality a model actually reads, so at most one per
    # anchored modality present in the matrix.
    ctrl_max = len(models) * len({c[1] for c in CASES if c[4]})
    print(f"catalog={len(catalog)} sweeping={len(models)} "
          f"{'(explicit subset)' if wanted else 'chat-served models'} "
          f"cells_already_recorded={len(done)} "
          f"calls={floor_n}..{max_n} (+ up to {ctrl_max} controls)")
    if args.dry_run:
        for mid, cells in sorted(ceiling.items()):
            if cells:
                print(f"  {mid:<36} {len(cells):>2}  {','.join(c[0] for c in cells)}")
        print(f"\nfloor {floor_n} if every gated cell stays gated, "
              f"ceiling {max_n} if every prerequisite succeeds, "
              f"plus up to {ctrl_max} controls.")
        return
    if max_n + ctrl_max > args.max_calls:
        sys.exit(f"up to {max_n + ctrl_max} calls exceeds --max-calls {args.max_calls}; "
                 "raise it deliberately")

    out = open(args.out, "a")
    out.write(json.dumps({
        "record": "run-header", "run_id": args.run_id, "max_tokens": args.max_tokens,
        "image_url": args.image_url,
        "in_scope": [m["id"] for m in models],
        "fingerprints": {c[0]: case_fingerprint(c, args.root, args.max_tokens, args.image_url)
                         for c in CASES},
    }) + "\n")
    out.flush()

    for m in models:
        mid = m["id"]
        base = {"model": mid, "declared": m.get("inputModalities") or [],
                "required": m.get("requiredModalities") or [],
                "owned_by": m.get("owned_by") or "", "model_type": m.get("type") or ""}
        settled = {}
        live_case = liveness_case(m)
        for case in CASES:
            cid, modality, _f, _mime, anchor, needs = case
            if (mid, cid) in done:
                settled[cid] = done[(mid, cid)]["verdict"]
                continue

            # Every skip is written down. An unmeasured cell that leaves no row
            # is indistinguishable in the report from one that was never
            # scheduled, and a missing row reads as absence of a problem rather
            # than absence of a measurement.
            skip = ""
            if cid == "image_url_https" and not args.image_url:
                skip = "no --image-url configured; the cell cannot produce evidence"
            elif needs and settled.get(needs) not in SUCCEEDED:
                skip = f"gated on {needs} ({settled.get(needs)})"
            elif cid != "text" and settled.get(live_case) == "REFUSED":
                skip = f"the liveness cell {live_case} was refused"
            if skip:
                rec = dict(base, case=cid, modality=modality, verdict="SKIPPED",
                           whose="", answer="", reason=skip,
                           fp=case_fingerprint(case, args.root, args.max_tokens, args.image_url))
                out.write(json.dumps(rec) + "\n"); out.flush()
                settled[cid] = "SKIPPED"
                continue

            # TRUNCATED is a statement about the budget, not the attachment, so
            # it is not an answer — it is a reason to ask again with room. A
            # reasoning model can spend thousands of tokens before emitting a
            # visible character, and a fixed ceiling will always be too small
            # for something. Escalate until the model has room or the ceiling
            # stops being credible.
            budget = args.max_tokens
            for attempt in range(4):
                a = call(args.gw, vk, build(mid, case, args.root, budget, args.image_url),
                         args.timeout, f"{args.run_id}-{mid}-{cid}")
                v, whose = verdict(a, anchor)
                if v != "TRUNCATED" or attempt == 3:
                    break
                budget *= 4
                print(f"  {mid:<34} {cid:<20} TRUNCATED at {budget // 4}, retrying at {budget}")

            # A vendor refusal is not accepted as a capability fact until a LEAN
            # request has been refused too. Twice in this programme a field we
            # added turned a working model into an apparently incapable one — an
            # unsupported `modalities` parameter took out eleven models, and a
            # small token budget took out every reasoning model. Recording either
            # as "this wire cannot" would have been our defect written down as
            # the vendor's limit.
            if v == "REFUSED" and whose == "VENDOR":
                lean = call(args.gw, vk,
                            build(mid, case, args.root, budget, args.image_url, lean=True),
                            args.timeout, f"{args.run_id}-{mid}-{cid}-lean")
                if lean.http == 200:
                    lean.lean_rescued = True
                    a = lean
                    v, whose = verdict(a, anchor)
                    print(f"  {mid:<34} {cid:<20} the refusal was OURS — a lean request "
                          f"carried the same attachment")
            print(f"  {mid:<34} {cid:<20} {v:<18} {whose:<8} "
                  f"{(a.transport or a.err or a.text or '')[:46]}")
            if v == "INFRA":
                # Deliberately NOT persisted. A transient blip written as a
                # settled verdict is a permanent capability fact the resume can
                # never revisit, and it gates every dependent cell off with it.
                settled[cid] = "INFRA"
                time.sleep(1.0)
                continue
            # The fingerprint uses the BASE budget, not the escalated one: the
            # case definition is what the resume compares, and a cell that
            # needed more room is still the same cell. max_tokens_used records
            # what it actually took, which is the interesting number.
            rec = dict(base, case=cid, modality=modality, http=a.http, verdict=v, whose=whose,
                       error_code=a.code, error_type=a.etype, finish_reason=a.finish,
                       usage=a.usage, max_tokens_used=budget,
                       answer=(a.text or "")[:160], reason=(a.err or a.transport)[:600],
                       fp=case_fingerprint(case, args.root, args.max_tokens, args.image_url))
            out.write(json.dumps(rec) + "\n"); out.flush()
            settled[cid] = v
            time.sleep(0.2)

        # One control per modality the model appeared to read.
        read_anchors = {}
        for c_id, c_mod, _cf, _cm, c_anchor, _cn in CASES:
            if c_anchor and settled.get(c_id) == "READ":
                read_anchors.setdefault(c_mod, set()).add(c_anchor)
        for c_mod, anchors in sorted(read_anchors.items()):
            ctrl = "negative_control_" + c_mod
            if (mid, ctrl) in done:
                continue
            a = call(args.gw, vk,
                     {"model": mid, "max_tokens": args.max_tokens, "stream": False,
                      "modalities": ["text"],
                      "messages": [{"role": "user", "content": PROMPTS[c_mod]}]},
                     args.timeout, f"{args.run_id}-{mid}-{ctrl}")
            leaked = [x for x in sorted(anchors)
                      if re.search(r"\b" + re.escape(x) + r"\b", a.text, re.I)]
            if a.http != 200:
                # A control that did not get an answer has not shown the absence
                # of a leak. Recording it clean would rest every READ for this
                # modality on a check that never ran.
                v = "CONTROL_INCONCLUSIVE"
            else:
                v = "CONTROL_LEAKED" if leaked else "CONTROL_CLEAN"
            rec = dict(base, case=ctrl, modality="control", http=a.http, verdict=v, whose="",
                       control_shape=REQUEST_SHAPE, answer=(a.text or "")[:160],
                       reason=("leaked " + ",".join(leaked)) if leaked else (a.err or a.transport)[:600])
            out.write(json.dumps(rec) + "\n"); out.flush()
            if v != "CONTROL_CLEAN":
                print(f"  {mid:<34} {ctrl:<20} {v} — {c_mod} READ verdicts do not stand")
    out.close()
    done, header = load_done(args.out, current_fp)
    report(done, header, baseline_rows)


def near_miss(answer, anchor, control_answer):
    """The model produced the anchor ALMOST — and did not when nothing was sent.

    Reclassified at report time rather than in verdict(), because it needs the
    control, and the control runs after the cells.

    Why it has to exist: with a poor fixture, models answered 19432, 19472,
    19352 to an anchor of 19452 and answered 42, 3, 880 to the same question
    with nothing attached. They had the image. Filing that under "accepted but
    did not use the attachment" points an investigation at a codec that is
    working. The discriminator is the control: a number this close, from a model
    whose control produced nothing close, came from the pixels.

    Deliberately narrow — same length, one digit different. Anything looser
    stops being evidence and starts being hope.
    """
    if not anchor or not anchor.isdigit():
        return False
    for cand in re.findall(r"\b\d{%d}\b" % len(anchor), answer or ""):
        if sum(a != b for a, b in zip(cand, anchor)) != 1:
            continue
        if any(sum(a != b for a, b in zip(c2, anchor)) <= 1
               for c2 in re.findall(r"\b\d{%d}\b" % len(anchor), control_answer or "")):
            return False  # the control produced one too; this proves nothing
        return True
    return False


def report(done, header=None, baseline=None):
    """Seven lists, because seven different decisions rest on them.

    `baseline` is an earlier run to diff against, and it is what keeps this
    instrument usable after the catalog is corrected. Once a row declares a
    modality, our own routing guard begins refusing that modality for every row
    that does NOT declare it — correctly, and the cell stops being a wire
    measurement. Re-deriving capability from scratch on every run would need the
    catalog to stay wrong forever. Diffing against what was measured when it
    could be measured does not.

    Two disagreements matter and they are opposite:
      · the baseline READ it and our catalog now refuses it — we took away a
        capability the model has
      · the baseline refused it and it now reads — a fix landed, or a wire
        changed under us
    """
    # A modality whose control leaked or never answered has not been shown to be
    # read. Void here, before any list can quietly rest on one.
    voided = {(r["model"], r["case"].replace("negative_control_", ""))
              for r in done.values()
              if r["verdict"] in ("CONTROL_LEAKED", "CONTROL_INCONCLUSIVE")}

    anchors = {c[0]: c[4] for c in CASES}
    controls = {(r["model"], r["case"].replace("negative_control_", "")): r.get("answer", "")
                for r in done.values() if r.get("modality") == "control"}

    by_case, rows = {}, []
    for (_model, case), r in sorted(done.items()):
        v, mod = r["verdict"], r.get("modality")
        if v == "READ" and (r["model"], mod) in voided:
            v = "READ_UNVALIDATED"
        if v == "ACCEPTED_NOT_READ" and near_miss(
                r.get("answer"), anchors.get(case), controls.get((r["model"], mod))):
            v = "NEAR_MISS"
        r = dict(r, verdict=v)
        by_case.setdefault(case, []).append(r)
        rows.append(r)

    understate, overstate, vendor_leaks, routed, misread = [], [], [], [], []
    not_read, unproven, unmeasured = [], [], []
    worklist = {}
    for r in rows:
        v, mod = r["verdict"], r.get("modality")
        model, case = r["model"], r["case"]
        decl = [d.lower() for d in (r.get("declared") or [])]
        if mod in ("text", "control", None):
            continue
        if model == AUTO_MODEL["id"]:
            # A routing outcome. It belongs in its own section: there is no row
            # to correct, and folding it into the catalog lists would name the
            # router as a model that understates itself.
            routed.append(f"{case}: {v}"
                          + (f" — {(r.get('reason') or '')[:70]}" if r.get("reason") else ""))
            continue
        # Only a READ proves the model used the attachment.
        if v == "READ" and mod not in decl:
            understate.append(f"{model} reads {case} but does not declare {mod}")
        # The direction the goal names first, and the more dangerous one:
        # routing acts on an overstatement.
        if mod in decl and v in ("REFUSED", "ACCEPTED_NOT_READ") and r.get("whose") != "CATALOG":
            overstate.append(f"{model} declares {mod} but {case} came back {v}"
                             f" — {(r.get('reason') or '')[:70]}")
        if v == "REFUSED" and r.get("whose") == "VENDOR":
            vendor_leaks.append(f"{model} {case}: {(r.get('reason') or '')[:90]}")
            key = (r.get("owned_by") or "?", mod, case)
            worklist.setdefault(key, (r.get("reason") or "")[:120])
        if v == "REFUSED" and r.get("whose") == "CATALOG":
            unmeasured.append(f"{model} {case}: our routing guard answered, from the row under test")
        if v == "ACCEPTED_NOT_READ":
            not_read.append(f"{model} {case}: answered {r.get('answer', '')[:50]!r}")
        if v == "NEAR_MISS":
            misread.append(f"{model} {case}: answered {r.get('answer', '')[:24]!r} for {anchors.get(case)}"
                           f" — control said {controls.get((model, mod), '')[:20]!r}")
        if v == "ACCEPTED":
            unproven.append(f"{model} {case}: 200, but this cell has no anchor to check")

    # The catalog-versus-baseline disagreements, which are the whole point of
    # keeping an earlier run.
    if baseline:
        regressed, recovered = [], []
        for (model, case), r in sorted(done.items()):
            b = baseline.get((model, case))
            if not b or r.get("modality") in ("text", "control", None):
                continue
            was, now = b["verdict"], r["verdict"]
            if was == "READ" and now == "REFUSED" and r.get("whose") == "CATALOG":
                regressed.append(f"{model} {case}: the wire READ this and our catalog now "
                                 f"refuses it")
            elif was == "READ" and now in ("REFUSED", "ACCEPTED_NOT_READ"):
                regressed.append(f"{model} {case}: READ -> {now}"
                                 f" ({(r.get('reason') or '')[:60]})")
            elif was in ("REFUSED", "ACCEPTED_NOT_READ") and now == "READ":
                recovered.append(f"{model} {case}: {was} -> READ")
        print(f"\n=== REGRESSED against the baseline: {len(regressed)} ===")
        for line in regressed[:40]:
            print("  " + line)
        print(f"\n=== RECOVERED against the baseline: {len(recovered)} ===")
        for line in recovered[:40]:
            print("  " + line)

    print("\n=== per case ===")
    for case, rs in by_case.items():
        counts = {}
        for r in rs:
            counts[r["verdict"]] = counts.get(r["verdict"], 0) + 1
        print(f"  {case:<22} " + "  ".join(f"{k}={v}" for k, v in sorted(counts.items())))

    # Coverage, stated rather than implied. A run in which a dozen models died
    # produces a report that otherwise looks complete.
    if header and header.get("in_scope"):
        scope = set(header["in_scope"])
        measured = {r["model"] for r in rows} | {m for m, _c in done}
        missing = sorted(scope - measured)
        print(f"\n=== coverage: {len(scope) - len(missing)} of {len(scope)} models in scope ===")
        for mid in missing[:20]:
            print(f"  NO ROWS: {mid}")

    if voided:
        print(f"\n=== CONTROLS THAT DID NOT HOLD — these READ verdicts do not stand: {len(voided)} ===")
        for model, mod in sorted(voided):
            print(f"  {model} / {mod}")

    if routed:
        print(f"\n=== model:auto — the path the product uses, and nobody chooses the model: "
              f"{len(routed)} ===")
        for line in routed:
            print("  " + line)

    for title, items in (
        ("CATALOG UNDERSTATES — the model reads it and the catalog does not say so", understate),
        ("CATALOG OVERSTATES — the catalog says yes and the wire did not deliver", overstate),
        ("VENDOR REFUSAL REACHED THE CALLER — each one is a message we should own", vendor_leaks),
        ("ACCEPTED BUT NOT READ — a 200 that did not use the attachment", not_read),
        ("UNPROVEN — 200 on a cell with no anchor; acceptance is not evidence of reading", unproven),
        ("UNMEASURED — our own guard answered, so the wire was never asked", unmeasured),
    ):
        print(f"\n=== {title}: {len(items)} ===")
        for line in items[:40]:
            print("  " + line)
        if len(items) > 40:
            print(f"  ... and {len(items) - 40} more")

    # The probe-3 checklist. A direct probe with the vendor's own documented
    # shape is the only one that distinguishes "the wire cannot" from "we
    # spelled it wrong", and it stays manual — it cannot use a virtual key, and
    # a hand-rolled vendor body that is itself misspelled produces the most
    # expensive wrong answer available. Manual work that is merely remembered
    # does not happen, so it is enumerated here instead: finite, deduplicated by
    # provider rather than by model, and shrinking.
    print(f"\n=== PROBE-3 WORKLIST — hand-probe these (provider, modality, case): {len(worklist)} ===")
    for (owner, mod, case), reason in sorted(worklist.items()):
        print(f"  {owner:<24} {mod:<7} {case:<20} {reason}")


if __name__ == "__main__":
    main()
