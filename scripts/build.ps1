# ZDownloader build & package script
# Usage: powershell -ExecutionPolicy Bypass -File scripts\build.ps1 [-CrossCompile] [-SkipTests]
param(
    [switch]$CrossCompile,  # also produce linux / macOS binaries
    [switch]$SkipTests      # 跳过 Go 测试套件(纯前端/文档改动用;改核心代码请务必跑全套)
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$core = Join-Path $root "core"
$dist = Join-Path $root "dist"
$version = "1.8.0"

# Locate Go: prefer the user toolchain (~\tools\go, 1.27+), then PATH, then Program Files
$go = $null
$candidates = @(
    (Join-Path $env:USERPROFILE "tools\go\bin\go.exe"),
    (Join-Path $env:ProgramFiles "Go\bin\go.exe")
)
foreach ($c in $candidates) {
    if (Test-Path $c) { $go = $c; break }
}
if (-not $go) {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { $go = $cmd.Source }
}
if (-not $go) { throw "Go toolchain not found. Install Go 1.22+ or extract it to ~\tools\go" }
Write-Host "== Go: $go =="
& $go version

# Web 构建必须在 vet/test 之前:core/web/(go:embed 嵌入根)已改为不入库、
# 构建时生成,干净检出上先 vet 会因 pattern web 无文件而直接失败。
# 顺序:genfont(重生成 src/fonts 子集)→ vite(产出 core/web)→ vet/test。

# 图标字体子集:MS_ICONS(webapp/src/lib/icons.js)为唯一事实源,新增图标后
# 由本步重新生成 woff2。依赖 python3 + fonttools/brotli,首次联网下载全量
# 字体缓存到 webapp/scripts/.cache(可用 --font 指定本地 ttf 离线生成)。
# 失败不阻断构建:沿用已提交的 woff2。
$webapp = Join-Path $core "webapp"
Write-Host "== Icon font subset =="
Push-Location (Join-Path $webapp "scripts")
try { python genfont.py } catch { Write-Host "genfont 失败(沿用已提交字体): $_" }
if ($LASTEXITCODE -ne 0) { Write-Host "genfont 失败(沿用已提交字体)" }
Pop-Location

# Vue 前端构建(vue 分支起 UI 源码在 core/webapp,产物写入 core/web 供 go:embed;
# 产物带内容哈希文件名,core/web 里不再有 app.js/style.css/fonts 旧文件)
if (Test-Path (Join-Path $webapp "package.json")) {
    Write-Host "== Web (vite build) =="
    Push-Location $webapp
    try {
        if (-not (Test-Path (Join-Path $webapp "node_modules"))) {
            & npm install
            if ($LASTEXITCODE -ne 0) { throw "npm install failed" }
        }
        $env:OUT_DIR = Join-Path $core "web"
        & npm run build
        if ($LASTEXITCODE -ne 0) { throw "vite build failed" }
    } finally {
        Remove-Item Env:OUT_DIR -ErrorAction SilentlyContinue
        Pop-Location
    }
}

Write-Host "== Test =="
Push-Location $core
& $go vet ./...
if ($SkipTests) {
    Write-Host "== Tests skipped (-SkipTests) =="
} else {
# 固定路径测试二进制:裸 go test 每次生成新临时 exe,Windows 防火墙会逐路径弹窗。
# 逐包编译到 testbin\ 下再运行,路径恒定即不再弹窗。
$testbin = Join-Path $core "testbin"
New-Item -ItemType Directory -Force -Path $testbin | Out-Null
$pkgs = & $go list ./...
foreach ($p in $pkgs) {
    # 只跑含测试文件的包
    $hasTests = & $go list -f "{{if or .TestGoFiles .XTestGoFiles}}yes{{end}}" $p
    if (-not $hasTests) { continue }
    $name = ($p -replace "^zdownloader[/\\]", "" -replace "[/\\]", "_")
    $exe = Join-Path $testbin "$name.exe"
    & $go test -c -o $exe $p
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "测试编译失败: $p" }
    # PowerShell 会把 -test.timeout 拆成 -test,须整体加引号并用 = 传值
    & $exe "-test.timeout=300s"
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "Tests failed: $p" }
}
}
# 此处不 Pop:后续 Build 步骤仍需 cwd=core,由原脚本的 Pop-Location 统一收尾
# 主图标:仓库根 icon.png 为唯一源,重建扩展图标与多尺寸 app.ico
# (换主图标 = 替换 icon.png 后重新构建,无需手动跑工具;genicons 零第三方依赖)
$rootIcon = Join-Path $root "icon.png"
if (Test-Path $rootIcon) {
    Write-Host "== Regenerate icons from icon.png =="
    Push-Location $core
    try { & $go run ./scripts/genicons (Join-Path $root "extension\icons") (Join-Path $core "assets\app.ico") } catch { Write-Host "genicons failed: $_" }
    if ($LASTEXITCODE -ne 0) { Write-Host "genicons failed, using committed icons" }
    Pop-Location
}
# 应用图标:.syso(资源 ID 3,供 wails 窗口/任务栏与 Explorer 使用)在 app.ico 变更后重新生成;
# genico 是独立模块(构建期工具,依赖不进核心 go.mod),刷新失败时沿用仓库内已提交的 .syso
$coreSyso = Join-Path $core "rsrc_windows_amd64.syso"
$coreIco = Join-Path $core "assets\app.ico"
if ((Test-Path $coreIco) -and ((-not (Test-Path $coreSyso)) -or ((Get-Item $coreIco).LastWriteTime -gt (Get-Item $coreSyso).LastWriteTime))) {
    Write-Host "== Regenerate icon syso =="
    Push-Location (Join-Path $core "scripts\genico")
    try { & $go run . } catch { Write-Host "genico skipped: $_" }
    if ($LASTEXITCODE -ne 0) { Write-Host "genico failed, using committed syso" }
    Pop-Location
}
# -H windowsgui: Windows 下无命令行窗口,常驻托盘,日志写 data\zdownloader.log
$outDir = Join-Path $dist "ZDownloader-v$version-windows-amd64"
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
& $go build -trimpath -tags "desktop,production" -ldflags "-s -w -H windowsgui" -o (Join-Path $outDir "zdownloader.exe") .
# Wails 桌面版必须带 desktop,production 构建标签(裸 go build 时 wails CLI 不会代加)
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "Build failed" }

if ($CrossCompile) {
    $env:GOOS = "linux";   $env:GOARCH = "amd64"
    & $go build -trimpath -ldflags "-s -w" -o (Join-Path $outDir "zdownloader-linux-amd64") .
    $env:GOOS = "darwin";  $env:GOARCH = "arm64"
    & $go build -trimpath -ldflags "-s -w" -o (Join-Path $outDir "zdownloader-macos-arm64") .
    Remove-Item Env:GOOS, Env:GOARCH
}
Pop-Location

Write-Host "== Assemble dist =="
$extDst = Join-Path $outDir "extension"
if (Test-Path $extDst) { Remove-Item -Recurse -Force $extDst }
# Windows 删除是异步完成的(delete-pending):立即重建同名目录会静默失败,需等待名字释放
foreach ($i in 1..30) {
    if (-not (Test-Path $extDst)) { break }
    Start-Sleep -Milliseconds 100
}
Copy-Item -Recurse -Force (Join-Path $root "extension") $extDst
if (-not (Test-Path (Join-Path $extDst "manifest.json"))) { throw "extension copy failed (delete-pending race?)" }
Copy-Item -Force (Join-Path $root "README.md") $outDir
Copy-Item -Force (Join-Path $root "README.zh-CN.md") $outDir
Copy-Item -Force (Join-Path $root "LICENSE") $outDir
# go build 替换正在运行的 exe 时会留下备份文件,不属于发布物
Remove-Item (Join-Path $outDir "zdownloader.exe~") -ErrorAction SilentlyContinue

$zip = "$outDir.zip"
if (Test-Path $zip) { Remove-Item $zip }
# Zip 从暂存目录打包,排除用户数据 data/(日志可能被运行中的实例锁定,且不属于发布物)
$stage = Join-Path $env:TEMP ("ZDownloader-v" + $version + "-windows-amd64")
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path $stage | Out-Null
Get-ChildItem $outDir | Where-Object { $_.Name -ne "data" } | ForEach-Object {
    Copy-Item -Recurse -Force $_.FullName (Join-Path $stage $_.Name)
}
Compress-Archive -Path $stage -DestinationPath $zip
Remove-Item -Recurse -Force $stage

Write-Host "Done: $outDir"
Write-Host "      $zip"
