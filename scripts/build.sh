#!/usr/bin/env bash
# ZDownloader macOS 构建脚本(对应 Windows 侧 scripts/build.ps1)。
# v3 三件套:zdl(无头引擎,纯 Go 零 cgo)+ zdl_shell(常驻托盘/悬浮窗)+
# zdl_ui(窗口模块,技术栈二选一:默认 wails(WKWebView,macOS 系统 webview,
# 内存占用低于 Fyne 的 GL/GLFW 栈);-UIVariant fyne 出 Fyne 变体,两者产物
# 同名 zdl_ui,常驻组件按固定名唤窗、不感知技术栈)。
# Fyne/Wails/托盘/悬浮窗的 darwin 侧均为 cgo(Objective-C),必须在 Mac 本机
# 构建(CGO_ENABLED=1 默认)——交叉编译出的 darwin 二进制链接不过、跑不起来。
# 依赖:Xcode Command Line Tools、Go ≥ 1.25(brew install go)。
# 模块若需联网拉取,提前 export HTTP_PROXY/HTTPS_PROXY。
# 用法:build.sh [--skip-tests] [-NoUI] [-UIVariant wails|fyne]
set -euo pipefail

skip_tests=0
no_ui=0
ui_variant="wails"
while [ $# -gt 0 ]; do
    case "$1" in
        --skip-tests) skip_tests=1 ;;
        -NoUI|--no-ui) no_ui=1 ;;
        -UIVariant|--ui-variant) shift; ui_variant="${1:-}" ;;
        -UIVariant=*|--ui-variant=*) ui_variant="${1#*=}" ;;
        *) echo "未知参数: $1(支持 --skip-tests / -NoUI / -UIVariant fyne|wails)" >&2; exit 1 ;;
    esac
    shift
done
case "$ui_variant" in
    fyne|wails) ;;
    *) echo "-UIVariant 仅支持 fyne|wails(当前: $ui_variant)" >&2; exit 1 ;;
esac
# 组件模型与 build.ps1 一致:引擎 zdl 恒编;zdl_shell 与 zdl_ui 是不可拆分的
# UI 对(shell 唤 ui 常驻、ui 依赖 shell 持久化),整对同编/同不编。
# -NoUI 只出引擎(纯 Go 无头,零前端依赖)。
want_ui=$((1 - no_ui))

root="$(cd "$(dirname "$0")/.." && pwd)"
core="$root/core"
dist="$root/dist"
# 版本单一来源:server.go 的 const Version(与 build.ps1 的 $version 同值,发版三处联动)
version="$(sed -n 's/.*const Version = "\(.*\)".*/\1/p' "$core/internal/server/server.go")"
if [ -z "$version" ]; then echo "无法从 server.go 解析 Version" >&2; exit 1; fi
arch="$(uname -m)"; [ "$arch" = "x86_64" ] && arch=amd64
out="$dist/ZDownloader-v$version-darwin-$arch"

cd "$core"
echo "== go version =="
go version
echo "== 组件: zdl=on zdl_shell=$([ "$want_ui" = 1 ] && echo on || echo off) zdl_ui=$([ "$want_ui" = 1 ] && echo "on($ui_variant)" || echo off) =="

# 图标字体子集:MS_ICONS(webapp/src/lib/icons.js)为唯一事实源,新增图标后
# 由本步重新生成 woff2。依赖 python3 + fonttools/brotli,首次联网下载全量
# 字体缓存到 webapp/scripts/.cache(--font 可指定本地 ttf 离线生成)。
# 失败不阻断构建:沿用已提交的 woff2。仅编 UI 组件时需要(浏览器管理界面/悬浮窗页面)。
if [ "$want_ui" = 1 ]; then
    echo "== Icon font subset =="
    (cd "$core/webapp/scripts" && { python3 genfont.py || python genfont.py; }) \
        || echo "genfont failed, 沿用已提交字体" >&2

    echo "== Web (vite build) =="
    # Vue 前端构建(UI 源码在 core/webapp,产物写入 core/web):引擎经
    # os.DirFS(同目录 web/)对浏览器开放管理界面,悬浮窗页面(float.html)
    # 亦由引擎供给。产物带内容哈希文件名,float.html 经 webapp/public 原样
    # 拷贝、不受 emptyOutDir 清空影响。
    if [ -f "$core/webapp/package.json" ]; then
        if [ ! -d "$core/webapp/node_modules" ]; then
            echo "== npm install =="
            (cd "$core/webapp" && npm install)
        fi
        (cd "$core/webapp" && OUT_DIR="$core/web" npm run build)
    fi
fi

echo "== go vet =="
go vet ./...

if [ "$skip_tests" = 0 ]; then
    echo "== go test =="
    # Mac 上无需 Windows 侧的 testbin 绕防火墙套路,直接跑
    go test ./...
fi

# 主图标:仓库根 icon.png 为唯一源,重建扩展图标与 Windows 多尺寸 app.ico
# (macOS 的 .icns 在下方 bundle 步骤由 genicns 同源生成;换主图标 = 替换 icon.png 后重新构建)
if [ -f "$root/icon.png" ]; then
    echo "== genicons(主图标 → 扩展图标 / app.ico) =="
    (cd "$core" && go run ./scripts/genicons "$root/extension/icons" "$core/assets/app.ico") \
        || echo "genicons failed, 沿用仓库内已提交图标" >&2
fi

echo "== build v3 三件套 =="
# Windows 的 -H windowsgui 在 Mac 无对应物,不需要
mkdir -p "$out"
go build -trimpath -ldflags "-s -w" -o "$out/zdl" ./cmd/zdl

if [ "$want_ui" = 1 ]; then
    go build -trimpath -ldflags "-s -w" -o "$out/zdl_shell" ./cmd/zdl_shell
    # zdl_ui 窗口技术栈二选一(Fyne 默认 / Wails·WKWebView;WebView2 是 Windows
    # 运行时,mac 的 Wails 变体走 WKWebView)。两者产物同名 zdl_ui——常驻组件
    # 按固定名唤窗、不感知技术栈,谁最后编进 dist 谁生效。Wails 分支固化
    # -tags "wails,production"(防"非生产桩"漏 tag 弹窗)。
    if [ "$ui_variant" = "wails" ]; then
        go build -trimpath -tags "wails,production" -ldflags "-s -w" -o "$out/zdl_ui" ./cmd/zdl_ui
    else
        go build -trimpath -ldflags "-s -w" -o "$out/zdl_ui" ./cmd/zdl_ui
    fi
fi

echo "== assemble dist =="
# 清理 1.8 布局遗留(旧脚本产物 zdownloader 单体二进制),避免与 v3 三件套混装
rm -f "$out/zdownloader" "$out/zdownloader~"
rm -rf "$out/extension"
cp -R "$root/extension" "$out/extension"
cp "$root/README.md" "$out/"
cp "$root/README.zh-CN.md" "$out/"
cp "$root/LICENSE" "$out/"

if [ "$want_ui" = 1 ]; then
    # 内置管理界面/悬浮窗页面:引擎经 os.DirFS(同目录 web/)对浏览器开放
    if [ -f "$core/web/index.html" ]; then
        echo "== Copy web UI -> dist =="
        rm -rf "$out/web"
        cp -r "$core/web" "$out/web"
    fi
fi

# .app bundle:Finder 双击/通知归属/magnet 与 .torrent 关联都依赖它。
# CFBundleExecutable=zdl_ui(v3 三件套的入口 exe:双击/关联/托盘召唤都汇聚
# 于此),三二进制 + web/ 同入 bundle 互拉(sibling 按同目录固定名)。
# 裸二进制保留在 bundle 同级供开发直跑(数据目录跟随 exe 所在目录;
# bundle 内运行则自动落用户配置目录,见各 cmd main.go 的 defaultDataDir)。
if [ "$want_ui" = 1 ]; then
    app="$out/ZDownloader.app"
    mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
    cp "$out/zdl_ui" "$out/zdl_shell" "$out/zdl" "$app/Contents/MacOS/"
    # web/ 随 Resources 走:codesign 要求 Contents/MacOS/ 下只容 Mach-O 可执行,
    # 网页资源放 MacOS/ 会被拒签("code object is not signed at all")
    if [ -d "$out/web" ]; then
        cp -R "$out/web" "$app/Contents/Resources/web"
    fi
    # icns:仓库根 icon.png 缩放出全尺寸 iconset → iconutil 打包(与扩展图标同源)
    (cd "$core" && go run ./scripts/genicns "$out/AppIcon.iconset" > /dev/null)
    iconutil -c icns "$out/AppIcon.iconset" -o "$app/Contents/Resources/ZDownloader.icns"
    rm -rf "$out/AppIcon.iconset"
    cat > "$app/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>ZDownloader</string>
  <key>CFBundleDisplayName</key><string>ZDownloader</string>
  <key>CFBundleIdentifier</key><string>com.zdownloader</string>
  <key>CFBundleExecutable</key><string>zdl_ui</string>
  <key>CFBundleIconFile</key><string>ZDownloader</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$version</string>
  <key>CFBundleVersion</key><string>$version</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>CFBundleURLTypes</key>
  <array>
    <dict>
      <key>CFBundleURLName</key><string>com.zdownloader.magnet</string>
      <key>CFBundleURLSchemes</key><array><string>magnet</string></array>
    </dict>
  </array>
  <key>CFBundleDocumentTypes</key>
  <array>
    <dict>
      <key>CFBundleTypeName</key><string>BitTorrent 文件</string>
      <key>CFBundleTypeRole</key><string>Viewer</string>
      <key>LSItemContentTypes</key><array><string>org.bittorrent.torrent</string></array>
      <key>CFBundleTypeExtensions</key><array><string>torrent</string></array>
    </dict>
  </array>
  <key>UTImportedTypeDeclarations</key>
  <array>
    <dict>
      <key>UTTypeIdentifier</key><string>org.bittorrent.torrent</string>
      <key>UTTypeDescription</key><string>BitTorrent 文件</string>
      <key>UTTypeConformsTo</key><array><string>public.data</string></array>
      <key>UTTypeTagSpecification</key>
      <dict><key>public.filename-extension</key><array><string>torrent</string></array></dict>
    </dict>
  </array>
</dict>
</plist>
EOF
    plutil -lint "$app/Contents/Info.plist" > /dev/null || { echo "Info.plist 校验失败" >&2; exit 1; }
    # 双保险:bundle 内绝不允许出现运行时数据(bundle 内运行走用户配置目录)
    rm -rf "$app/Contents/MacOS/data"
    # ad-hoc 签名:本机构建无开发者证书,ad-hoc 保持通知/TCC 归属稳定;
    # 失败即中断发布——静默放过的未签名包会被 Gatekeeper/TCC 反复重新弹窗。
    # 三件套(bundle 内三个 Mach-O):codesign 把嵌套二进制视作子组件要求
    # 先签,逐个签完再签总包
    for exe in zdl zdl_shell zdl_ui; do
        codesign --force -s - "$app/Contents/MacOS/$exe"
    done
    codesign --force -s - "$app"
fi

zip="$out.zip"
rm -f "$zip"
# 从暂存目录打包并排除用户数据 data/(日志可能被运行中的实例持有,且不属于发布物)
stage="/tmp/ZDownloader-v$version-darwin-$arch"
rm -rf "$stage"; mkdir -p "$stage"
for f in "$out"/*; do
    [ "$(basename "$f")" = "data" ] && continue
    cp -R "$f" "$stage/"
done
# 双保险:zip 里绝不允许出现运行时数据
rm -rf "$stage/ZDownloader.app/Contents/MacOS/data"
(cd "$(dirname "$stage")" && zip -qr "$zip" "$(basename "$stage")")
rm -rf "$stage"

echo "Done: $out"
echo "      $zip"
