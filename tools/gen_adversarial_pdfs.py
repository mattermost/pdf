#!/usr/bin/env python3
"""Generate adversarial PDFs that stress cancellation / allocation gaps in mattermost/pdf.

These are for local investigation (often under GOMEMLIMIT), not for normal CI.

Gaps targeted
-------------
1. tounicode_cmap     – huge ToUnicode CMap; readCmap uses context.Background()
2. huge_literal_tj    – one enormous (...) Tj; readLiteralString runs between cancel polls
3. flate_bomb_tj      – small file, FlateDecode expands into one huge literal string
4. predictor_columns  – FlateDecode + Predictor 12 with huge Columns (pre-read alloc)
5. many_operators     – control: many tiny Tj ops (cancel *can* stop between them)
6. acroform_fields    – many AcroForm fields (negative control: GetPlainText does not walk them)
7. nested_content     – deeply nested arrays in the content stream (readObject depth)

Examples
--------
  ./tools/gen_adversarial_pdfs.py -o /tmp/pdfs
  ./tools/gen_adversarial_pdfs.py -o /tmp/pdfs --scale large

Then, from a Go harness or the examples/ binary, run GetPlainText under e.g.:
  GOMEMLIMIT=256MiB go test ./... -run Adversarial
"""

from __future__ import annotations

import argparse
import zlib
from pathlib import Path


def _xref_and_trailer(body: bytearray, offsets: list[int], root_obj: int = 1) -> bytes:
    xref_pos = len(body)
    n = len(offsets)
    parts = [f"xref\n0 {n}\n".encode(), b"0000000000 65535 f \r\n"]
    for off in offsets[1:]:
        parts.append(f"{off:010d} 00000 n \r\n".encode())
    parts.append(
        f"trailer << /Size {n} /Root {root_obj} 0 R >>\n"
        f"startxref\n{xref_pos}\n%%EOF\n".encode()
    )
    return b"".join(parts)


def _write_objects(objs: list[bytes]) -> bytes:
    """objs is 1-indexed conceptually; objs[0] unused. Each entry is object body only."""
    body = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = [0] * len(objs)
    for i in range(1, len(objs)):
        offsets[i] = len(body)
        body.extend(f"{i} 0 obj\n".encode())
        body.extend(objs[i])
        body.extend(b"\nendobj\n")
    body.extend(_xref_and_trailer(body, offsets))
    return bytes(body)


def _stream(data: bytes, extra_dict: str = "") -> bytes:
    return (
        f"<< /Length {len(data)}{extra_dict} >>\nstream\n".encode()
        + data
        + b"\nendstream"
    )


def _flate_stream(raw: bytes, extra_dict: str = "") -> bytes:
    compressed = zlib.compress(raw, 9)
    return _stream(compressed, f" /Filter /FlateDecode{extra_dict}")


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

def build_tounicode_cmap(bfchar_count: int) -> bytes:
    """Page uses a font whose ToUnicode CMap has many bfchar entries.

    GetPlainText → Tf → Font.Encoder() → readCmap → Interpret(context.Background()).
    Cancellation of the outer ctx does not stop cmap parsing.
    """
    # Identity-ish mapping: <XXXX> <00XX> pairs; keep entries small but numerous.
    lines = [
        b"/CIDInit /ProcSet findresource begin",
        b"12 dict begin",
        b"begincmap",
        b"/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def",
        b"/CMapName /Adobe-Identity-UCS def",
        b"/CMapType 2 def",
        b"1 begincodespacerange",
        b"<0000> <FFFF>",
        b"endcodespacerange",
    ]
    # PDF allows multiple beginbfchar blocks; keep each block ≤ 100 for realism.
    remaining = bfchar_count
    code = 0
    while remaining > 0:
        n = min(100, remaining)
        lines.append(f"{n} beginbfchar".encode())
        for _ in range(n):
            lines.append(f"<{code:04X}> <{code & 0xFF:02X}>".encode())
            code = (code + 1) & 0xFFFF
        lines.append(b"endbfchar")
        remaining -= n
    lines += [b"endcmap", b"CMapName currentdict /CMap defineresource pop", b"end", b"end"]
    cmap_raw = b"\n".join(lines)

    content = b"BT /F1 12 Tf 10 700 Td (x) Tj ET"

    # 1 Catalog, 2 Pages, 3 Page, 4 Contents, 5 Font, 6 ToUnicode, 7 Font descriptor-ish skip
    objs: list[bytes] = [b""]
    objs.append(b"<< /Type /Catalog /Pages 2 0 R >>")
    objs.append(b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
    objs.append(
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"
    )
    objs.append(_stream(content))
    # Type1 + missing /Encoding → getEncoder → charmapEncoding → readCmap.
    # (Type0 /Identity-H also works, but empty DescendantFonts is fragile.)
    objs.append(
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Evil /ToUnicode 6 0 R >>"
    )
    objs.append(_flate_stream(cmap_raw))
    return _write_objects(objs)


def build_huge_literal_tj(string_bytes: int) -> bytes:
    """Single Tj whose literal string is string_bytes long.

    Interpret checks ctx only *before* readToken; readLiteralString then
    appends the entire payload with no further cancel checks.
    """
    # Keep the PDF valid: avoid raw parentheses/backslashes inside the literal.
    payload = b"A" * string_bytes
    content = b"BT /F1 12 Tf 10 700 Td (" + payload + b") Tj ET"
    return _simple_page_with_content(content, flate=False)


def build_flate_bomb_tj(expanded_bytes: int) -> bytes:
    """Small on-disk FlateDecode content stream that expands to one huge Tj string."""
    payload = b"A" * expanded_bytes
    content = b"BT /F1 12 Tf 10 700 Td (" + payload + b") Tj ET"
    return _simple_page_with_content(content, flate=True)


def build_predictor_columns(columns: int) -> bytes:
    """Content stream with Predictor 12 and a huge Columns value.

    applyFilter allocates hist/tmp of length 1+columns before any Interpret loop.
    """
    # Minimal flate payload; allocation happens at filter setup, not from inflate size.
    raw = b"q Q\n"
    compressed = zlib.compress(raw, 9)
    extras = (
        f" /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns {columns} >>"
    )
    content_obj = _stream(compressed, extras)

    objs: list[bytes] = [b""]
    objs.append(b"<< /Type /Catalog /Pages 2 0 R >>")
    objs.append(b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
    objs.append(
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"
    )
    objs.append(content_obj)
    objs.append(b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
    return _write_objects(objs)


def build_many_operators(op_count: int) -> bytes:
    """Control case: many tiny Tj operators — cancel *should* stop between them."""
    parts = [b"BT /F1 12 Tf 10 700 Td"]
    for _ in range(op_count):
        parts.append(b"(.) Tj")
    parts.append(b"ET")
    return _simple_page_with_content(b" ".join(parts), flate=False)


def build_acroform_fields(field_count: int) -> bytes:
    """AcroForm with many field dictionaries.

    Negative control: GetPlainText never walks /AcroForm. Useful to show that
    'AcroForm-heavy' alone does not exercise the extract path unless page
    content/fonts also pull those objects in.
    """
    # Layout:
    # 1 Catalog (+AcroForm), 2 Pages, 3 Page, 4 Contents, 5 Font,
    # 6 AcroForm dict, 7..6+field_count field objs
    objs: list[bytes | None] = [None] * (7 + field_count)
    objs[0] = b""
    field_refs = " ".join(f"{7 + i} 0 R" for i in range(field_count))
    objs[1] = b"<< /Type /Catalog /Pages 2 0 R /AcroForm 6 0 R >>"
    objs[2] = b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>"
    objs[3] = (
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"
    )
    objs[4] = _stream(b"BT /F1 12 Tf 10 700 Td (hello) Tj ET")
    objs[5] = b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"
    objs[6] = f"<< /Fields [{field_refs}] /NeedAppearances true >>".encode()
    for i in range(field_count):
        objs[7 + i] = (
            f"<< /FT /Tx /T (field{i}) /V ({'X' * 64}) "
            f"/Rect [0 0 10 10] /Subtype /Widget /Type /Annot >>"
        ).encode()
    return _write_objects(objs)  # type: ignore[arg-type]


def build_nested_content(depth: int) -> bytes:
    """Content stream whose first object is a deeply nested array.

    Interpret → readObject on '[' … nested … ']'. Hits maxObjectDepth (1000)
    if depth is large enough; demonstrates uncancellable work inside one readObject.
    """
    nested = b"[" * depth + b"1" + b"]" * depth
    # After the nested object, a trivial text op so a shallow parse still looks like a page.
    content = nested + b" BT /F1 12 Tf 10 700 Td (x) Tj ET"
    return _simple_page_with_content(content, flate=False)


def _simple_page_with_content(content: bytes, *, flate: bool) -> bytes:
    objs: list[bytes] = [b""]
    objs.append(b"<< /Type /Catalog /Pages 2 0 R >>")
    objs.append(b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
    objs.append(
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
        b"/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"
    )
    objs.append(_flate_stream(content) if flate else _stream(content))
    objs.append(b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
    return _write_objects(objs)


# ---------------------------------------------------------------------------
# Scales
# ---------------------------------------------------------------------------

SCALES = {
    # Small enough to generate quickly and inspect; may not OOM.
    "small": {
        "tounicode_cmap": 2_000,
        "huge_literal_tj": 1_000_000,       # 1 MiB string
        "flate_bomb_tj": 5_000_000,         # 5 MiB expanded
        "predictor_columns": 5_000_000,     # ~5 MiB alloc ×2
        "many_operators": 10_000,
        "acroform_fields": 1_000,
        "nested_content": 1_200,            # must exceed maxObjectDepth=1000
    },
    # Default for automated probes: big enough to show gaps, safe on a laptop.
    "medium": {
        "tounicode_cmap": 50_000,
        "huge_literal_tj": 16_000_000,      # 16 MiB
        "flate_bomb_tj": 32_000_000,        # 32 MiB expanded
        "predictor_columns": 16_000_000,    # ~16 MiB ×2
        "many_operators": 80_000,
        "acroform_fields": 10_000,
        "nested_content": 1_200,            # above maxObjectDepth=1000
    },
    # Intended for GOMEMLIMIT experiments on a workstation.
    "large": {
        "tounicode_cmap": 200_000,
        "huge_literal_tj": 64_000_000,      # 64 MiB
        "flate_bomb_tj": 256_000_000,       # 256 MiB expanded from small file
        "predictor_columns": 64_000_000,    # ~64 MiB ×2
        "many_operators": 200_000,
        "acroform_fields": 50_000,
        "nested_content": 1_200,            # above maxObjectDepth=1000
    },
}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("-o", "--outdir", type=Path, default=Path("tools/adversarial_pdfs"))
    ap.add_argument("--scale", choices=sorted(SCALES), default="medium")
    ap.add_argument(
        "only",
        nargs="*",
        help="Optional fixture names to build (default: all)",
    )
    args = ap.parse_args()
    args.outdir.mkdir(parents=True, exist_ok=True)
    cfg = SCALES[args.scale]

    builders = {
        "tounicode_cmap": lambda: build_tounicode_cmap(cfg["tounicode_cmap"]),
        "huge_literal_tj": lambda: build_huge_literal_tj(cfg["huge_literal_tj"]),
        "flate_bomb_tj": lambda: build_flate_bomb_tj(cfg["flate_bomb_tj"]),
        "predictor_columns": lambda: build_predictor_columns(cfg["predictor_columns"]),
        "many_operators": lambda: build_many_operators(cfg["many_operators"]),
        "acroform_fields": lambda: build_acroform_fields(cfg["acroform_fields"]),
        "nested_content": lambda: build_nested_content(cfg["nested_content"]),
    }
    selected = args.only or list(builders)
    unknown = set(selected) - set(builders)
    if unknown:
        ap.error(f"unknown fixtures: {sorted(unknown)}")

    manifest = []
    for name in selected:
        data = builders[name]()
        path = args.outdir / f"{name}.{args.scale}.pdf"
        path.write_bytes(data)
        manifest.append((name, path, len(data)))
        print(f"wrote {path} ({len(data):,} bytes on disk)")

    readme = args.outdir / "README.md"
    readme.write_text(
        f"""# Adversarial PDF fixtures (scale={args.scale})

Generated by `tools/gen_adversarial_pdfs.py`.

| File | Gap under test |
|------|----------------|
| `tounicode_cmap.*.pdf` | `readCmap` → `Interpret(context.Background())` ignores caller cancel |
| `huge_literal_tj.*.pdf` | `readLiteralString` allocates one giant token between cancel polls |
| `flate_bomb_tj.*.pdf` | Same as above, but FlateDecode hides size on disk |
| `predictor_columns.*.pdf` | `applyFilter` allocates `1+Columns` before Interpret runs |
| `many_operators.*.pdf` | **Control** — cancel can stop between Tj ops |
| `acroform_fields.*.pdf` | **Negative control** — GetPlainText does not walk `/AcroForm` |
| `nested_content.*.pdf` | Deep `readObject` nesting inside one content-stream object |

## Suggested probe

```bash
# From the pdf module root, with a small memory ceiling:
GOMEMLIMIT=256MiB go run ./tools/probe_extract.go /path/to/fixture.pdf
```

Expect for bomb fixtures: process hits the memory limit (or grows RSS sharply)
even when the context is cancelled / short-deadline, if the gap is real.
For `many_operators`, a cancelled context should return promptly with little growth.
For `acroform_fields`, extraction should stay cheap (fields unused).
"""
    )
    print(f"wrote {readme}")


if __name__ == "__main__":
    main()
