"""Small PNG images for the attachment tests, made without any imaging
library: a word in large block letters (black on white), readable by a
model, and solid squares."""

from __future__ import annotations

import struct
import zlib

# 5×7 glyphs, one string of 5 cells per row; '#' is ink.
GLYPHS = {
    "A": [" ### ", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"],
    "E": ["#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#####"],
    "G": [" ####", "#    ", "#    ", "#  ##", "#   #", "#   #", " ####"],
    "I": ["#####", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "#####"],
    "L": ["#    ", "#    ", "#    ", "#    ", "#    ", "#    ", "#####"],
    "M": ["#   #", "## ##", "# # #", "# # #", "#   #", "#   #", "#   #"],
    "N": ["#   #", "##  #", "# # #", "#  ##", "#   #", "#   #", "#   #"],
    "O": [" ### ", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "],
    "P": ["#### ", "#   #", "#   #", "#### ", "#    ", "#    ", "#    "],
    "R": ["#### ", "#   #", "#   #", "#### ", "# #  ", "#  # ", "#   #"],
    "T": ["#####", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "  #  "],
    "U": ["#   #", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "],
}


def _png(width: int, height: int, rows: list[bytes]) -> bytes:
    """An 8-bit greyscale PNG of rows (each width bytes)."""
    def chunk(typ: bytes, data: bytes) -> bytes:
        return struct.pack(">I", len(data)) + typ + data + struct.pack(">I", zlib.crc32(typ + data) & 0xFFFFFFFF)
    raw = b"".join(b"\x00" + r for r in rows)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 0, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


def word_png(word: str, scale: int = 12, margin: int = 3) -> bytes:
    """word in block letters, each cell scale pixels, margin cells around."""
    cells_w = len(word) * 6 - 1 + 2 * margin
    cells_h = 7 + 2 * margin
    grid = [[False] * cells_w for _ in range(cells_h)]
    for i, ch in enumerate(word.upper()):
        for y, row in enumerate(GLYPHS[ch]):
            for x, c in enumerate(row):
                grid[margin + y][margin + i * 6 + x] = c == "#"
    rows = []
    for cy in range(cells_h):
        line = bytes(0 if grid[cy][cx // scale] else 255 for cx in range(cells_w * scale))
        rows += [line] * scale
    return _png(cells_w * scale, cells_h * scale, rows)


def square_png(shade: int, size: int = 8) -> bytes:
    """A solid grey square: distinct bytes per shade."""
    return _png(size, size, [bytes([shade]) * size] * size)
