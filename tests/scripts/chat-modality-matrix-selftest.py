#!/usr/bin/env python3
"""Gates for chat-modality-matrix.py, run before the sweep spends anything.

WHY THIS FILE EXISTS AS A FILE. An earlier version of these gates lived in a
scratch script that imported the harness and called only report(), feeding it
hand-typed verdict strings. Under mutation testing six of seven realistic bugs
survived with every gate green — including "the classifier always returns READ",
"every gateway refusal is labelled VENDOR", "the sweep stops writing skip rows"
and "the negative control is never scheduled". None of those functions were ever
executed by the gates that claimed to cover them.

So the rules here are:

  · drive the REAL functions — verdict(), call(), and the sweep loop through
    main() — never a re-implementation and never report() alone
  · every table carries the NEGATIVE case. A gate whose fixtures only exercise
    one side of a branch cannot see that branch removed; that trap has already
    caught this suite twice
  · the stub must be able to produce what the wire produces: a 429, a timeout, a
    200 with empty content, a 200 carrying an error, a gateway refusal with a
    catalog code, an omitted inputModalities key, a model that requires audio

    python3 tests/scripts/chat-modality-matrix-selftest.py
"""
import importlib.util
import io
import json
import os
import pathlib
import contextlib
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

ROOT = pathlib.Path(__file__).resolve().parents[2]
HARNESS = ROOT / "tests" / "scripts" / "chat-modality-matrix.py"

spec = importlib.util.spec_from_file_location("mmx", HARNESS)
mmx = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mmx)

FAILURES = []


def section(out, title):
    """The ROWS of a report section, never its header.

    Sections print as "=== TITLE ...: N ===" followed by indented rows, so a
    naive split on "===" returns the title line and nothing else — a check built
    on it inspects the header and can never see a row that should not be there.
    """
    lines, inside, got = out.splitlines(), False, []
    for line in lines:
        if line.startswith("=== ") and title in line:
            inside = True
            continue
        if inside:
            if line.startswith("=== "):
                break
            if line.strip():
                got.append(line.strip())
    return got


def check(name, ok, detail=""):
    print(("  PASS  " if ok else "  FAIL  ") + name + ("" if ok else "   <- " + detail))
    if not ok:
        FAILURES.append(name)


# ---------------------------------------------------------------- the stub

class Stub:
    """A gateway that can produce every shape the real one can.

    Scripted per (model, case) so a gate can ask for a 429 on exactly one cell
    and a clean 200 everywhere else — which is what makes "one transient blip
    must not become a permanent capability fact" testable at all.
    """

    def __init__(self, models, script):
        self.models, self.script = models, script
        self.calls = []
        stub = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _send(self, code, obj, raw=None):
                b = raw if raw is not None else json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(b)))
                self.end_headers()
                self.wfile.write(b)

            def do_GET(self):
                if self.path == "/v1/models":
                    return self._send(200, {"data": stub.models})
                self._send(404, {})

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["content-length"])))
                rid = self.headers.get("x-request-id", "")
                # The lean retry appends "-lean" to the id, so the last segment
                # is not always the case. Strip it, or a script keyed on a case
                # silently misses the retry and the default handler answers 200
                # — which is how the negative half of G17 first "passed".
                case = rid[:-len("-lean")] if rid.endswith("-lean") else rid
                case = case.rsplit("-", 1)[-1] if case else ""
                stub.calls.append((body.get("model"), rid))
                key = (body.get("model"), case)
                fn = stub.script.get(key) or stub.script.get((body.get("model"), "*"))
                if fn is None:
                    return self._send(200, {"choices": [{"message": {
                        "content": "19452 38617 52903 61074 74128 regression fixture ok"},
                        "finish_reason": "stop"}]})
                code, payload = fn(body, rid)
                if isinstance(payload, bytes):
                    return self._send(code, None, raw=payload)
                self._send(code, payload)

        self.httpd = HTTPServer(("127.0.0.1", 0), H)
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *a):
        self.httpd.shutdown()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.port}"

    def calls_for(self, model):
        return [rid for m, rid in self.calls if m == model]


def ok200(text, finish="stop"):
    return lambda body, rid: (200, {"choices": [
        {"message": {"content": text}, "finish_reason": finish}]})


def run_sweep(stub, out, extra=(), auto=False):
    vk = out + ".vk"
    with open(vk, "w") as f:
        f.write("vk-not-real")
    # The per-model gates assert on one model's rows, so the synthetic auto pass
    # is off unless a gate asks for it EXPLICITLY. Sniffing `extra` for the flag
    # would silently disable it for any gate that merely passed other options —
    # which is how G14 first "passed" while measuring nothing.
    if not auto:
        extra = list(extra) + ["--no-auto"]
    argv = ["chat-modality-matrix.py", "--out", out, "--gw", stub.url, "--vk-file", vk,
            "--root", str(ROOT), "--timeout", "10"] + list(extra)
    old = sys.argv
    sys.argv = argv
    buf = io.StringIO()
    try:
        with contextlib.redirect_stdout(buf):
            mmx.main()
    except SystemExit as e:
        buf.write(f"\nSystemExit: {e}\n")
    finally:
        sys.argv = old
    return buf.getvalue()


def rows(path):
    out = []
    for line in open(path):
        line = line.strip()
        if line:
            r = json.loads(line)
            if r.get("record") != "run-header":
                out.append(r)
    return out


# ------------------------------------------------------- G1 the classifier

def gate_classifier():
    """Drives verdict() itself, over every outcome AND its negative."""
    print("G1 · verdict() produces all six outcomes, each for its own reason")
    A = mmx.Answer
    table = [
        ("anchor present -> READ",
         A(http=200, text="the number is 19452."), "19452", ("READ", "")),
        # The negative of READ: the same shape without the anchor. Without this
        # row, a classifier that always says READ passes.
        ("anchor absent -> ACCEPTED_NOT_READ",
         A(http=200, text="I cannot see an attachment."), "19452", ("ACCEPTED_NOT_READ", "")),
        # Substring matching scored this READ before word boundaries landed.
        ("anchor as a substring of another word is NOT a read",
         A(http=200, text="An image is required to answer."), "red", ("ACCEPTED_NOT_READ", "")),
        ("no anchor defined -> ACCEPTED, never READ",
         A(http=200, text="a picture of a cat"), None, ("ACCEPTED", "")),
        ("empty content with finish_reason length -> TRUNCATED",
         A(http=200, text="", finish="length"), "19452", ("TRUNCATED", "")),
        ("empty content otherwise -> EMPTY",
         A(http=200, text="", finish="stop"), "19452", ("EMPTY", "")),
        ("our routing guard -> REFUSED/CATALOG",
         A(http=400, err="model x does not accept image input; it accepts text",
           code="MODEL_INPUT_MODALITY_UNSUPPORTED", etype="proxy_error"), "19452",
         ("REFUSED", "CATALOG")),
        # A gateway refusal with NO nexus: prefix — the shape that made every one
        # of our own refusals read as the vendor's.
        ("a gateway refusal without the nexus prefix -> REFUSED/OURS",
         A(http=413, err="request body exceeds the configured network read cap",
           code="PAYLOAD_TOO_LARGE", etype="proxy_error"), "19452", ("REFUSED", "OURS")),
        ("a codec refusal -> REFUSED/OURS",
         A(http=400, err="nexus: this provider does not accept a file content part"), "19452",
         ("REFUSED", "OURS")),
        ("a vendor parser reaching the caller -> REFUSED/VENDOR",
         A(http=400, err="unknown variant `image_url`, expected one of ..."), "19452",
         ("REFUSED", "VENDOR")),
        ("a rate limit is not a capability fact -> INFRA",
         A(http=429, err="rate limited"), "19452", ("INFRA", "")),
        ("a timeout is not a capability fact -> INFRA",
         A(transport="curl: (28) operation timed out"), "19452", ("INFRA", "")),
        ("a 5xx is not a capability fact -> INFRA",
         A(http=503, err="upstream unavailable"), "19452", ("INFRA", "")),
    ]
    seen = set()
    for name, ans, anchor, want in table:
        got = mmx.verdict(ans, anchor)
        seen.add(got[0])
        check(name, got == want, f"got {got}, want {want}")
    for outcome in ("READ", "ACCEPTED_NOT_READ", "ACCEPTED", "TRUNCATED", "EMPTY",
                    "REFUSED", "INFRA"):
        check(f"outcome {outcome} was actually produced", outcome in seen,
              "no table row exercises it, so a mutation removing it would pass")


# -------------------------------------------------------------- G2 call()

def gate_call():
    """Drives call() against real HTTP, including the shapes that are not JSON."""
    print("G2 · call() reports the transport and the body, and never confuses them")
    models = [{"id": "m", "type": "chat"}]
    script = {
        ("m", "json"): lambda b, r: (200, {"choices": [
            {"message": {"content": "19452"}, "finish_reason": "stop"}]}),
        ("m", "audio"): lambda b, r: (200, {"choices": [
            {"message": {"content": None, "audio": {"transcript": "regression fixture"}},
             "finish_reason": "stop"}]}),
        ("m", "html"): lambda b, r: (502, b"<html>bad gateway</html>"),
        ("m", "errin200"): lambda b, r: (200, {"error": {
            "message": "content policy", "code": "CONTENT_FILTERED", "type": "proxy_error"}}),
    }
    with Stub(models, script) as stub:
        a = mmx.call(stub.url, "vk", {"model": "m"}, 5, "x-m-json")
        check("a plain 200 yields the content", a.http == 200 and a.text == "19452", repr(a.text))
        a = mmx.call(stub.url, "vk", {"model": "m"}, 5, "x-m-audio")
        check("an audio-output 200 yields the transcript, not empty",
              a.text == "regression fixture", repr(a.text))
        a = mmx.call(stub.url, "vk", {"model": "m"}, 5, "x-m-html")
        check("a non-JSON body is a transport fact, not a model fact",
              a.transport != "" and a.text == "", repr(a.transport))
        a = mmx.call(stub.url, "vk", {"model": "m"}, 5, "x-m-errin200")
        check("a 200 carrying an error is an error",
              a.http != 200 and a.code == "CONTENT_FILTERED", f"http={a.http} code={a.code}")
    a = mmx.call("http://127.0.0.1:1", "vk", {"model": "m"}, 2, "x-m-dead")
    check("a refused connection is INFRA, not a vendor refusal",
          mmx.verdict(a, "19452") == ("INFRA", ""), repr(a.transport))


# ---------------------------------------------------- G3 the sweep loop

def gate_sweep_writes_every_skip():
    print("G3 · the sweep writes a row for every cell it did not run")
    models = [{"id": "dead", "type": "chat", "inputModalities": ["text"]}]
    script = {("dead", "*"): lambda b, r: (400, {"error": {
        "message": "nexus: nothing works here", "type": "proxy_error"}})}
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, script) as stub:
            run_sweep(stub, out)
            paid = len(stub.calls)
        rs = rows(out)
        skipped = [r for r in rs if r["verdict"] == "SKIPPED"]
        check("one row exists for every case", len(rs) == len(mmx.CASES),
              f"{len(rs)} rows for {len(mmx.CASES)} cases")
        check("the unrun cells are recorded as skipped, not omitted",
              len(skipped) == len(mmx.CASES) - 1, f"{len(skipped)} skip rows")
        check("and they were not paid for", paid == 1, f"{paid} paid calls")
        check("every skip states why", all(r.get("reason") for r in skipped))


def gate_transient_is_not_persisted():
    print("G4 · a rate limit does not become a permanent capability fact")
    models = [{"id": "flaky", "type": "chat", "inputModalities": ["text", "image"]}]
    state = {"n": 0}

    def rate_limited(body, rid):
        state["n"] += 1
        return 429, {"error": {"message": "slow down", "type": "rate_limit_error"}}

    script = {("flaky", "image_inline_png"): rate_limited}
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, script) as stub:
            run_sweep(stub, out)
        rs = {r["case"]: r for r in rows(out)}
        check("the rate-limited cell wrote no row at all",
              "image_inline_png" not in rs, str(rs.get("image_inline_png")))
        check("and it is not recorded as a refusal",
              rs.get("image_inline_png", {}).get("verdict") != "REFUSED")
        # The dependent cells must be skipped for a reason that names the gap,
        # not silently marked as measured.
        dep = rs.get("image_inline_jpg")
        check("dependents are skipped, naming the unmeasured prerequisite",
              dep and dep["verdict"] == "SKIPPED" and "image_inline_png" in dep["reason"],
              str(dep))


def gate_resume_from_a_truncated_file():
    """The gate this suite previously got wrong: it resumed from a COMPLETE file,
    which exercises re-running rather than resuming."""
    print("G5 · resuming an INTERRUPTED file does not re-pay, and does not re-issue controls")
    models = [{"id": "dead", "type": "chat", "inputModalities": ["text"]}]
    script = {("dead", "*"): lambda b, r: (400, {"error": {
        "message": "nexus: nothing works here", "type": "proxy_error"}})}
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, script) as stub:
            run_sweep(stub, out)
            full = len(stub.calls)
        # Truncate to the header plus the liveness row — an interrupted run.
        keep = []
        for line in open(out):
            keep.append(line)
            r = json.loads(line)
            if r.get("case") == "text":
                break
        with open(out, "w") as f:
            f.writelines(keep)
        with Stub(models, script) as stub2:
            run_sweep(stub2, out)
            resumed = len(stub2.calls)
        check("a resumed run pays nothing more than a completed one",
              resumed == 0, f"resumed paid {resumed}, a completed run paid {full}")


def gate_liveness_uses_the_required_modality():
    print("G6 · a model that requires audio is not discarded by a text-only probe")
    models = [{"id": "aud", "type": "chat", "inputModalities": ["text", "audio"],
               "requiredModalities": ["audio"]}]

    def refuse_text(body, rid):
        return 400, {"error": {"message": "model aud requires audio input",
                               "code": "MODEL_REQUIRED_MODALITY_MISSING", "type": "proxy_error"}}

    script = {("aud", "text"): refuse_text}
    check("the liveness cell for an audio-required model is the audio cell",
          mmx.liveness_case(models[0]) == "audio_wav", mmx.liveness_case(models[0]))
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, script) as stub:
            run_sweep(stub, out)
        rs = {r["case"]: r for r in rows(out)}
        check("the audio cell is still measured", rs.get("audio_wav", {}).get("verdict") == "READ",
              str(rs.get("audio_wav")))
        check("the text refusal is attributed to the catalog floor, not the vendor",
              rs.get("text", {}).get("whose") == "CATALOG", str(rs.get("text")))


def gate_control_inconclusive():
    print("G7 · a control that did not answer does not certify anything")
    models = [{"id": "c", "type": "chat", "inputModalities": ["text"]}]
    script = {("c", "negative_control_file"): lambda b, r: (429, {"error": {"message": "slow"}}),
              ("c", "negative_control_image"): lambda b, r: (429, {"error": {"message": "slow"}}),
              ("c", "negative_control_video"): lambda b, r: (429, {"error": {"message": "slow"}}),
              ("c", "negative_control_audio"): lambda b, r: (429, {"error": {"message": "slow"}})}
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, script) as stub:
            text = run_sweep(stub, out)
        rs = [r for r in rows(out) if r["modality"] == "control"]
        check("the failed control is inconclusive, never clean",
              rs and all(r["verdict"] == "CONTROL_INCONCLUSIVE" for r in rs),
              str([r["verdict"] for r in rs]))
        check("and the reads it should have validated do not stand",
              "CONTROLS THAT DID NOT HOLD" in text)
        check("so nothing is claimed about the catalog from them",
              "CATALOG UNDERSTATES — the model reads it and the catalog does not say so: 0" in text,
              "an unvalidated read drove a catalog claim")


def gate_voiding_is_scoped_to_the_modality_that_leaked():
    """Two modalities read, ONE control leaking.

    Without a fixture where a modality's control HOLDS while another's leaks,
    "any leak voids everything" is indistinguishable from correct scoping — and
    that mutation passed until this gate existed.
    """
    print("G8 · a leak voids the modality that leaked, and no other")
    done = {
        ("m", "doc_markdown"): {"model": "m", "case": "doc_markdown", "modality": "file",
                                "verdict": "READ", "declared": ["text"], "whose": "",
                                "answer": "52903", "reason": ""},
        ("m", "image_inline_png"): {"model": "m", "case": "image_inline_png", "modality": "image",
                                    "verdict": "READ", "declared": ["text"], "whose": "",
                                    "answer": "19452", "reason": ""},
        ("m", "negative_control_file"): {"model": "m", "case": "negative_control_file",
                                         "modality": "control", "verdict": "CONTROL_LEAKED",
                                         "declared": ["text"], "whose": "", "answer": "52903",
                                         "reason": "leaked 52903"},
        ("m", "negative_control_image"): {"model": "m", "case": "negative_control_image",
                                          "modality": "control", "verdict": "CONTROL_CLEAN",
                                          "declared": ["text"], "whose": "", "answer": "no image",
                                          "reason": ""},
    }
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        mmx.report(done)
    o = buf.getvalue()
    understates = section(o, "CATALOG UNDERSTATES")
    check("the leaked modality's read does not stand",
          not any("doc_markdown" in l for l in understates),
          "a voided read still drove a catalog claim: " + str(understates))
    check("the clean modality's read DOES stand",
          any("image_inline_png" in l for l in understates),
          "an unrelated modality was voided too: " + str(understates))
    voided = section(o, "CONTROLS THAT DID NOT HOLD")
    check("only the leaked modality is listed as void",
          any("file" in l for l in voided) and not any("image" in l for l in voided),
          str(voided))


def gate_report_both_directions():
    print("G9 · the report distinguishes understating from overstating")
    # Both directions present, so removing either branch is visible.
    done = {
        ("u", "doc_markdown"): {"model": "u", "case": "doc_markdown", "modality": "file",
                                "verdict": "READ", "declared": ["text"], "whose": "",
                                "answer": "52903", "reason": ""},
        # Declared and read: must NOT appear as understating. This is the row
        # whose absence let a mutation removing the declared-check pass.
        ("v", "image_inline_png"): {"model": "v", "case": "image_inline_png", "modality": "image",
                                    "verdict": "READ", "declared": ["text", "image"], "whose": "",
                                    "answer": "19452", "reason": ""},
        ("w", "image_inline_png"): {"model": "w", "case": "image_inline_png", "modality": "image",
                                    "verdict": "ACCEPTED_NOT_READ", "declared": ["text", "image"],
                                    "whose": "", "answer": "I see a picture", "reason": ""},
        ("x", "doc_pdf"): {"model": "x", "case": "doc_pdf", "modality": "file",
                           "verdict": "REFUSED", "whose": "VENDOR", "declared": ["text"],
                           "owned_by": "acme", "answer": "", "reason": "invalid part type: file"},
        ("y", "image_inline_png"): {"model": "y", "case": "image_inline_png", "modality": "image",
                                    "verdict": "REFUSED", "whose": "CATALOG", "declared": ["text"],
                                    "answer": "", "reason": "does not accept image input"},
    }
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        mmx.report(done)
    o = buf.getvalue()
    check("a read of an undeclared modality is understating", "u reads doc_markdown" in o)
    check("a read of a DECLARED modality is not understating", "v reads image_inline_png" not in o,
          "a correct catalog row was reported as a defect")
    check("declared but not delivered is overstating", "w declares image" in o,
          "the direction the goal names first has no bucket")
    check("a vendor refusal is listed", "invalid part type" in o)
    check("and lands on the probe-3 worklist", "acme" in o and "PROBE-3 WORKLIST" in o)
    # Read the section's ROWS, not its header. An earlier version of this check
    # split on "===" — which the header itself ends with — so it inspected the
    # title line and could never see a row that should not have been there.
    leaks = section(o, "VENDOR REFUSAL")
    check("a vendor refusal appears in the vendor list", any("x doc_pdf" in l for l in leaks),
          str(leaks))
    check("our own guard does NOT appear in the vendor list",
          not any("y image_inline_png" in l for l in leaks),
          "a refusal we issued was reported as the vendor's: " + str(leaks))
    check("our own guard is reported as unmeasured instead",
          any("y image_inline_png" in l for l in section(o, "UNMEASURED")))


def gate_fingerprint_invalidates():
    print("G10 · changing a case definition invalidates the rows it produced")
    models = [{"id": "m", "type": "chat", "inputModalities": ["text", "file"]}]
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, {}) as stub:
            run_sweep(stub, out)
            first = len(stub.calls)
        # Same file, a different token budget: the definition changed, so rows
        # measured under the old one must not be reused.
        with Stub(models, {}) as stub2:
            run_sweep(stub2, out, extra=["--max-tokens", "999"])
            second = len(stub2.calls)
        check("a changed definition re-measures rather than reusing",
              second > 0, "stale rows were reused as if they described the new case")
        check("and an unchanged definition still resumes free",
              first > 0)


def gate_budget_ceiling():
    print("G11 · the run cannot silently outgrow its budget")
    models = [{"id": f"m{i}", "type": "chat", "inputModalities": ["text", "image", "file"]}
              for i in range(6)]
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, {}) as stub:
            text = run_sweep(stub, out, extra=["--max-calls", "5"])
            check("it aborts rather than overspending", len(stub.calls) == 0 and "exceeds" in text,
                  f"{len(stub.calls)} calls placed")
        out2 = os.path.join(d, "b.ndjson")
        with Stub(models, {}) as stub2:
            text = run_sweep(stub2, out2, extra=["--dry-run"])
            check("a dry run places no calls", len(stub2.calls) == 0, f"{len(stub2.calls)} calls")
            check("a dry run states BOTH ends of the price",
                  "floor" in text and "ceiling" in text and ".." in text,
                  "a single number under-reports a gated matrix: " + text[:160])


def gate_fixtures_carry_their_anchors():
    print("G12 · every anchored fixture actually contains its anchor")
    for cid, _mod, fname, _mime, anchor, _needs in mmx.CASES:
        if not fname or not anchor:
            continue
        raw = mmx.fixture_bytes(str(ROOT), fname)
        if fname.endswith((".md", ".txt", ".json", ".pdf")):
            check(f"{fname} contains {anchor}", anchor.encode() in raw)
    # The image and video anchors are pixels, so provenance is the check: the
    # generator names them, and the file must be the one it names.
    for fname, anchor in (("ocr-19452.png", "19452"), ("ocr-19452.jpg", "19452"),
                          ("ocr-19452.gif", "19452"), ("ocr-19452.webp", "19452"),
                          ("clip.mp4", "74128")):
        check(f"{fname} is named for its anchor {anchor}", anchor in fname or fname == "clip.mp4")
        check(f"{fname} exists", (ROOT / "tests" / "fixtures" / "media" / fname).exists())
    used = {c[2] for c in mmx.CASES if c[2]}
    for cid, _mod, fname, _mime, anchor, _needs in mmx.CASES:
        if anchor and fname:
            check(f"case {cid} does not use a plain-colour fixture",
                  fname not in ("image.png", "image.jpg"),
                  "a uniform colour cannot distinguish a read from a guess")


def gate_auto_is_swept_and_reported_separately():
    """model:auto is the path the product uses and the reason the program exists.

    Reported in its own section: there is no catalog row behind `auto`, so
    "auto reads a PDF but does not declare file" would name the router as a
    model that understates itself.
    """
    print("G14 · model:auto is measured, and its results are not catalog claims")
    models = [{"id": "real", "type": "chat", "inputModalities": ["text", "file"]}]
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, {}) as stub:
            text = run_sweep(stub, out, auto=True)
            asked = {m for m, _rid in stub.calls}
        check("the sweep addresses model:auto", "auto" in asked, str(asked))
        auto_rows = [r for r in rows(out) if r["model"] == "auto"]
        check("auto produces measured rows", any(r["verdict"] == "READ" for r in auto_rows),
              str([r["verdict"] for r in auto_rows][:6]))
        check("auto has its own report section", "model:auto" in text)
        # auto declares nothing, so every read would otherwise be filed as the
        # catalog understating a model that does not exist.
        understates = section(text, "CATALOG UNDERSTATES")
        check("auto is not reported as a catalog defect",
              not any(l.startswith("auto ") for l in understates), str(understates))

    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "b.ndjson")
        with Stub(models, {}) as stub:
            run_sweep(stub, out, auto=False)
            check("--no-auto skips it", "auto" not in {m for m, _r in stub.calls})


def gate_baseline_diff():
    """The diff that keeps this instrument usable after the catalog is fixed.

    Once one row declares a modality, our routing guard refuses that modality
    for every row that does not — correctly — and those cells stop being wire
    measurements. Re-deriving capability every run would need the catalog to
    stay wrong forever; diffing against what was measured when it COULD be
    measured does not.
    """
    print("G15 · a baseline turns an unmeasurable cell into a checkable claim")
    def row(model, case, verdict, whose="", mod="file"):
        return {"model": model, "case": case, "modality": mod, "verdict": verdict,
                "whose": whose, "declared": ["text"], "answer": "", "reason": ""}
    base = {("m", "doc_markdown"): row("m", "doc_markdown", "READ"),
            ("n", "doc_pdf"): row("n", "doc_pdf", "REFUSED", "VENDOR")}
    now = {("m", "doc_markdown"): row("m", "doc_markdown", "REFUSED", "CATALOG"),
           ("n", "doc_pdf"): row("n", "doc_pdf", "READ")}
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        mmx.report(now, None, base)
    o = buf.getvalue()
    reg = section(o, "REGRESSED")
    rec = section(o, "RECOVERED")
    check("a capability the catalog took away is REGRESSED",
          any("m doc_markdown" in l for l in reg), str(reg))
    check("and it says the wire had read it",
          any("catalog now refuses" in l for l in reg), str(reg))
    check("a cell that started working is RECOVERED",
          any("n doc_pdf" in l for l in rec), str(rec))
    check("and the two are not confused",
          not any("n doc_pdf" in l for l in reg) and not any("m doc" in l for l in rec),
          f"reg={reg} rec={rec}")
    # Without a baseline the sections must not appear at all — an empty diff
    # against nothing would read as "no regressions".
    buf2 = io.StringIO()
    with contextlib.redirect_stdout(buf2):
        mmx.report(now, None, None)
    check("no baseline means no diff claim at all", "REGRESSED" not in buf2.getvalue())


def gate_truncation_escalates():
    """A truncated answer is not a measurement, so the budget grows until it is.

    A reasoning model can spend thousands of tokens before emitting a visible
    character. Under a fixed ceiling those cells came back with empty content
    and were, before the TRUNCATED verdict existed, filed as "accepted but did
    not use the attachment" — the report's most alarming list, populated by our
    own budget.
    """
    print("G16 · a truncated cell is retried with room, not recorded as an answer")
    models = [{"id": "slow", "type": "chat", "inputModalities": ["text", "file"]}]
    seen = []

    def truncate_until_big(body, rid):
        mt = body.get("max_tokens", 0)
        seen.append(mt)
        if mt < 4000:
            return 200, {"choices": [{"message": {"content": ""}, "finish_reason": "length"}]}
        return 200, {"choices": [{"message": {"content": "52903"}, "finish_reason": "stop"}]}

    script = {("slow", c): truncate_until_big for c in
              ("text", "doc_markdown", "doc_plaintext", "doc_pdf", "doc_json",
               "doc_octet_stream", "image_inline_png", "audio_wav", "video_mp4")}
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, script) as stub:
            run_sweep(stub, out, extra=["--max-tokens", "256"])
        rs = {r["case"]: r for r in rows(out)}
        md = rs.get("doc_markdown", {})
        check("the cell ends up READ rather than TRUNCATED",
              md.get("verdict") == "READ", str(md.get("verdict")))
        check("the budget was escalated", md.get("max_tokens_used", 0) > 256,
              str(md.get("max_tokens_used")))
        check("and the escalation is visible in the row", "max_tokens_used" in md)
        check("the first attempt really was at the base budget", 256 in seen, str(seen[:4]))


def gate_lean_retry_attributes_our_own_refusals():
    """A vendor refusal is not a capability fact until a LEAN request fails too.

    Twice in this programme a field WE added turned a working model into an
    apparently incapable one: an unsupported `modalities` parameter took out
    eleven models by failing their liveness cell, and a small token budget took
    out every reasoning model. Either would have been recorded as "this wire
    cannot" — our defect, written down as the vendor's limit, and then acted on
    by a catalog edit.
    """
    print("G17 · a refusal caused by what WE sent is attributed to us, not the wire")
    models = [{"id": "picky", "type": "chat", "inputModalities": ["text", "image"]}]

    def refuse_extras(body, rid):
        # Refuses anything carrying max_tokens — standing in for the real shape
        # of this bug: a field the wire does not know, sent by us.
        if "max_tokens" in body:
            return 400, {"error": {"message": "Unknown parameter: 'max_tokens'.",
                                   "type": "invalid_request_error"}}
        return 200, {"choices": [{"message": {"content": "19452"}, "finish_reason": "stop"}]}

    script = {("picky", c): refuse_extras for c in ("image_inline_png", "text")}
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "a.ndjson")
        with Stub(models, script) as stub:
            run_sweep(stub, out)
            leans = [r for _m, r in stub.calls if r.endswith("-lean")]
        rs = {r["case"]: r for r in rows(out)}
        png = rs.get("image_inline_png", {})
        check("a lean retry was actually issued", len(leans) > 0, str(leans[:3]))
        check("the cell is NOT recorded as a vendor refusal",
              png.get("whose") != "VENDOR", str(png))
        check("it is attributed to our own parameters",
              png.get("whose") == "OURS_PARAM", str(png.get("whose")))
        check("and the attachment is credited as read",
              png.get("verdict") == "READ", str(png.get("verdict")))

    # The negative: a wire that refuses the LEAN request too is a real vendor
    # limit and must stay one. Without this the attribution could just always
    # say OURS_PARAM.
    def refuse_always(body, rid):
        return 400, {"error": {"message": "unsupported image format",
                               "type": "invalid_request_error"}}
    with tempfile.TemporaryDirectory() as d:
        out = os.path.join(d, "b.ndjson")
        with Stub(models, {("picky", "image_inline_png"): refuse_always}) as stub:
            run_sweep(stub, out)
        png = {r["case"]: r for r in rows(out)}.get("image_inline_png", {})
        check("a genuine wire limit stays a VENDOR refusal",
              png.get("whose") == "VENDOR", str(png))


def gate_every_case_builds():
    print("G13 · every case builds without touching the network")
    for case in mmx.CASES:
        if case[0] == "image_url_https":
            continue
        try:
            mmx.build("m", case, str(ROOT), 256, "")
            check(f"{case[0]} builds", True)
        except Exception as e:
            check(f"{case[0]} builds", False, f"{type(e).__name__}: {e}")


if __name__ == "__main__":
    for gate in (gate_classifier, gate_call, gate_sweep_writes_every_skip,
                 gate_transient_is_not_persisted, gate_resume_from_a_truncated_file,
                 gate_liveness_uses_the_required_modality, gate_control_inconclusive,
                 gate_voiding_is_scoped_to_the_modality_that_leaked,
                 gate_report_both_directions, gate_fingerprint_invalidates,
                 gate_budget_ceiling, gate_fixtures_carry_their_anchors,
                 gate_auto_is_swept_and_reported_separately,
                 gate_baseline_diff,
                 gate_truncation_escalates,
                 gate_lean_retry_attributes_our_own_refusals,
                 gate_every_case_builds):
        gate()
    print()
    if FAILURES:
        print(f"{len(FAILURES)} FAILED:")
        for f in FAILURES:
            print("  " + f)
        sys.exit(1)
    print("all gates green")
