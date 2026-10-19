// ZDownloader 核心:多线程下载管理器,单二进制、零第三方依赖。
// Windows 下以 GUI 子系统构建(无命令行窗口),托盘常驻,日志写入数据目录。
// 默认启动桌面版(Wails 窗口 + 托盘 + 悬浮窗 + 开机自启 + 文件关联);
// 带 -headless 参数则为纯 HTTP 服务 + 下载引擎(无窗口/托盘/悬浮窗/自启/文件关联),
// 供 TV(Android)与服务器环境嵌入。BT 协议栈纯 Go 零 cgo,可 CGO_ENABLED=0 交叉编译:
//
//	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "-s -w" -o zdownloader .
//
// 启动参数两形态一致:-port(覆盖配置)/-dir(下载目录)/-data(数据目录)/-headless(无头模式)。
// headless 实例生命周期由宿主负责:宿主先 GET /api/ping 探测,在运行则复用,不重复拉起。
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"zdownloader/internal/autostart"
	"zdownloader/internal/conf"
	"zdownloader/internal/desktop"
	"zdownloader/internal/engine"
	"zdownloader/internal/hub"
	"zdownloader/internal/server"
	"zdownloader/internal/tray"
)

//go:embed web
var webContent embed.FS

//go:embed assets/tray.png
var trayIconPNG []byte

func main() {
	var (
		flagPort = flag.Int("port", 0, "监听端口(覆盖配置文件)")
		flagDir  = flag.String("dir", "", "下载目录(覆盖配置文件)")
		flagData = flag.String("data", "", "数据目录(默认: macOS .app 用用户配置目录,其余程序同级 data/)")
		noTray   = flag.Bool("no-tray", false, "不显示系统托盘图标(关闭窗口即退出)")
		// 桌面版从不打开浏览器;开机自启写入的命令行带 -no-browser,以此区分静默
		// 自启(登录后台常驻托盘、窗口创建即隐藏不弹出)与手动启动(直接显示窗口)
		noBrowser = flag.Bool("no-browser", false, "开机自启静默启动:窗口创建即隐藏不弹出,仅托盘常驻")
		// 无头模式:仅纯 HTTP 服务 + 下载引擎,无 Wails 窗口/托盘/悬浮窗/开机自启/
		// 文件关联,供 TV(Android)与服务器嵌入。与桌面版同一二进制,仅启动参数区分。
		headless = flag.Bool("headless", false, "无头模式:纯 HTTP 服务 + 下载引擎,供 TV/服务器嵌入")
	)
	flag.Parse()

	exePath, err := os.Executable()
	if err != nil {
		log.Fatal("无法定位程序路径: ", err)
	}
	exeDir := filepath.Dir(exePath)

	// 数据目录:.app 形态(macOS)用用户配置目录——写进 bundle 会污染发布物
	// 并导致 codesign 失败(见 datadir_darwin.go);其余优先程序同级 data/,
	// 不可写则回退用户配置目录
	dataDir := *flagData
	if dataDir == "" {
		if d, ok := appBundleDataDir(exePath); ok {
			dataDir = d
		} else {
			dataDir = filepath.Join(exeDir, "data")
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				base, e := os.UserConfigDir()
				if e != nil {
					log.Fatalf("数据目录不可用: %v", e)
				}
				dataDir = filepath.Join(base, "ZDownloader")
				if err := os.MkdirAll(dataDir, 0o755); err != nil {
					log.Fatalf("数据目录不可用: %v", err)
				}
			}
		}
	}
	setupLogging(dataDir)
	cfgPath := filepath.Join(dataDir, "config.json")
	tasksDir := filepath.Join(dataDir, "tasks")

	cfg := conf.Load(cfgPath, defaultDownloadDir())
	if *flagPort > 0 {
		cfg.Port = *flagPort
	}
	if *flagDir != "" {
		cfg.DownloadDir = *flagDir
	}
	if *headless {
		// TV/服务端嵌入:下载完成即止,不做种(设备内存/带宽有限,无需维持分享)
		cfg.BT.AutoSeed = false
	}
	_ = os.MkdirAll(cfg.DownloadDir, 0o755)

	// 关联启动 + 单实例转交(仅桌面):命令行带 magnet: 链接或 .torrent 路径
	// (文件关联/协议处理传入的位置参数)时,先尝试转交给运行中的实例(配置
	// 端口),转交成功本进程即退出;否则正常启动,服务就绪后自动创建该任务
	// (挂起为待创建,UI 弹出创建窗口供选保存目录与文件)。headless 由宿主嵌入,
	// 生命周期宿主负责(宿主 GET /api/ping 探测,在运行则复用),无需转交/单实例。
	var boot string
	if !*headless {
		// 关联启动(带 magnet:/.torrent):转交运行中实例(弹创建窗预填磁链/种子),
		// 成功本进程即退出;失败则本进程成为宿主,服务就绪后写持久槽弹创建窗。
		boot = pickBootstrap(flag.Args())
		if boot != "" {
			if forwardToRunning(cfg, boot) {
				return
			}
		} else if activateRunningInstance(cfg) {
			// 纯启动(双击图标/快捷方式,无文件参数):已有实例在运行→唤其窗口到最顶层并
			// 取焦,本进程退出,不再开第二个实例(旧行为会顺延端口另起一个)。
			return
		}
	}

	h := hub.New()
	mgr := engine.NewManager(cfg, cfgPath, tasksDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mgr.Run(ctx)

	webSub, err := fs.Sub(webContent, "web")
	if err != nil {
		log.Fatal("内嵌资源异常: ", err)
	}
	svc := server.New(mgr, h, webSub)
	srv := &http.Server{
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go svc.StartAggregator(ctx.Done())

	// 端口被占用则向后顺延
	var ln net.Listener
	port := cfg.Port
	for i := 0; i < 10; i++ {
		l, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if e == nil {
			ln = l
			break
		}
		port++
	}
	if ln == nil {
		log.Fatalf("端口 %d 起连续 10 个均被占用,无法启动", cfg.Port)
	}
	cfg.Port = port
	_ = cfg.Save(cfgPath)

	go func() {
		if e := srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常: %v", e)
		}
	}()

	ui := fmt.Sprintf("http://127.0.0.1:%d/", port)

	// ===== headless:服务起来后仅等信号做简单收尾,直接返回 =====
	if *headless {
		// headless 无系统通知渠道:回调落日志,避免 RSS 路径空指针
		mgr.OnNotify = func(title, text string) { log.Printf("[notify] %s %s", title, text) }
		log.Printf("ZDownloader v%s headless 已启动  本地接口: %s", server.Version, ui)
		log.Printf("数据目录: %s", dataDir)
		log.Printf("下载目录: %s", cfg.DownloadDir)

		// SIGTERM/中断 → 掐 SSE,停服务存任务,退出
		<-ctx.Done()
		log.Println("正在退出,停止服务并保存任务状态...")
		svc.CloseSSE()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		mgr.Shutdown(shutdownCtx)
		log.Println("已退出")
		return
	}

	// ===== 以下为桌面版 =====
	log.Printf("ZDownloader v%s 桌面版已启动  本地接口: %s", server.Version, ui)
	log.Printf("数据目录: %s", dataDir)
	log.Printf("下载目录: %s", cfg.DownloadDir)

	// 已开启自启但注册表路径与当前程序不一致时(移动目录/升级覆盖),静默修正
	autostart.RefreshSelf()

	// 桌面端:UI 承载于 Wails 原生窗口,不打开系统浏览器。
	desk := desktop.New(webSub, svc.Handler(), *noTray, stop, filepath.Join(dataDir, "webview2"))
	if *noBrowser {
		desk.StartHidden() // 静默自启:窗口创建即隐藏,不弹出
	}
	// 主窗位置尺寸记忆:初始值来自配置,变化经 manager 落盘(与设置保存同源,互不覆盖)
	desk.SetWindowPosMemory(cfg.WinX, cfg.WinY, cfg.WinW, cfg.WinH, cfg.WinPosSet, mgr.UpdateWindowPos)
	svc.ShowUI = desk.ShowUI           // 供扩展「打开管理界面」唤起桌面窗口
	svc.SaveTorrent = desk.SaveTorrent // 「另存为种子」走原生保存对话框,不落 webview 下载流
	svc.PickDir = desk.PickDir         // 「浏览…」文件夹选择走 Wails 原生对话框(跨平台)

	// macOS .app 关联启动(magnet: URL / 双击 .torrent 经 Apple Event 送达):
	// 复用命令行 argv 的 pickBootstrap 过滤与 bootstrap 槽;启动早期送达时
	// ShowUI 空操作(槽持久,前端载入后取走),运行中送达则唤窗弹创建窗
	desk.SetOpenTargetHandler(func(target string) {
		b := pickBootstrap([]string{target})
		if b == "" {
			return
		}
		log.Printf("关联启动(macOS 事件): %s", b)
		svc.QueueBootstrapTarget(b)
		svc.ShowUI(false)
	})

	// 关联启动且本进程即宿主:记入持久槽(窗口尚未就绪时唤窗为空操作,前端
	// 载入后 onopen/获焦取走并弹预填创建窗)。磁链 → 预填磁力链接;.torrent → 预填文件。
	if boot != "" {
		svc.QueueBootstrapTarget(boot)
		log.Printf("已从启动参数唤起创建窗口: %s", boot)
	}

	// RSS 自动下载(完成后提醒)走 Windows 系统通知(托盘气泡,操作中心可见)
	mgr.OnNotify = tray.Notify

	// 种子自动打开冲突(同 InfoHash 已有任务):不静默跳过,交由前端弹确认框,
	// 让用户自行决定是否仍要创建(引擎无法替用户决定)
	mgr.OnSeedConflict = func(name, torrentPath, dir string) {
		svc.QueueSeedConflict(name, torrentPath, dir)
	}

	// 任务进入 choosing(磁力元数据异步就绪需选文件/单文件关联启动需先选目录):
	// 唤窗置前取焦并强制唤醒自托盘隐藏的 WebView2,让前端 maybePromptChoose 弹出
	// 文件选择/创建窗。窗口隐藏时 WebView2 挂起,前端自身无法弹层,须服务端主动 poke。
	mgr.OnChoosePrompt = func(id string) {
		log.Printf("任务 %s 进入选择状态,唤起窗口弹出选择窗", id)
		svc.ShowUI(false)
	}

	// 置顶悬浮进度窗(设置 showFloatWindow 控制,默认开启),右键菜单同托盘
	desktop.NewFloatWindow(ui,
		func() { desk.ShowUI(false) },
		func() { desk.ShowUI(true) },
		stop).Start()

	// 系统托盘:左键唤起桌面窗口,右键菜单(打开界面/设置/退出)
	if !*noTray {
		go func() {
			if err := tray.Run(trayIconPNG,
				"ZDownloader v"+server.Version+" 桌面版 — 点击唤起窗口",
				func() { desk.ShowUI(false) },
				func() { desk.ShowUI(true) },
				stop); err != nil {
				log.Printf("系统托盘不可用: %v", err)
			}
		}()
	}

	// 收尾:托盘退出/SIGTERM → 先让窗口与托盘图标一起消失,再后台停服务存任务
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		log.Println("正在退出,先收起窗口与托盘,后台保存任务状态...")
		tray.Close() // 托盘图标先消失(主循环仍在,同步移除)
		log.Println("退出:托盘已收起")
		desktop.CloseFloatWindow() // 悬浮窗同步消失,不留残影
		log.Println("退出:悬浮窗已收起")
		desk.Quit() // 主窗消失(quitting 放行,不最小化)
		log.Println("退出:已通知主窗隐藏并退出事件循环")
		svc.CloseSSE() // 掐断 SSE 长连接:否则 http.Server.Shutdown 干等满超时
		log.Println("退出:SSE 已关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		mgr.Shutdown(shutdownCtx)
		close(shutdownDone)
	}()

	// 兜底(防"中断了关机"弹窗):系统关机/重启时若 8 秒内未自然退出
	// (某处意外阻塞/挂死),系统在宽限期末强杀本进程并弹"ZDownloader 中断了
	// 关机"。8 秒短于系统强杀宽限:后台保存状态预算 <5 秒,正常退出(托盘退
	// 出/关机通知/SIGTERM)都远在此之前完成,此路只收拾真挂死的进程。
	go func() {
		<-ctx.Done()
		time.Sleep(8 * time.Second)
		log.Println("退出流程 8 秒未完成,强制终止进程")
		os.Exit(0)
	}()

	// 异常退出后 WebView2 用户数据目录可能被残留子进程短暂锁定,自动重试
	var runErr error
	for i := 0; i < 6; i++ {
		runErr = desk.Run("ZDownloader v"+server.Version, 1180, 760)
		if runErr == nil {
			break
		}
		log.Printf("桌面窗口启动失败(%v),1.5 秒后重试", runErr)
		time.Sleep(1500 * time.Millisecond)
	}
	if runErr != nil {
		log.Fatalf("桌面窗口异常: %v", runErr)
	}
	<-shutdownDone
	log.Println("已退出")
}

// pickBootstrap 从命令行位置参数提取关联启动目标:首个 magnet: 链接,
// 或首个在盘上的 .torrent 文件(转绝对路径)。没有则返回空串。
// flag 包遇位置参数即停止解析,flag.Args() 即这些参数。
func pickBootstrap(args []string) string {
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(a), "magnet:") {
			return a
		}
		if strings.EqualFold(filepath.Ext(a), ".torrent") {
			abs, err := filepath.Abs(a)
			if err != nil {
				continue
			}
			if _, e := os.Stat(abs); e == nil {
				return abs
			}
		}
	}
	return ""
}

// forwardToRunning 探测配置端口上运行中的实例并转交启动目标:
// /api/ping 应答含 ZDownloader 视为在运行,请运行中实例弹「创建对话框」并
// 预填磁链/种子(不自动建任务)后,本进程退出,避免关联启动拉起第二个实例。
// 旧版实例无该语义(返回 400)时回退直接建任务(旧行为),保证磁链不丢。
// 仅在确认有实例在运行时返回 true(转交失败也返回 true:宁可少建一个也不开双实例)。
func forwardToRunning(cfg conf.Config, target string) bool {
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	cl := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := cl.Get(base + "/api/ping")
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ZDownloader") {
		return false
	}
	// 首选:请运行中实例弹创建对话框并预填(关联启动 → 不自动建任务)。
	payload, _ := json.Marshal(newTaskForwardPayload(target))
	pr, err := cl.Post(base+"/api/ui/newtask", "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("转交启动目标失败(实例在运行): %v", err)
		return true
	}
	st := pr.StatusCode
	_ = pr.Body.Close()
	if st == http.StatusOK {
		log.Printf("已转交运行中的实例(%s,弹创建窗): %s", base, target)
		return true
	}
	// 回退旧版实例:直接建任务(旧行为),磁链/种子仍交给 ZDownloader。
	req := engine.CreateReq{Dir: cfg.DownloadDir, Choose: true, ChooseAny: true}
	if strings.HasPrefix(strings.ToLower(target), "magnet:") {
		req.Magnet = target
	} else {
		req.TorrentPath = target
	}
	b, _ := json.Marshal(req)
	fb, err := cl.Post(base+"/api/tasks/bt", "application/json", bytes.NewReader(b))
	if err != nil {
		log.Printf("转交启动目标(回退)失败: %v", err)
	} else {
		_ = fb.Body.Close()
		log.Printf("运行中实例为旧版(HTTP %d),已回退直接建任务(%s): %s", st, base, target)
	}
	return true
}

// activateRunningInstance 纯启动时探测运行中实例并唤窗:GET 配置端口的 /api/ping,
// 应答含 ZDownloader 视为已有实例在运行,再 GET /api/ui/show 让已有实例把窗口拉到
// 最顶层并唤醒 WebView2,本进程随即退出(避免旧行为顺延端口另起第二个实例)。
// 仅当确认有实例在运行时返回 true(唤窗接口不可用也返回 true:宁可少开一个)。
func activateRunningInstance(cfg conf.Config) bool {
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	cl := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := cl.Get(base + "/api/ping")
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ZDownloader") {
		return false
	}
	sr, err := cl.Get(base + "/api/ui/show")
	if err == nil {
		_ = sr.Body.Close()
		log.Printf("已有实例在运行(%s),已唤起其窗口,本进程退出", base)
		return true
	}
	log.Printf("已有实例在运行(%s)但唤窗接口不可用,本进程仍退出", base)
	return true
}

// newTaskForwardPayload 构造 /api/ui/newtask 的转交负载:磁链 → magnet;
// .torrent → torrentPath。保存目录不传,由运行中实例按其自身默认目录预填。
func newTaskForwardPayload(target string) map[string]string {
	m := map[string]string{}
	if strings.HasPrefix(strings.ToLower(target), "magnet:") {
		m["magnet"] = target
	} else {
		m["torrentPath"] = target
	}
	return m
}
