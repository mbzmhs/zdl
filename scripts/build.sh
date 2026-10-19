#!/usr/bin/env bash
# ZDownloader macOS 构建脚本(对应 Windows 侧 scripts/build.ps1)。
# Wails darwin 前端是 cgo(Objective-C),必须在 Mac 本机构建(CGO_ENABLED=1 默认),
# 交叉编译出的 darwin 二进制链接不过、跑不起来。
# 依赖:Xcode Command Line Tools、Go ≥ 1.25(brew install go)。
# 模块若需联网拉取,提前 export HTTP_PROXY/HTTPS_PROXY。
set -euo pipefail

skip_tests=0
[ "${1:-}" = "--skip-tests" ] && skip_tests=1

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

echo "== go vet =="
go vet -tags "desktop,production" ./...

if [ "$skip_tests" = 0 ]; then
    echo "== go test =="
    # Mac 上无需 Windows 侧的 testbin 绕防火墙套路,直接跑
    go test ./...
fi

echo "== Web (vite build) =="
# Vue 前端构建(vue 分支起 UI 源码在 core/webapp,产物写入 core/web 供 go:embed;
# 产物带内容哈希文件名,float.html 经 webapp/public 原样拷贝、不受 emptyOutDir 清空影响)
if [ -f "$core/webapp/package.json" ]; then
    if [ ! -d "$core/webapp/node_modules" ]; then
        echo "== npm install =="
        (cd "$core/webapp" && npm install)
    fi
    (cd "$core/webapp" && OUT_DIR="$core/web" npm run build)
fi

# 主图标:仓库根 icon.png 为唯一源,重建扩展图标与 Windows 多尺寸 app.ico
# (macOS 的 .icns 在下方 bundle 步骤由 genicns 同源生成;换主图标 = 替换 icon.png 后重新构建)
if [ -f "$root/icon.png" ]; then
    echo "== genicons(主图标 → 扩展图标 / app.ico) =="
    (cd "$core" && go run ./scripts/genicons "$root/extension/icons" "$core/assets/app.ico") \
        || echo "genicons failed, 沿用仓库内已提交图标" >&2
fi

echo "== build =="
# Wails 桌面版必须带 desktop,production 构建标签(裸 go build 不会自动加);
# Windows 的 -H windowsgui 在 Mac 无对应物,不需要
mkdir -p "$out"
go build -trimpath -tags "desktop,production" -ldflags "-s -w" -o "$out/zdownloader" .

echo "== assemble dist =="
rm -rf "$out/extension"
cp -R "$root/extension" "$out/extension"
cp "$root/README.md" "$out/"
cp "$root/README.zh-CN.md" "$out/"
cp "$root/LICENSE" "$out/"
# go build 替换正在运行的二进制会留下 ~ 备份,不属于发布物
rm -f "$out/zdownloader~"

# .app bundle:Finder 图标/通知归属/magnet 与 .torrent 关联都依赖它;
# 裸二进制保留在同级供开发直跑(数据目录跟随 exe 所在目录)
app="$out/ZDownloader.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$out/zdownloader" "$app/Contents/MacOS/zdownloader"
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
  <key>CFBundleExecutable</key><string>zdownloader</string>
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
# 清理 bundle 内残留运行时数据(旧版本 .app 会把日志/任务写进 Contents/MacOS/data,
# 残留会使 codesign 递归签名失败;新版本运行时已改用用户配置目录,见 datadir_darwin.go)
rm -rf "$app/Contents/MacOS/data"
# ad-hoc 签名:本机构建无开发者证书,ad-hoc 保持通知/TCC 归属稳定;
# 失败即中断发布——静默放过的未签名包会被 Gatekeeper/TCC 反复重新弹窗
codesign --force -s - "$app"

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
