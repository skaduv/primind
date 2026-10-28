# -*- coding: utf-8 -*-
"""从 RISE 主标识生成平台使用的一整套图标资源。

输入：scripts/brand/source/rise-logo-master.png（白底主标识，1254x1254）
输出：
  frontend/src/assets/brand/*.png     前端 logo（浅色 / 深色 / 完整 / 紧凑 / 图形）
  frontend/public/favicon.ico         浏览器页签图标（16~256 多尺寸）
  frontend/public/apple-touch-icon.png
  cmd/desktop/build/appicon.png       桌面端（Wails）应用图标

用法：
  python scripts/brand/build_logo_assets.py

说明：
  * 主标识原图是白底位图。这里用 color-to-alpha 去掉白底，并把边缘像素
    按覆盖率反预乘，避免在深色背景上出现白色描边。
  * 所有缩放都走预乘 alpha + BOX 采样，避免 LANCZOS 振铃在透明边缘产生锯齿。
"""

from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw, ImageFilter

ROOT = Path(__file__).resolve().parents[2]
SRC = ROOT / "scripts" / "brand" / "source" / "rise-logo-master.png"
BRAND_DIR = ROOT / "frontend" / "src" / "assets" / "brand"
PUBLIC_DIR = ROOT / "frontend" / "public"
DESKTOP_DIR = ROOT / "cmd" / "desktop" / "build"

# 主标识在 1254x1254 原图中的内容边界
FULL_BOX = (84, 416, 1161, 815)
MARK_BOX = (84, 416, 552, 815)

# 深色背景下文字改用的浅色
ON_DARK_TEXT = (237, 245, 255)
# “E” 顶部的蓝绿渐变条（原图坐标），深色版必须保留，不能跟着文字一起刷白
GRADIENT_BAR_BOX = (1000, 520, 1161, 580)


def load_master() -> Image.Image:
    """读取白底主标识，返回去底后的 RGBA 图。"""
    rgb = np.asarray(Image.open(SRC).convert("RGB")).astype(np.float32)
    # 白底位图的覆盖率：最暗通道越接近 0，说明该像素越“实”。
    alpha = np.clip(1.0 - rgb.min(axis=2) / 255.0, 0.0, 1.0)
    alpha[alpha < 0.08] = 0.0
    a = np.maximum(alpha, 1e-4)[..., None]
    # 反预乘：去掉边缘像素里混入的白色
    color = np.clip((rgb - 255.0 * (1.0 - a)) / a, 0, 255)
    return Image.fromarray(np.dstack([color, alpha * 255]).astype(np.uint8), "RGBA")


def resize_premul(img: Image.Image, width: int) -> Image.Image:
    """预乘 alpha 的等比缩放，避免透明边缘发黑/发白。"""
    arr = np.asarray(img).astype(np.float32)
    alpha = arr[..., 3:4] / 255.0
    prem = arr[..., :3] * alpha
    size = (width, max(1, round(img.height * width / img.width)))
    if width <= img.width:
        # 缩小：BOX（面积平均）不会在透明边缘产生振铃。
        rgb = np.asarray(
            Image.fromarray(np.clip(prem, 0, 255).astype(np.uint8)).resize(size, Image.BOX)
        ).astype(np.float32)
        out_alpha = np.asarray(Image.fromarray(arr[..., 3]).resize(size, Image.BOX)).astype(np.float32)
    else:
        # 放大：32 位浮点通道 + LANCZOS，避免先量化再插值产生色带。
        def resize_channel(channel: np.ndarray) -> np.ndarray:
            return np.asarray(
                Image.fromarray(channel.astype(np.float32), mode="F").resize(size, Image.LANCZOS)
            ).astype(np.float32)

        rgb = np.stack([resize_channel(prem[..., i]) for i in range(3)], axis=-1)
        out_alpha = resize_channel(arr[..., 3])

    out_alpha = np.clip(out_alpha, 0, 255)
    out_alpha[out_alpha < 6] = 0
    color = np.clip(rgb / np.maximum(out_alpha / 255.0, 1e-6)[..., None], 0, 255)
    return Image.fromarray(np.dstack([color, out_alpha]).astype(np.uint8), "RGBA")


def recolor_on_dark(img: Image.Image) -> Image.Image:
    """把深蓝文字/副标题改成浅色，保留 E 上的蓝绿渐变条和左侧图形。"""
    arr = np.asarray(img).astype(np.float32).copy()
    h, w = arr.shape[:2]
    bar = (
        GRADIENT_BAR_BOX[0] - FULL_BOX[0],
        GRADIENT_BAR_BOX[1] - FULL_BOX[1],
        GRADIENT_BAR_BOX[2] - FULL_BOX[0],
        GRADIENT_BAR_BOX[3] - FULL_BOX[1],
    )
    text_x = 470  # 左侧图形与文字之间的分界（裁剪坐标）
    region = arr[:, text_x:, :3]
    mask = np.zeros(region.shape[:2], dtype=bool)
    mask[:] = True
    mask[bar[1] : bar[3], max(bar[0] - text_x, 0) : bar[2] - text_x] = False
    region[mask] = ON_DARK_TEXT
    arr[:, text_x:, :3] = region
    return Image.fromarray(arr.astype(np.uint8), "RGBA")


def make_mark(master: Image.Image, canvas_size: int = 512, ratio: float = 0.92) -> Image.Image:
    """把左侧图形放到正方形透明画布中央。"""
    mark = master.crop(MARK_BOX)
    target_w = round(canvas_size * ratio)
    mark = resize_premul(mark, target_w)
    canvas = Image.new("RGBA", (canvas_size, canvas_size), (0, 0, 0, 0))
    canvas.alpha_composite(mark, ((canvas_size - mark.width) // 2, (canvas_size - mark.height) // 2))
    return canvas


def make_tile(
    master: Image.Image,
    size: int = 1024,
    ratio: float = 0.64,
    radius_ratio: float = 0.225,
    shadow: bool = True,
) -> Image.Image:
    """白色圆角方块 + 图形，用于应用图标 / 页签图标。"""
    canvas = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    pad = round(size * 0.055)
    box = (pad, pad, size - pad, size - pad)
    radius = round(size * radius_ratio)

    if shadow:
        shadow_layer = Image.new("RGBA", (size, size), (0, 0, 0, 0))
        draw = ImageDraw.Draw(shadow_layer)
        drop = max(1, round(size * 0.012))
        draw.rounded_rectangle(
            (box[0], box[1] + drop, box[2], box[3] + drop),
            radius=radius,
            fill=(8, 26, 60, 46),
        )
        canvas.alpha_composite(shadow_layer.filter(ImageFilter.GaussianBlur(size * 0.016)))

    draw = ImageDraw.Draw(canvas)
    draw.rounded_rectangle(
        box,
        radius=radius,
        fill=(255, 255, 255, 255),
        outline=(16, 48, 96, 22),
        width=max(1, size // 512),
    )

    mark = resize_premul(master.crop(MARK_BOX), round(size * ratio))
    canvas.alpha_composite(mark, ((size - mark.width) // 2, (size - mark.height) // 2))
    return canvas


def main() -> None:
    for folder in (BRAND_DIR, PUBLIC_DIR, DESKTOP_DIR):
        folder.mkdir(parents=True, exist_ok=True)

    master = load_master()
    full = master.crop(FULL_BOX)
    # 紧凑版与完整版同框（高度由左侧图形决定），只是不含中文副标题。
    # 侧栏高度只有 39px，完整版里的副标题会缩到 3~4px，必须去掉。
    compact = master.crop(FULL_BOX)
    compact.paste((0, 0, 0, 0), (470, 280, compact.width, compact.height))

    outputs = {
        BRAND_DIR / "rise-logo.png": resize_premul(full, 800),
        BRAND_DIR / "rise-logo-on-dark.png": resize_premul(recolor_on_dark(full), 800),
        BRAND_DIR / "rise-logo-compact.png": resize_premul(compact, 480),
        BRAND_DIR / "rise-logo-compact-on-dark.png": resize_premul(recolor_on_dark(compact), 480),
        BRAND_DIR / "rise-mark.png": make_mark(master, 512, 0.92),
    }
    for path, image in outputs.items():
        image.save(path, optimize=True)
        print(f"wrote {path.relative_to(ROOT)} ({image.width}x{image.height})")

    # 应用图标：桌面端 1024
    app_icon = make_tile(master, 1024, ratio=0.64)
    app_icon.save(DESKTOP_DIR / "appicon.png", optimize=True)
    print(f"wrote {(DESKTOP_DIR / 'appicon.png').relative_to(ROOT)} (1024x1024)")

    # 页签图标：多尺寸 ICO
    favicon_master = make_tile(master, 512, ratio=0.86, radius_ratio=0.20, shadow=False)
    favicon_master.resize((256, 256), Image.LANCZOS).save(
        PUBLIC_DIR / "favicon.ico",
        format="ICO",
        sizes=[(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)],
    )
    print(f"wrote {(PUBLIC_DIR / 'favicon.ico').relative_to(ROOT)}")

    # iOS 主屏图标：不透明正方形，由系统自行切圆角
    touch = Image.new("RGBA", (180, 180), (255, 255, 255, 255))
    mark = resize_premul(master.crop(MARK_BOX), round(180 * 0.70))
    touch.alpha_composite(mark, ((180 - mark.width) // 2, (180 - mark.height) // 2))
    touch.convert("RGB").save(PUBLIC_DIR / "apple-touch-icon.png", optimize=True)
    print(f"wrote {(PUBLIC_DIR / 'apple-touch-icon.png').relative_to(ROOT)}")


if __name__ == "__main__":
    main()
