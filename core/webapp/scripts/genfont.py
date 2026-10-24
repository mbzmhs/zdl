#!/usr/bin/env python3
"""genfont.py — 重新生成图标字体子集 src/fonts/ms-rounded.woff2。

子集列表的唯一事实源是 src/lib/icons.js 的 MS_ICONS(图标名→码位):
新增图标 = 在 MS_ICONS 加一条,重跑本脚本。码位不做输入,只做核对——
与全量字体 cmap 反查结果不一致即失败,防止表与字体漂移。

全量字体(Material Symbols Rounded,Apache 2.0)自动下载到 .cache/;
可用 --font 指定本地 ttf 离线生成。依赖: pip install fonttools brotli。

用法(在 webapp/scripts 目录):
    python genfont.py                 # 缓存/下载全量字体并生成
    python genfont.py --font X.ttf    # 用本地全量字体
"""
import argparse
import os
import re
import sys
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ICONS_JS = os.path.join(HERE, "..", "src", "lib", "icons.js")
OUT_WOFF2 = os.path.join(HERE, "..", "src", "fonts", "ms-rounded.woff2")
CACHE = os.path.join(HERE, ".cache")
FONT_URL = ("https://github.com/google/material-design-icons/raw/master/"
            "variablefont/MaterialSymbolsRounded%5BFILL%2CGRAD%2Copsz%2Cwght%5D.ttf")
FONT_NAME = "MaterialSymbolsRounded.ttf"


def read_ms_icons():
    """解析 MS_ICONS:返回 [(name, codepoint)] 按文件序。"""
    src = open(ICONS_JS, encoding="utf-8").read()
    m = re.search(r"export const MS_ICONS = \{(.*?)\n\}", src, re.S)
    if not m:
        sys.exit("icons.js 里找不到 MS_ICONS")
    bs = chr(92)
    pairs = re.findall(r"([a-z_0-9]+):\s*" + chr(34) + bs + bs + r"u([0-9a-fA-F]{4})" + chr(34), m.group(1))
    if not pairs:
        sys.exit("MS_ICONS 解析结果为空")
    return [(n, int(c, 16)) for n, c in pairs]


def get_full_font(explicit):
    if explicit:
        return explicit
    os.makedirs(CACHE, exist_ok=True)
    path = os.path.join(CACHE, FONT_NAME)
    if not os.path.exists(path) or os.path.getsize(path) < 1 << 20:
        print("下载全量字体(约 15MB,一次性缓存):", FONT_URL)
        urllib.request.urlretrieve(FONT_URL, path)
    return path


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--font", help="本地全量 MaterialSymbolsRounded.ttf(离线用)")
    args = ap.parse_args()

    from fontTools.subset import main as subset_main
    from fontTools.ttLib import TTFont

    icons = read_ms_icons()
    print(f"MS_ICONS 子集列表: {len(icons)} 个图标")

    font_path = get_full_font(args.font)
    font = TTFont(font_path)
    cmap = font.getBestCmap()
    by_name = {}
    for cp, gname in cmap.items():
        by_name.setdefault(gname, []).append(cp)

    # 码位核对:表里的码位必须与字体 cmap 一致
    bad = [(n, hex(c), [hex(x) for x in sorted(by_name.get(n, []))])
           for n, c in icons if n not in by_name or c not in by_name[n]]
    if bad:
        for n, given, have in bad:
            print(f"码位不符: {n} 表={given} 字体={have}", file=sys.stderr)
        sys.exit("MS_ICONS 码位与全量字体不一致,请按字体 cmap 修正表")

    cps = sorted({c for _, c in icons})
    subset_main([font_path,
                 "--unicodes=" + ",".join(hex(c)[2:] for c in cps),
                 "--flavor=woff2",
                 "--output-file=" + OUT_WOFF2,
                 "--no-hinting", "--desubroutinize"])

    sub = TTFont(OUT_WOFF2)
    submap = set(sub.getBestCmap().keys())
    missing = [hex(c) for c in cps if c not in submap]
    if missing:
        sys.exit(f"子集缺码位: {missing}")
    axes = [(a.axisTag, int(a.minValue), int(a.maxValue)) for a in sub["fvar"].axes]
    print(f"生成 {OUT_WOFF2}: {os.path.getsize(OUT_WOFF2)} bytes, {len(cps)} 码位, 轴 {axes}")


if __name__ == "__main__":
    main()
