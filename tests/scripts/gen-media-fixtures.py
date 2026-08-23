#!/usr/bin/env python3
"""Bake a distinct number into each media fixture, so every media case can
assert comprehension rather than bookkeeping.

A media test can only prove the model RECEIVED the image if the assertion needs
something that is inside the image and nowhere else. The previous image fixture
was a 190-byte plain red square: nothing in it to ask about, so every question
put to it ("what is this", "one word") was answerable — or refusable — without
the bytes ever arriving. A run against it could not tell a working pipeline from
a dropped one.

This bakes a digit string into the pixels. The prompt asks for the number and
nothing else; an answer containing it is evidence the pixels were decoded by the
model, and no prior, guess, or echo of the request reaches it.

Each modality gets a DIFFERENT number. That is deliberate: if the video arm ever
passes carrying the image's number, the arms are crossed and the run would
otherwise report both as healthy.

    image  19452     pdf  38617     markdown  52903     video  74128

Audio keeps its spoken "regression fixture" anchor — a number would need a TTS
engine in the generator, and a spoken phrase is already unreachable without the
bytes.

The fixtures are COMMITTED, so this script runs once on a maintainer's machine
and never in the smoke path. Only the PNG path is stdlib-pure; the PDF is
hand-assembled here and the video needs ffmpeg.

    python3 tests/scripts/gen-media-fixtures.py
"""

import binascii
import io
import json
import os
import pathlib
import struct
import subprocess
import sys
import zlib

# 5x7 bitmap digits. Hand-drawn rather than pulled from a font file so the
# fixture has no external dependency and the glyphs stay legible at the scale
# used below — an OCR fixture that is ambiguous to a human is not a fair test.
GLYPHS = {
    "0": ["01110", "10001", "10011", "10101", "11001", "10001", "01110"],
    "1": ["00100", "01100", "00100", "00100", "00100", "00100", "01110"],
    "2": ["01110", "10001", "00001", "00010", "00100", "01000", "11111"],
    "3": ["01110", "10001", "00001", "00110", "00001", "10001", "01110"],
    "4": ["00010", "00110", "01010", "10010", "11111", "00010", "00010"],
    "5": ["11111", "10000", "11110", "00001", "00001", "10001", "01110"],
    "6": ["00110", "01000", "10000", "11110", "10001", "10001", "01110"],
    "7": ["11111", "00001", "00010", "00100", "01000", "01000", "01000"],
    "8": ["01110", "10001", "10001", "01110", "10001", "10001", "01110"],
    "9": ["01110", "10001", "10001", "01111", "00001", "00010", "01100"],
}

SCALE = 14      # px per glyph pixel
PAD = 28        # border, so no digit touches an edge
GAP = 2         # blank glyph-columns between digits


def render(text: str) -> bytes:
    """The anchor as pixels a MODEL can read, not merely a human.

    The hand-drawn 5x7 set below was legible to a reviewer and not to the
    models. Measured across the catalog: with the image attached, models
    answered 19432, 19472, 19352, 19280, 15452 — each within a digit or two of
    19452 — and with nothing attached the same models answered 42, 3, 880. They
    were reading the image and misreading a glyph, and every one of those runs
    was filed under "accepted but did not use the attachment", the list that
    exists to catch a dropped attachment. A fixture that manufactures that
    verdict is worse than no fixture.

    The confusions were structural rather than random: at this resolution 5 and
    3 differ in two cells, and 5 and 7 share their entire top bar. A real
    typeface has none of those collisions, so the glyphs are drawn from one when
    Pillow is present and the bitmap set is kept as the fallback that needs no
    dependency at all.
    """
    try:
        return _render_truetype(text)
    except Exception:
        pass
    for ch in text:
        if ch not in GLYPHS:
            raise SystemExit(f"no glyph for {ch!r}; digits only")
    gw = 5 * len(text) + GAP * (len(text) - 1)
    gh = 7
    w, h = gw * SCALE + 2 * PAD, gh * SCALE + 2 * PAD
    # White background, black digits — maximum contrast for any vision model.
    rows = [bytearray(b"\xff" * (w * 3)) for _ in range(h)]
    for gi, ch in enumerate(text):
        x0 = gi * (5 + GAP)
        for gy, line in enumerate(GLYPHS[ch]):
            for gx, bit in enumerate(line):
                if bit != "1":
                    continue
                for py in range(SCALE):
                    y = PAD + gy * SCALE + py
                    start = (PAD + (x0 + gx) * SCALE) * 3
                    rows[y][start:start + SCALE * 3] = b"\x00" * (SCALE * 3)
    raw = b"".join(b"\x00" + bytes(r) for r in rows)

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", binascii.crc32(tag + data) & 0xFFFFFFFF))

    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 9))
            + chunk(b"IEND", b""))


# Preferred first, and all bold: a heavy weight survives JPEG and h264 better
# than a light one, and the video fixture has to stay legible after encoding.
_FONT_CANDIDATES = (
    "/System/Library/Fonts/Supplemental/Arial Black.ttf",
    "/System/Library/Fonts/Supplemental/Arial Bold.ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
)


def _render_truetype(text: str) -> bytes:
    from PIL import Image, ImageDraw, ImageFont  # optional; bitmap fallback otherwise

    font = None
    for path in _FONT_CANDIDATES:
        if os.path.exists(path):
            font = ImageFont.truetype(path, 160)
            break
    if font is None:
        raise RuntimeError("no bundled-quality font available")
    tmp = ImageDraw.Draw(Image.new("RGB", (1, 1)))
    box = tmp.textbbox((0, 0), text, font=font)
    w, h = box[2] - box[0] + 2 * PAD, box[3] - box[1] + 2 * PAD
    img = Image.new("RGB", (w, h), "white")
    ImageDraw.Draw(img).text((PAD - box[0], PAD - box[1]), text, font=font, fill="black")
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    return buf.getvalue()


def make_pdf(number: str) -> bytes:
    """A one-page PDF whose only content is the number, in Helvetica.

    Hand-assembled rather than generated by a library so the fixture has no
    build dependency and stays a few hundred bytes — a reviewer can read the
    whole thing and see there is nothing in it but the anchor.
    """
    content = f"BT /F1 64 Tf 48 60 Td ({number}) Tj ET".encode()
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 360 180] "
        b"/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
        b"<< /Length " + str(len(content)).encode() + b" >>\nstream\n" + content + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for i, body in enumerate(objs, start=1):
        offsets.append(len(out))
        out += f"{i} 0 obj\n".encode() + body + b"\nendobj\n"
    xref = len(out)
    out += f"xref\n0 {len(objs) + 1}\n".encode() + b"0000000000 65535 f \n"
    for off in offsets:
        out += f"{off:010d} 00000 n \n".encode()
    out += (f"trailer\n<< /Size {len(objs) + 1} /Root 1 0 R >>\n"
            f"startxref\n{xref}\n%%EOF\n").encode()
    return bytes(out)


def make_markdown(number: str) -> str:
    """The markdown arm's anchor, stated once and unmistakably.

    The previous fixture asked the model to notice that SQLite alone is not
    idempotent — a good anchor, but a semantic one: a model reasoning well from
    priors could land on it, and a model reasoning badly could miss it while
    having read the file perfectly. A number is unambiguous in both directions.
    """
    return (
        "# Migration runbook\n\n"
        "This document is a test fixture for media comprehension.\n\n"
        f"## Reference number\n\nThe reference number for this runbook is {number}.\n\n"
        "| Database   | File                    | Idempotent |\n"
        "|------------|-------------------------|------------|\n"
        "| PostgreSQL | `schema/widget-pg.sql`  | yes        |\n"
        "| MySQL      | `schema/widget-my.sql`  | yes        |\n"
        "| SQLite     | `schema/widget-lt.sql`  | no         |\n\n"
        "The SQLite variant is **not** idempotent — running it twice creates a\n"
        "duplicate row. The other two guard with `IF NOT EXISTS`.\n"
    )


def make_json(number: str) -> str:
    """The JSON arm's anchor, in bytes that are ACTUALLY JSON.

    Reusing the markdown fixture under an application/json label — which is what
    happened before this existed — confounds the one axis the cell isolates. A
    provider that parses JSON attachments returns a legitimate "this is not
    JSON" refusal, and that refusal then reads as a capability limit rather than
    as a payload we malformed ourselves.

    A distinct number from the markdown arm, for the same reason the modalities
    differ: if this cell ever answers 52903, the bytes we sent were not the
    bytes we named.
    """
    return json.dumps({
        "document": "migration runbook",
        "referenceNumber": number,
        "note": "This document is a test fixture for media comprehension.",
        "migrations": [
            {"database": "PostgreSQL", "file": "schema/widget-pg.sql", "idempotent": True},
            {"database": "MySQL", "file": "schema/widget-my.sql", "idempotent": True},
            {"database": "SQLite", "file": "schema/widget-lt.sql", "idempotent": False},
        ],
    }, indent=2) + "\n"


def transcode_image(src: pathlib.Path, out_path: pathlib.Path) -> None:
    """The same glyphs in another container, so every image format carries the
    same unguessable anchor.

    Format was the only axis these cells were ever meant to vary. Rendering each
    one separately — or worse, keeping a plain coloured square for some of them —
    means a refusal cannot be attributed to the container rather than to the
    content, which is the single question the format matrix asks.

    Lossy encoders get quality pinned high for the same reason the video does:
    a fixture whose anchor the encoder destroyed tests the encoder.
    """
    quality = []
    if out_path.suffix == ".jpg":
        quality = ["-q:v", "2"]
    elif out_path.suffix == ".webp":
        quality = ["-quality", "95"]
    subprocess.run(
        ["ffmpeg", "-y", "-loglevel", "error", "-i", str(src)] + quality + [str(out_path)],
        check=True,
    )


def make_video(number: str, out_path: pathlib.Path) -> None:
    """A short clip showing the number, built from the same glyph renderer.

    ffmpeg is required, and only here: the clip is committed like every other
    fixture, so the smoke never needs it.
    """
    import tempfile

    frame = render(number)
    with tempfile.TemporaryDirectory() as td:
        src = pathlib.Path(td) / "frame.png"
        src.write_bytes(frame)
        subprocess.run(
            # -crf 18 and a slow preset, deliberately. The first version took
            # the encoder's defaults and produced a 3 KB clip at 4 kb/s: the
            # SAME glyph render that a model reads correctly as a PNG became
            # unreadable once h264 at that bitrate had smeared the strokes, and
            # the arm reported "the model did NOT demonstrate reading the
            # media" for a clip in which the digits were no longer there. A
            # fixture the anchor cannot survive tests nothing but the encoder.
            ["ffmpeg", "-y", "-loglevel", "error", "-loop", "1", "-i", str(src),
             "-t", "3", "-r", "8", "-pix_fmt", "yuv420p",
             "-crf", "18", "-preset", "slow",
             "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", str(out_path)],
            check=True,
        )


if __name__ == "__main__":
    media = pathlib.Path(__file__).resolve().parents[1] / "fixtures" / "media"

    (media / "ocr-19452.png").write_bytes(render("19452"))
    (media / "doc.pdf").write_bytes(make_pdf("38617"))
    (media / "doc.md").write_text(make_markdown("52903"))
    # The same characters under a name and type a plain-text cell can claim
    # honestly. The media type is then the only variable between the two cells.
    (media / "doc.txt").write_text(make_markdown("52903"))
    (media / "doc.json").write_text(make_json("61074"))
    make_video("74128", media / "clip.mp4")

    # Every image format carries the same anchor, so the format matrix varies
    # format and nothing else.
    # Every format a caller plausibly attaches, from the same glyph source, so
    # the format matrix varies format and nothing else. bmp and tiff are here
    # because "which formats does this wire read" cannot be answered by testing
    # only the ones we expect to work — a wire that reads tiff and a catalog
    # that does not say so is the same defect as the reverse.
    for ext in ("jpg", "gif", "webp", "bmp", "tiff"):
        transcode_image(media / "ocr-19452.png", media / f"ocr-19452.{ext}")

    written = ["ocr-19452.png", "ocr-19452.jpg", "ocr-19452.gif", "ocr-19452.webp",
               "ocr-19452.bmp", "ocr-19452.tiff",
               "doc.pdf", "doc.md", "doc.txt", "doc.json", "clip.mp4"]
    for name in written:
        f = media / name
        print(f"{name:18s} {f.stat().st_size:>9,d} B")
