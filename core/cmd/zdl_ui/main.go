// ZDownloader 窗口进程(zdl_ui):Fyne 主窗口 + 引擎 API 客户端。三件套架构
// 中的"可替换 UI 模块":生命周期保障(引擎拉起、召唤、单实例仲裁)全部归
// 常驻组件 zdl_shell,本进程只管窗口与交互,可随时退出(内存开关);将来
// Wails 等其它 UI 实现可整体替换本 exe,只要讲同一套回环 API 语义。
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"

	"zdownloader/internal/conf"
)

// defaultDataDir 数据目录默认值(与 cmd/zdl、cmd/zdl_shell 同款):裸跑跟随
// exe 所在目录(文件夹分发形态,绿色便携);.app bundle 内运行(mac)改用
// 用户配置目录——往已签名 bundle 的 Contents/MacOS 写运行时数据既脏又在
// 升级时丢失,且与 1.8 mac 用户数据(~/Library/Application Support/ZDownloader)延续。
func defaultDataDir(exeDir string) string {
	if strings.Contains(exeDir, ".app/Contents/MacOS") {
		if base, e := os.UserConfigDir(); e == nil {
			return filepath.Join(base, "ZDownloader")
		}
	}
	return filepath.Join(exeDir, "data")
}

func main() {
	var (
		uiSettings = flag.Bool("settings", false, "启动即直达设置页(常驻组件拉起时传)")
		flagData   = flag.String("data", "", "数据目录(默认程序同级 data/;互拉时显式传递)")
		pprofPort  = flag.Int("pprof", 0, "诊断端口(127.0.0.1 专用;0=关闭)")
	)
	flag.Parse()

	os.Setenv("GODEBUG", "madvdontneed=1")

	exePath, err := os.Executable()
	if err != nil {
		log.Fatal("无法定位程序路径: ", err)
	}
	exeDir := filepath.Dir(exePath)
	dataDir := *flagData
	if dataDir == "" {
		dataDir = defaultDataDir(exeDir)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		base, e := os.UserConfigDir()
		if e != nil {
			log.Fatalf("数据目录不可用: %v", e)
		}
		dataDir = filepath.Join(base, "ZDownloader")
		_ = os.MkdirAll(dataDir, 0o755)
	}
	setupLogging(dataDir)

	cfg := conf.Load(filepath.Join(dataDir, "config.json"), "")

	// 文件关联入口:系统以 `<zdl_ui.exe> <种子路径|磁链>` 启动。目标由本进程
	// 编排落持久槽(见 uiEnter):确保引擎/常驻组件在跑 → 落槽 → 按单实例
	// 转交唤起或自任窗口。UI 不在线也能用(槽在引擎侧,窗口下次唤起必弹)
	bootstrap := ""
	for _, a := range flag.Args() {
		if strings.HasPrefix(strings.ToLower(a), "magnet:") ||
			strings.EqualFold(filepath.Ext(a), ".torrent") {
			bootstrap = a
			break
		}
	}

	if *pprofPort > 0 {
		startDiagnostics(*pprofPort)
	}
	runUI(uiOpts{
		exePath: exePath, dataDir: dataDir, cfg: cfg,
		settings: *uiSettings, bootstrap: bootstrap,
	})
}
