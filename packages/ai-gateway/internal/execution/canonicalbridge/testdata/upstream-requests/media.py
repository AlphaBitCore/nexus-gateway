"""Real media bytes for the request corpus.

The fixtures these replace carried `https://ex.com/a.png` and a six-character
`JVBERi0x` "PDF". Neither is fetchable or parseable, so a capture using them
would measure the provider's rejection rather than the conversion — and a
hand-written fixture is exactly what a captured corpus exists to stop being.

Both assets are generated rather than downloaded: no network at test time, no
licence question, and the bytes are identical on every machine.
"""

import base64
import zlib


def png_1x1() -> bytes:
    """A valid 1x1 opaque red PNG, built chunk by chunk.

    Small enough to inline in a request body, real enough that every vision
    model decodes it instead of answering about a broken image.
    """

    def chunk(kind: bytes, payload: bytes) -> bytes:
        return (
            len(payload).to_bytes(4, "big")
            + kind
            + payload
            + zlib.crc32(kind + payload).to_bytes(4, "big")
        )

    ihdr = (
        (1).to_bytes(4, "big")  # width
        + (1).to_bytes(4, "big")  # height
        + bytes([8, 2, 0, 0, 0])  # 8-bit, truecolour, no interlace
    )
    raw = bytes([0, 255, 0, 0])  # filter byte 0, then one RGB pixel
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(raw))
        + chunk(b"IEND", b"")
    )


def pdf_one_page(text: str = "Nexus corpus fixture: the weather report is attached.") -> bytes:
    """A valid single-page PDF carrying one line of text.

    The xref offsets are computed from the objects as written rather than
    guessed, because a PDF with a wrong xref table parses in some readers and
    fails in others — and a fixture that some providers reject would look like
    a codec defect the first time it was hit.
    """
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 80] "
        b"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
        None,  # content stream, filled below
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    stream = f"BT /F1 10 Tf 12 40 Td ({text}) Tj ET".encode()
    objects[3] = b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream"

    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for i, body in enumerate(objects, start=1):
        offsets.append(len(out))
        out += str(i).encode() + b" 0 obj\n" + body + b"\nendobj\n"

    xref_at = len(out)
    out += b"xref\n0 " + str(len(objects) + 1).encode() + b"\n"
    out += b"0000000000 65535 f \n"
    for off in offsets:
        out += f"{off:010d} 00000 n \n".encode()
    out += (
        b"trailer\n<< /Size " + str(len(objects) + 1).encode() + b" /Root 1 0 R >>\n"
        b"startxref\n" + str(xref_at).encode() + b"\n%%EOF\n"
    )
    return bytes(out)


PNG_B64 = base64.b64encode(png_1x1()).decode()
PDF_B64 = base64.b64encode(pdf_one_page()).decode()
PNG_DATA_URL = "data:image/png;base64," + PNG_B64
PDF_DATA_URL = "data:application/pdf;base64," + PDF_B64
