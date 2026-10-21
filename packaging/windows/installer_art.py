#!/usr/bin/env python3
"""Draws the side picture of the Windows installer's finish page (164 x 314, as NSIS wants it).

usage: python3 packaging/windows/installer_art.py <out.bmp>     (needs Pillow; packaging/package.sh runs it)

The app's icon on a soft blue panel: the same blue as the window's accent (web/colors.css).
"""
import sys
from PIL import Image

W, H = 164, 314
top, bottom = (0xF4, 0xF7, 0xFC), (0xDC, 0xE7, 0xFB)
img = Image.new("RGB", (W, H))
px = img.load()
for y in range(H):
    t = y / (H - 1)
    c = tuple(round(a + (b - a) * t) for a, b in zip(top, bottom))
    for x in range(W):
        px[x, y] = c
icon = Image.open("extension/icons/icon256.png").convert("RGBA").resize((88, 88), Image.LANCZOS)
img.paste(icon, ((W - 88) // 2, 64), icon)
img.save(sys.argv[1], "BMP")
