# ZDownloader build & package script
# Usage: powershell -ExecutionPolicy Bypass -File scripts\build.ps1 [-NoUI] [-UIVariant fyne|wails] [-CrossCompile] [-SkipTests]
param(
    [switch]$CrossCompile,  # also produce linux / macOS binaries
    [switch]$SkipTests,     # 跳过 Go 测试套件(纯前端/文档改动用;改核心代码请务必跑全套)
    # NoUI/Headless:只构建引擎 zdl(纯 Go 无头,零前端依赖)。zdl_shell 与 zdl_ui
    # 是不可拆分的 UI 对(shell 唤 ui 常驻、ui 依赖 shell 持久化),要么整对编要么
    # 整对不编,故"无界面"= 只出引擎,连带跳过 shell、ui 及前端(vite)/图标。
    [switch]$NoUI,
    # 桌面窗口技术栈,二选一,产物同名 zdl_ui.exe(谁最后构建谁即当前窗口——
    # 常驻组件按固定名 zdl_ui.exe 唤窗,不感知其技术栈):
    #   fyne  = Fyne 原生窗口(默认,跨平台)
    #   wails = Wails/WebView2 窗口(仅 Windows;须带 production 标签,脚本已固化)
    [ValidateSet("fyne","wails")][string]$UIVariant = "fyne"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$core = Join-Path $root "core"
$dist = Join-Path $root "dist"
$version = "2.0.0"
# 产物目录(与 mac 侧 build.sh 同名规则):旧单体与三件套共用一个版本化目录
$outDir = Join-Path $dist ("ZDownloader-v{0}-windows-amd64" -f $version)

# 组件模型:引擎 zdl 恒编;zdl_shell 与 zdl_ui 是不可拆分的 UI 对(shell 唤 ui 常驻、
# ui 依赖 shell 持久化),整对同编/同不编。-NoUI 时整对不编(只出引擎)。前端产物
# (vite/图标字体、core/web、.syso)仅 UI 消费,故 $wantWeb 与 $wantUI 同起同落;
# 引擎(zdl)零前端依赖,无头构建可整段跳过前端与图标。
$wantUI    = -not $NoUI
$wantShell = $wantUI
$wantZdl   = $true
$wantWeb   = $wantUI
Write-Host ("== 组件: zdl={0} zdl_shell={1} zdl_ui={2} (前端/图标={3}) ==" -f $wantZdl, $wantShell, $wantUI, $wantWeb)

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
if ($wantWeb) {
    Write-Host "== Icon font subset =="
    Push-Location (Join-Path $webapp "scripts")
    try { python genfont.py } catch { Write-Host "genfont 失败(沿用已提交字体): $_" }
    if ($LASTEXITCODE -ne 0) { Write-Host "genfont 失败(沿用已提交字体)" }
    Pop-Location
}

# Vue 前端构建(vue 分支起 UI 源码在 core/webapp,产物写入 core/web 供 go:embed;
# 产物带内容哈希文件名,core/web 里不再有 app.js/style.css/fonts 旧文件)。
# 仅当要编 zdl_ui($wantWeb)时执行:无头产物(zdl/zdl_shell)零前端依赖,可整段跳过。
if ($wantWeb -and (Test-Path (Join-Path $webapp "package.json"))) {
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
# app.ico/.syso 仅 zdl_ui 消费;zdl、zdl_shell 无图标资源,故仅在编 UI 时重建
if ($wantUI -and (Test-Path $rootIcon)) {
    Write-Host "== Regenerate icons from icon.png =="
    Push-Location $core
    try { & $go run ./scripts/genicons (Join-Path $root "extension\icons") (Join-Path $core "assets\app.ico") } catch { Write-Host "genicons failed: $_" }
    if ($LASTEXITCODE -ne 0) { Write-Host "genicons failed, using committed icons" }
    Pop-Location
}
# 应用图标:.syso(资源 ID 3,供窗口/任务栏与 Explorer 使用)在 app.ico 变更后重新生成;
# genico 是独立模块(构建期工具,依赖不进核心 go.mod),刷新失败时沿用仓库内已提交的 .syso
# Go 只自动链接"被编译包目录"下的 .syso:core 根那份供旧 Wails 构建,原生壳的窗口
# 是 cmd\zdl_ui 包,故每次生成/有更新后须同步一份到该目录,否则 zdl_ui.exe 无图标资源
# (Windows 默认图);zdl/zdl_shell 无图标,无需同步。
$coreSyso = Join-Path $core "rsrc_windows_amd64.syso"
$uiSyso   = Join-Path $core "cmd\zdl_ui\rsrc_windows_amd64.syso"
$coreIco = Join-Path $core "assets\app.ico"
if ($wantUI -and (Test-Path $coreIco) -and ((-not (Test-Path $coreSyso)) -or ((Get-Item $coreIco).LastWriteTime -gt (Get-Item $coreSyso).LastWriteTime))) {
    Write-Host "== Regenerate icon syso =="
    Push-Location (Join-Path $core "scripts\genico")
    try { & $go run . } catch { Write-Host "genico skipped: $_" }
    if ($LASTEXITCODE -ne 0) { Write-Host "genico failed, using committed syso" }
    Pop-Location
}
if ($wantUI -and (Test-Path $coreSyso) -and ((-not (Test-Path $uiSyso)) -or ((Get-Item $coreSyso).LastWriteTime -gt (Get-Item $uiSyso).LastWriteTime))) {
    # 同步失败只降级(沿用 cmd\zdl_ui 已提交的 syso),不能终止整个构建——
    # 图标子步是锦上添花,绝不能因它漏编三个 exe(曾因此整段构建中止)。
    try { Copy-Item $coreSyso $uiSyso -Force } catch { Write-Host "syso 同步到 cmd\zdl_ui 失败,沿用已提交 syso: $_" }
}
# -H windowsgui: Windows 下无命令行窗口,常驻托盘,日志写 data\zdownloader.log
# 旧 1.8 Wails 单体(zdownloader,根包)已移除,入口迁到 cmd/{zdl,zdl_shell,zdl_ui};
# 干净检出的 core/ 根目录没有任何 Go 文件,若仍对根包执行 go build .,会在
# ErrorActionPreference=Stop 下以 "no Go files" 中止整个构建(曾因此三件套漏编、
# 产物停留旧版)。故先探测根包是否可编译,不可则整段跳过旧单体的
# 构建/交叉编译/打包,只产出三件套。
$hasLegacyApp = (& $go list -e -f "{{if .GoFiles}}yes{{end}}" . 2>$null) -eq "yes"
if ($hasLegacyApp) {
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null
    & $go build -trimpath -tags "desktop,production" -ldflags "-s -w -H windowsgui" -o (Join-Path $outDir "zdownloader.exe") .
    # Wails 桌面版必须带 desktop,production 构建标签(裸 go build 时 wails CLI 不会代加)
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "Build failed" }
}

# 三件套(cmd/zdl + cmd/zdl_shell + cmd/zdl_ui):zdl=无头引擎(纯 Go,
# 零 cgo,无图标);zdl_shell=常驻组件(托盘/悬浮窗,纯 Win32,无图标);
# zdl_ui=可替换 UI 模块(窗口技术栈 Fyne/Wails 二选一,见下方 -UIVariant,
# 两者产物同名 zdl_ui.exe)。字体/图标子集已提交到 internal/assets,无需额外生成步骤。
# 内置管理界面:引擎经 os.DirFS(同目录 web/)对浏览器开放,与桌面端共用
# 同一套 API(旧 Wails 壳退役后 web 不再 go:embed,改为文件夹分发,可独立更新)
$webDist = Join-Path $outDir "web"
# web/ 仅 UI/浏览器管理界面消费;无头产物(不编 zdl_ui)跳过整段拷贝
if ($wantWeb -and (Test-Path (Join-Path $core "web\index.html"))) {
    Write-Host "== Copy web UI -> dist =="
    if (Test-Path $webDist) { Remove-Item $webDist -Recurse -Force }
    Copy-Item (Join-Path $core "web") $webDist -Recurse
}

if ($wantZdl) {
    Write-Host "== Build zdl (headless engine) =="
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null
    & $go build -trimpath -ldflags "-s -w -H windowsgui" -o (Join-Path $outDir "zdl.exe") ./cmd/zdl
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "zdl build failed" }
}
if ($wantShell) {
    Write-Host "== Build zdl_shell (resident tray) =="
    & $go build -trimpath -ldflags "-s -w -H windowsgui" -o (Join-Path $outDir "zdl_shell.exe") ./cmd/zdl_shell
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "zdl_shell build failed" }
}
# 桌面窗口模块 zdl_ui.exe:技术栈二选一(Fyne 默认 / Wails·WebView2),两者产物同名
# zdl_ui.exe——常驻组件按固定名 zdl_ui.exe 唤窗、不感知其技术栈,故谁最后编进 dist
# 谁即当前窗口。Wails 变体必须带 "wails,production" 标签:缺 production 会编进 Wails
# 的"非生产桩",运行时弹 "will not build without the correct build tags" 且窗口不显示
# 界面(裸 go build 时 wails CLI 不会代加此标签,故在此固化,杜绝漏 tag 手滑)。
if ($wantUI) {
    if ($UIVariant -eq "wails") {
        Write-Host "== Build zdl_ui (Wails/WebView2 窗口; -tags wails,production) =="
        & $go build -trimpath -tags "wails,production" -ldflags "-s -w -H windowsgui" -o (Join-Path $outDir "zdl_ui.exe") ./cmd/zdl_ui
    } else {
        Write-Host "== Build zdl_ui (Fyne 窗口, 默认) =="
        & $go build -trimpath -ldflags "-s -w -H windowsgui" -o (Join-Path $outDir "zdl_ui.exe") ./cmd/zdl_ui
    }
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "zdl_ui build failed" }
}

# 无根包单体可交叉编译;-CrossCompile 仅对旧 Wails 单体生效
if ($CrossCompile -and $hasLegacyApp) {
    $env:GOOS = "linux";   $env:GOARCH = "amd64"
    & $go build -trimpath -ldflags "-s -w" -o (Join-Path $outDir "zdownloader-linux-amd64") .
    $env:GOOS = "darwin";  $env:GOARCH = "arm64"
    & $go build -trimpath -ldflags "-s -w" -o (Join-Path $outDir "zdownloader-macos-arm64") .
    Remove-Item Env:GOOS, Env:GOARCH
}
Pop-Location

if ($hasLegacyApp) {
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
} else {
    # 三件套打包:与 mac 侧 build.sh 同构——装配 extension/README/LICENSE 后
    # 从暂存目录打 zip,排除用户数据 data/(日志可能被运行中的实例锁定,
    # 且不属于发布物);按实际编出的组件动态回显,无头(-NoUI)时不再是"三件套"
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
    foreach ($exe in @("zdl.exe", "zdl_shell.exe", "zdl_ui.exe")) {
        Remove-Item (Join-Path $outDir ($exe + "~")) -ErrorAction SilentlyContinue
    }

    $zip = "$outDir.zip"
    if (Test-Path $zip) { Remove-Item $zip }
    $stage = Join-Path $env:TEMP ("ZDownloader-v" + $version + "-windows-amd64")
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory -Path $stage | Out-Null
    Get-ChildItem $outDir | Where-Object { $_.Name -ne "data" } | ForEach-Object {
        Copy-Item -Recurse -Force $_.FullName (Join-Path $stage $_.Name)
    }
    Compress-Archive -Path $stage -DestinationPath $zip
    Remove-Item -Recurse -Force $stage

    $built = @()
    if ($wantZdl)   { $built += "zdl.exe" }
    if ($wantShell) { $built += "zdl_shell.exe" }
    if ($wantUI)    { $built += "zdl_ui.exe"; $built += "web/" }
    Write-Host ("== 构建完成(组件: {0}) ==" -f ($built -join " / "))
    Write-Host "Done: $outDir"
    Write-Host "      $zip"
}
