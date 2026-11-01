// ZDownloader 常驻组件(zdl_shell):托盘 + 悬浮窗,原生件跨平台(darwin=
// NSStatusItem/NSPanel,win=Win32),不初始化任何 UI 框架。三件套架构的
// "系统侧锚点":
//   - 保证引擎存活:zdl 不在则拉起,引擎退出则本组件随行收摊;
//   - 唤起/跟踪窗口进程 zdl_ui(驻留经 uiShow 中继秒开,已退则重拉);
//   - 托盘「退出」= /api/shutdown 引擎优雅退出 = 全部退出。
//
// zdl_ui 是可替换模块(Fyne today,Wails 等其它 UI 可整体换文件),本组件
// 只按同目录兄弟文件名 summon,不感知其技术栈。开机自启登记指向本 exe
// (win=注册表 Run 键,darwin=LaunchAgent plist,见 internal/autostart)。
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"zdownloader/internal/autostart"
	"zdownloader/internal/conf"
)

// defaultDataDir 数据目录默认值(与 cmd/zdl、cmd/zdl_ui 同款):裸跑跟随
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
		flagData  = flag.String("data", "", "数据目录(默认程序同级 data/;互拉时显式传递)")
		pprofPort = flag.Int("pprof", 0, "诊断端口(127.0.0.1 专用;0=关闭)")
	)
	flag.Parse()

	// darwin 要求最先锁主线程:主 goroutine 必须抢在 Go runtime 调度器把它
	// 挪离进程主线程之前固定住,才能专职跑 Cocoa 循环(见下方 darwin 分支)
	runtime.LockOSThread()

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

	// 引擎端口从共享配置读(引擎把实际绑定端口回写 config.json)
	cfg := conf.Load(filepath.Join(dataDir, "config.json"), "")

	// 自启登记自愈:zdl_shell 是开机自启的登记目标,登录拉起时顺带把 Run 键
	// 里陈旧的值(移动安装目录、旧版本误写的多余参数等)静默纠正为当前 exe
	// 的裸路径;未开启自启时 RefreshSelf 直接返回,无副作用。
	autostart.RefreshSelf()

	// darwin:Cocoa 主线程循环必须有人跑,托盘/悬浮窗的 dispatch_async 块才
	// 能落地(独立进程没有 wails 时代的事件循环)。主 goroutine 已锁主线程,
	// 专职 runCocoaLoop(阻塞至退出,永不返回);shell 逻辑移入后台 goroutine,
	// 退出统一走 exitAll/引擎随行 watcher 的 os.Exit。其余平台主 goroutine
	// 直跑 runShell 后自然返回。
	if runtime.GOOS == "darwin" {
		go func() {
			runShell(shellOpts{
				exePath: exePath, dataDir: dataDir, cfg: cfg,
				portFlag: cfg.Port, pprofFlag: *pprofPort,
			})
			os.Exit(0) // 兜底:正常退出路径在退出链/watcher 里直接 os.Exit
		}()
		runCocoaLoop()
		return
	}
	runShell(shellOpts{
		exePath: exePath, dataDir: dataDir, cfg: cfg,
		portFlag: cfg.Port, pprofFlag: *pprofPort,
	})
}
