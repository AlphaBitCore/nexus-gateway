#!/usr/bin/env python3
"""Turn a measured sweep into the two things that get WRITTEN from it.

The NDJSON is evidence; nobody consults evidence while editing a policy struct.
So this derives the two artefacts the measurement exists to produce, in the shape
they are typed in:

  1. PER-PROVIDER CODEC POLICY — the image formats a wire decoded, the content
     parts it refused, the document types it read. This is what goes into a
     specutil.ContentPolicy, and getting it from here rather than from memory is
     the difference between a whitelist that matches the wire and one that
     refuses what the wire reads. That mistake has already been made once, from
     a four-format list copied across providers.

  2. PER-MODEL CATALOG ROW — the inputModalities a row should declare, beside
     what it declares today. Routing acts on the declaration, so a row that
     understates hides a capable model and one that overstates sends work to a
     model that cannot do it.

A capability is written down ONLY from a READ. An ACCEPTED without an anchor is
not evidence of reading, a TRUNCATED is a statement about the token budget, and a
READ whose control did not hold has not been shown at all.

    python3 tests/scripts/chat-modality-derive.py measured.ndjson > CAPABILITIES.md
"""
import collections
import json
import sys

# case -> (modality name a catalog row spells, media type the wire was given)
IMAGE_CASES = {
    "image_inline_png": "image/png",
    "image_inline_jpg": "image/jpeg",
    "image_inline_gif": "image/gif",
    "image_inline_webp": "image/webp",
    "image_inline_bmp": "image/bmp",
    "image_inline_tiff": "image/tiff",
    "image_inline_heic": "image/heic",
    "image_inline_svg": "image/svg+xml",
}
DOC_CASES = {
    "doc_markdown": "text/markdown",
    "doc_plaintext": "text/plain",
    "doc_pdf": "application/pdf",
    "doc_json": "application/json",
    "doc_octet_stream": "application/octet-stream",
}
MODALITY_OF = {"image": "image", "file": "file", "audio": "audio", "video": "video"}


def load(path):
    """Last row wins per (model, case) — the file is append-only and a cell
    re-measured after a fixture change appends rather than replaces."""
    last = {}
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        r = json.loads(line)
        if r.get("record") == "run-header":
            continue
        last[(r["model"], r["case"])] = r
    return list(last.values())


def main():
    rows = load(sys.argv[1])
    voided = {(r["model"], r["case"].replace("negative_control_", ""))
              for r in rows if r["verdict"] in ("CONTROL_LEAKED", "CONTROL_INCONCLUSIVE")}

    def is_read(r):
        return r["verdict"] == "READ" and (r["model"], r.get("modality")) not in voided

    owner = {}
    declared = {}
    by_model = collections.defaultdict(dict)
    for r in rows:
        owner[r["model"]] = r.get("owned_by") or "?"
        declared[r["model"]] = [d.lower() for d in (r.get("declared") or [])]
        by_model[r["model"]][r["case"]] = r

    print("# Measured capabilities — what to write into a codec policy and a catalog row\n")
    print("Derived from a sweep, not from documentation. A capability appears here only when the")
    print("anchor baked into the fixture came back: an ACCEPTED with no anchor proves nothing, a")
    print("TRUNCATED is about the token budget, and a READ whose control did not hold is void.\n")

    # ---- 1. per-provider codec policy -------------------------------------
    print("## Per-provider codec policy\n")
    print("`ImageFormats` decides what WE refuse, so it is built from the COMPLEMENT of the")
    print("measured refusals — not from the measured reads. A format that produced no anchor")
    print("(none defined, or a truncated answer) was not shown to be unreadable, and refusing it")
    print("would take away a capability the wire has with nothing in the error to explain it.\n")
    prov = collections.defaultdict(lambda: {"img_read": set(), "img_refused": {}, "doc_read": set(),
                                            "doc_refused": {}, "part_refused": {},
                                            "img_unmeasured": set(IMAGE_CASES.values())})
    for r in rows:
        p = prov[owner[r["model"]]]
        case = r["case"]
        if case in IMAGE_CASES:
            mt = IMAGE_CASES[case]
            if r["verdict"] not in ("SKIPPED",):
                p["img_unmeasured"].discard(mt)
            if is_read(r):
                p["img_read"].add(mt)
            elif r["verdict"] == "REFUSED" and r.get("whose") == "VENDOR":
                p["img_refused"][mt] = (r.get("reason") or "")[:70]
        elif case in DOC_CASES:
            mt = DOC_CASES[case]
            if is_read(r):
                p["doc_read"].add(mt)
            elif r["verdict"] == "REFUSED" and r.get("whose") == "VENDOR":
                p["doc_refused"][mt] = (r.get("reason") or "")[:70]
        elif case == "video_mp4" and r["verdict"] == "REFUSED" and r.get("whose") == "VENDOR":
            p["part_refused"]["video_url"] = (r.get("reason") or "")[:70]

    for name in sorted(prov):
        if name in ("?", "nexus-routing"):
            continue
        p = prov[name]
        print(f"### {name}\n")
        print("```go")
        allow = sorted(set(IMAGE_CASES.values()) - set(p["img_refused"]) - p["img_unmeasured"])
        if p["img_refused"]:
            print("ImageFormats: map[string]bool{  // everything NOT measured as refused")
            for mt in allow:
                print(f'\t"{mt}": true,')
            print("},")
        else:
            print("// nothing measured as refused on this wire — a list here would refuse blind")
        print("```\n")
        if p["img_refused"]:
            print("Measured refused (vendor):  " +
                  ", ".join(f"`{k}`" for k in sorted(p["img_refused"])) + "\n")
        unmeasured = sorted(p["img_unmeasured"])
        if unmeasured:
            print("**Unmeasured — a whitelist written now would refuse these blind:** " +
                  ", ".join(f"`{m}`" for m in unmeasured) + "\n")
        if p["doc_read"]:
            print("Documents READ: " + ", ".join(f"`{m}`" for m in sorted(p["doc_read"])) + "\n")
        if p["doc_refused"]:
            print("Documents refused: " +
                  ", ".join(f"`{k}`" for k in sorted(p["doc_refused"])) + "\n")
        if p["part_refused"]:
            for kind, why in sorted(p["part_refused"].items()):
                print(f"Part `{kind}` refused: {why}\n")

    # ---- 2. per-model catalog row -----------------------------------------
    print("## Per-model catalog rows\n")
    print("`should declare` is the union of the modalities this model was MEASURED to read.")
    print("A row is wrong in either direction: understating hides a capable model from routing,")
    print("overstating sends it work it cannot do.\n")
    print("| model | provider | declares today | should declare | direction |")
    print("|---|---|---|---|---|")
    for m in sorted(by_model):
        if m == "auto":
            continue
        read_mods = {MODALITY_OF[r["modality"]] for r in by_model[m].values()
                     if r.get("modality") in MODALITY_OF and is_read(r)}
        have = set(declared.get(m, [])) - {"text"}
        should = read_mods
        if have == should:
            continue
        missing = sorted(should - have)
        extra = sorted(have - should)
        direction = []
        if missing:
            direction.append("understates: **+" + ",".join(missing) + "**")
        if extra:
            direction.append("claims but never demonstrated: " + ",".join(extra))
        print(f"| `{m}` | {owner.get(m,'?')} | {','.join(sorted(have)) or '—'} | "
              f"{','.join(sorted(should)) or '—'} | {' · '.join(direction)} |")


if __name__ == "__main__":
    main()
