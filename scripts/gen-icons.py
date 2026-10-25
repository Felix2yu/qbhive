#!/usr/bin/env python3
"""QBHive 图标资源生成器：单一母版 → web/static 下的 SVG / PNG / ICO。

用法：
    python3 scripts/gen-icons.py      # 等价于 make icons

几何常量全部集中在本文件顶部，改配色或比例后重跑即可，不要手改生成的位图。

依赖：
    1. 无头 Chromium：按 $QBHIVE_CHROME → Playwright 缓存 → 系统 Chrome 的顺序查找
       （ImageMagick 自带渲染器不支持 SVG 渐变，会把六边形画成纯黑，故不走它）
    2. ImageMagick 7 的 magick：小尺寸降采样与 ico 打包
"""

from __future__ import annotations

import math
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
STATIC = ROOT / "web" / "static"

# ===== 几何与配色（唯一事实来源） =====
CX = CY = 256.0          # 画布 512×512，标记居中
HEX_R = 208.0            # 六边形外接圆半径
HEX_STROKE = 40.0        # 六边形描边宽度（round join 负责圆角）
BOLT_STROKE = 18.0       # 闪电描边宽度（仅用于圆角过渡）
BOLT = [(269, 126), (139, 282), (256, 282), (243, 386), (373, 230), (256, 230)]
INK = "#1d1503"          # 闪电颜色（与前端 --accent-ink 暗色值一致）
GRAD = [(0.0, "#ffd77a"), (0.45, "#f2b544"), (1.0, "#e0951a")]  # 蜂蜡琥珀主色
TILE = [(0.0, "#181c22"), (1.0, "#0a0c0f")]                     # 应用图标底板

# 应用图标：标记占画布的比例；maskable 需留在 80% 圆安全区内，故更小
TILE_MARK_SCALE = 0.86
MASKABLE_MARK_SCALE = 0.64

ICO_SIZES = (16, 32, 48)


def hex_path() -> str:
    dx, dy = HEX_R, HEX_R * math.sqrt(3) / 2
    pts = [
        (CX + dx, CY),
        (CX + dx / 2, CY + dy),
        (CX - dx / 2, CY + dy),
        (CX - dx, CY),
        (CX - dx / 2, CY - dy),
        (CX + dx / 2, CY - dy),
    ]
    return "M " + " L ".join(f"{x:.2f} {y:.2f}" for x, y in pts) + " Z"


def bolt_path() -> str:
    return "M " + " L ".join(f"{x:.2f} {y:.2f}" for x, y in BOLT) + " Z"


def gradient(id_: str, stops, x1: float, y1: float, x2: float, y2: float) -> str:
    body = "".join(
        f'\n      <stop offset="{off:g}" stop-color="{color}"/>' for off, color in stops
    )
    return (
        f'<linearGradient id="{id_}" x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}"'
        f' gradientUnits="userSpaceOnUse">{body}\n    </linearGradient>'
    )


def mark_defs() -> str:
    return gradient("qbHex", GRAD, 72, 56, 440, 456)


def mark_body() -> str:
    return (
        f'<path d="{hex_path()}" fill="url(#qbHex)" stroke="url(#qbHex)"'
        f' stroke-width="{HEX_STROKE:g}" stroke-linejoin="round"/>\n'
        f'  <path d="{bolt_path()}" fill="{INK}" stroke="{INK}"'
        f' stroke-width="{BOLT_STROKE:g}" stroke-linejoin="round" stroke-linecap="round"/>'
    )


def mark_svg(px: int | None = None) -> str:
    size = f' width="{px}" height="{px}"' if px else ""
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"{size}'
        f' role="img" aria-label="QBHive">\n'
        f'  <title>QBHive</title>\n'
        f"  <defs>\n    {mark_defs()}\n  </defs>\n"
        f"  {mark_body()}\n</svg>\n"
    )


def tile_svg(scale: float, px: int | None = None) -> str:
    size = f' width="{px}" height="{px}"' if px else ""
    tx = CX * (1 - scale)
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512"{size}>\n'
        f"  <defs>\n"
        f"    {gradient('qbBg', TILE, 0, 0, 512, 512)}\n"
        f"    {mark_defs()}\n"
        f"  </defs>\n"
        f'  <rect width="512" height="512" fill="url(#qbBg)"/>\n'
        f'  <g transform="translate({tx:.2f} {tx:.2f}) scale({scale:g})">\n'
        f"    {mark_body()}\n  </g>\n</svg>\n"
    )


def find_chrome() -> str:
    cached = []
    for base in (
        Path.home() / "Library" / "Caches" / "ms-playwright",
        Path.home() / ".cache" / "ms-playwright",
    ):
        cached += sorted(base.glob("chromium_headless_shell-*/chrome-headless-shell-*/*"))
        cached += sorted(base.glob("chromium-*/chrome-*/Google Chrome for Testing.app/Contents/MacOS/*"))
        cached += sorted(base.glob("chromium-*/chrome-*/chrome"))
    candidates = [
        os.environ.get("QBHIVE_CHROME", ""),
        str(cached[-1]) if cached else "",
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
        "/usr/bin/google-chrome",
        "/usr/bin/chromium",
        "/usr/bin/chromium-browser",
    ]
    for path in candidates:
        if path and Path(path).exists():
            return path
    sys.exit("找不到无头 Chromium；请设置 QBHIVE_CHROME 指向可执行文件后重试")


def render(svg: str, px: int, out: Path, chrome: str) -> None:
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "icon.svg"
        src.write_text(svg.replace('viewBox="0 0 512 512"', f'width="{px}" height="{px}" viewBox="0 0 512 512"'), encoding="utf-8")
        proc = subprocess.run(
            [
                chrome,
                "--headless",
                "--disable-gpu",
                "--hide-scrollbars",
                "--no-first-run",
                "--default-background-color=00000000",
                f"--window-size={px},{px}",
                f"--screenshot={out}",
                src.as_uri(),
            ],
            capture_output=True,
            text=True,
        )
    if not out.exists():
        sys.exit(f"渲染失败（{out.name}）：{proc.stderr.strip()[-400:]}")


def resize(src: Path, size: int, out: Path) -> None:
    subprocess.run(
        ["magick", str(src), "-filter", "Lanczos", "-resize", f"{size}x{size}", str(out)],
        check=True,
    )


def main() -> None:
    chrome = find_chrome()
    if not shutil.which("magick"):
        sys.exit("找不到 magick（ImageMagick 7），无法缩放与打包 ico")

    STATIC.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp:
        tmpdir = Path(tmp)
        master = tmpdir / "mark@1024.png"
        render(mark_svg(), 1024, master, chrome)
        tile = tmpdir / "tile@1024.png"
        render(tile_svg(TILE_MARK_SCALE), 1024, tile, chrome)
        maskable = tmpdir / "maskable@1024.png"
        render(tile_svg(MASKABLE_MARK_SCALE), 1024, maskable, chrome)

        # 矢量母版：现代浏览器优先取它，任意缩放都清晰
        (STATIC / "icon.svg").write_text(mark_svg(), encoding="utf-8")

        # iOS/PWA 桌面图标要求不透明，统一用深色底板版本
        pngs = [(tile, 180, "apple-touch-icon.png"), (tile, 192, "icon-192.png"),
                (tile, 512, "icon-512.png"), (maskable, 512, "icon-maskable-512.png")]
        for src, size, name in pngs:
            resize(src, size, STATIC / name)

        # favicon：16/32/48 三档打进同一个 ico，Windows 与老浏览器兼容
        ico_parts = []
        for size in ICO_SIZES:
            part = tmpdir / f"fav{size}.png"
            resize(master, size, part)
            ico_parts.append(str(part))
        subprocess.run(["magick", *ico_parts, str(STATIC / "favicon.ico")], check=True)

    print("已生成：")
    for name in ("icon.svg", "favicon.ico", "apple-touch-icon.png", "icon-192.png",
                 "icon-512.png", "icon-maskable-512.png"):
        path = STATIC / name
        print(f"  {path.relative_to(ROOT)}  {path.stat().st_size:>7d} B")


if __name__ == "__main__":
    main()
