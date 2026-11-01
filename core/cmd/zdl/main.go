// ZDownloader 无头引擎(zdl):纯数据面——下载/BT/HLS/RSS 引擎 + 回环 REST/SSE
// API + 任务持久化。设计约束(三件套架构,见 cmd/zdlui):
//   - 零系统特殊性:无托盘、无窗口、无文件关联、无原生对话框、不 spawn 任何
//     进程;OS 相关职责全部归 zdlui(托盘/悬浮窗常驻组件 + Fyne 窗口)。
//   - 纯 Go 可构建:不依赖 cgo/Fyne,交叉编译无需 MinGW。
//   - 生命周期由常驻组件驱动:引擎常驻直到 /api/shutdown(托盘「退出」)或
//     SIGTERM;UI 进程生死与引擎无关(关窗驻留/退进程均不影响下载)。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"zdownloader/internal/conf"
	"zdownloader/internal/engine"
	"zdownloader/internal/hub"
	"zdownloader/internal/logbuf"
	"zdownloader/internal/server"
)

func main() {
	var (
		flagPort  = flag.Int("port", 0, "监听端口(覆盖配置文件)")
		flagDir   = flag.String("dir", "", "下载目录(覆盖配置文件)")
		flagData  = flag.String("data", "", "数据目录(默认程序同级 data/)")
		pprofPort = flag.Int("pprof", 0, "诊断端口(127.0.0.1 专用;0=关闭)")
	)
	flag.Parse()

	// 提示运行时尽快把已释放内存还给 OS(Windows 默认保留较多虚拟内存)
	os.Setenv("GODEBUG", "madvdontneed=1")

	exePath, err := os.Executable()
	if err != nil {
		log.Fatal("无法定位程序路径: ", err)
	}
	exeDir := filepath.Dir(exePath)
	dataDir := *flagData
	if dataDir == "" {
		dataDir = defaultDataDir(exeDir)
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
	setupLogging(dataDir)
	if *pprofPort > 0 {
		startDiagnostics(*pprofPort)
	}
	cfgPath := filepath.Join(dataDir, "config.json")
	tasksDir := filepath.Join(dataDir, "tasks")

	cfg := conf.Load(cfgPath, defaultDownloadDir())
	if *flagPort > 0 {
		cfg.Port = *flagPort
	}
	if *flagDir != "" {
		cfg.DownloadDir = *flagDir
	}
	_ = os.MkdirAll(cfg.DownloadDir, 0o755)

	log.SetPrefix("[引擎] ")

	h := hub.New()
	mgr := engine.NewManager(cfg, cfgPath, tasksDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mgr.Run(ctx)

	// 内置管理界面:同目录 web/(vite 产物)经浏览器直接访问,与桌面端共用
	// 同一套回环 API/SSE。目录缺失时静态区整体 404,自动退化为纯 API 模式
	// (go:embed 无法跨目录引用,且文件夹分发让 UI 产物可独立更新)。
	// .app bundle(mac)分发时 web/ 在 Contents/Resources/web——codesign 要求
	// Contents/MacOS/ 下只容 Mach-O,网页资源须随 Resources 走(构建脚本同规则)
	webDir := filepath.Join(exeDir, "web")
	if _, e := os.Stat(filepath.Join(webDir, "index.html")); e != nil {
		if alt, e2 := filepath.Abs(filepath.Join(exeDir, "..", "Resources", "web")); e2 == nil {
			if _, e3 := os.Stat(filepath.Join(alt, "index.html")); e3 == nil {
				webDir = alt
			}
		}
	}
	svc := server.New(mgr, h, os.DirFS(webDir))
	svc.OnExit = stop // /api/shutdown 走完存盘后结束进程(常驻组件托盘「退出」)
	srv := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go svc.StartAggregator(ctx.Done())

	var ln net.Listener
	port := cfg.Port
	for i := 0; i < 10; i++ {
		l, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if e == nil {
			ln = l
			break
		}
		// 端口被占:若正是另一个 zdl 在跑,静默退出(单实例);否则顺延重试
		if i == 0 && engineAlreadyRunning(cfg.Port) {
			log.Printf("引擎已在运行(127.0.0.1:%d),本进程退出", cfg.Port)
			return
		}
		port++
	}
	if ln == nil {
		log.Fatalf("端口 %d 起连续 10 个均被占用,无法启动", cfg.Port)
	}
	cfg.Port = port
	_ = cfg.Save(cfgPath) // 实际端口回写:zdlui/扩展以 config.json 为准
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	log.Printf("ZDownloader v%s 引擎已启动  本地接口: %s", server.Version, base)
	log.Printf("数据目录: %s", dataDir)
	log.Printf("下载目录: %s", cfg.DownloadDir)

	go func() {
		if e := srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常: %v", e)
		}
	}()

	<-ctx.Done()
	log.Println("引擎正在退出,保存任务状态...")
	svc.CloseSSE()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	mgr.Shutdown(shutdownCtx)
	log.Println("引擎已退出")
}

// engineAlreadyRunning 探测端口上是否已是本引擎(zdl 无窗,无需唤窗,只判定)
func engineAlreadyRunning(port int) bool {
	cl := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := cl.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK && strings.Contains(string(body), "ZDownloader")
}

/* ---------------- 启动辅助(与 zdlui 各持副本;v3 转正后收敛 internal/boot) ---------------- */

func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

// defaultDataDir 数据目录默认值:裸跑跟随 exe 所在目录(文件夹分发形态,
// 绿色便携);.app bundle 内运行(mac)改用用户配置目录——往已签名 bundle 的
// Contents/MacOS 写运行时数据既脏又在升级时丢失,且与 1.8 mac 用户数据
// (~/Library/Application Support/ZDownloader)延续。
func defaultDataDir(exeDir string) string {
	if strings.Contains(exeDir, ".app/Contents/MacOS") {
		if base, e := os.UserConfigDir(); e == nil {
			return filepath.Join(base, "ZDownloader")
		}
	}
	return filepath.Join(exeDir, "data")
}

func setupLogging(dataDir string) {
	logPath := filepath.Join(dataDir, "zdownloader.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	if st, e := f.Stat(); e == nil && st.Size() > 8<<20 {
		f.Close()
		_ = os.Rename(logPath, logPath+".old")
		f, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
	}
	log.SetOutput(io.MultiWriter(f, logbuf.Default()))
	log.SetFlags(log.LstdFlags)
}

func startDiagnostics(port int) {
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", pprof.Handler("index"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("诊断端点: http://127.0.0.1:%d/debug/pprof/", port)
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("诊断端点退出: %v", err)
		}
	}()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		var ms runtime.MemStats
		for range t.C {
			runtime.ReadMemStats(&ms)
			log.Printf("[mem] HeapAlloc=%dMB HeapSys=%dMB Sys=%dMB NumGC=%d Goroutines=%d",
				ms.HeapAlloc>>20, ms.HeapSys>>20, ms.Sys>>20, ms.NumGC, runtime.NumGoroutine())
		}
	}()
}
