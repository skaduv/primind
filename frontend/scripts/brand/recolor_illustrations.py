# -*- coding: utf-8 -*-
"""把插画类 SVG 里的绿色系色相平移到品牌蓝（保持饱和度/明度，避免整体改色走形）。

锚点：#07C05F(绿, hue 148.5°) -> #037EF2(品牌蓝, hue 209.1°)，即 +60.6°。
只处理色相落在 [75°, 180°] 的绿色系颜色，蓝/灰/白不动。
"""
import colorsys
import re
import sys

DELTA = 60.6
GREEN_RANGE = (75.0, 180.0)
HEX_RE = re.compile(r"#([0-9a-fA-F]{6})\b")


def shift(match: re.Match) -> str:
    hx = match.group(1)
    r, g, b = (int(hx[i:i + 2], 16) / 255 for i in (0, 2, 4))
    h, l, s = colorsys.rgb_to_hls(r, g, b)
    deg = h * 360
    if not (GREEN_RANGE[0] <= deg <= GREEN_RANGE[1]):
        return match.group(0)
    h2 = ((deg + DELTA) % 360) / 360
    r2, g2, b2 = colorsys.hls_to_rgb(h2, l, s)
    out = "#%02X%02X%02X" % (round(r2 * 255), round(g2 * 255), round(b2 * 255))
    print(f"  {match.group(0)} ({deg:5.1f}°) -> {out} ({(deg + DELTA) % 360:5.1f}°)")
    return out


for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as fh:
        text = fh.read()
    print(path)
    new = HEX_RE.sub(shift, text)
    with open(path, "w", encoding="utf-8", newline="") as fh:
        fh.write(new)
