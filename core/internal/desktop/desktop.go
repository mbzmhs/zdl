// Package desktop 提供基于 Wails 的跨平台桌面端外壳:
// 复用内嵌 Web UI 与既有 HTTP API,窗口内完成全部交互,不再打开系统浏览器。
// /api/* 请求被转发回同一个 HTTP 服务,其余路径由 Wails 资产服务(或本地
// 文件服务兜底)返回静态 UI。
package desktop

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

var (
	appMu sync.Mutex
	app   *App // 包级指针供 //export 回调(zdAppOnSystemShutdown)找回应用(与 floatWin 同款)
)

type App struct {
	assets      fs.FS
	api         http.Handler
	wctx        context.Context
	closeToQuit bool // 无托盘模式:关闭窗口即退出,否则隐藏到托盘
	quitting    bool // 真退出(托盘退出/SIGTERM):OnBeforeClose 据此放行窗口消失,而非最小化
	// systemTerminating 系统即将关机/重启(macOS NSWorkspace WillPowerOff/
	// WillRestart 通知)。此期间 OnBeforeClose 一律取消关闭(不放行、也不
	// 隐藏到托盘)——停事件循环统一交给退出 goroutine 的 quitAssist 完成。
	systemTerminating atomic.Bool
	startHidden       bool // 静默自启(开机 -no-browser):窗口创建即隐藏不弹出,仅托盘常驻
	onCancel    func()
	udfPath     string       // WebView2 用户数据目录(多实例隔离)
	openTarget  func(string) // macOS 关联启动回调(magnet:/.torrent 经 Apple Event 送达)

	// 主窗位置尺寸记忆:初始值来自配置(saveWinPos 由 main 注入 manager 落盘);
	// offscreen=已点 X 离屏到托盘(当前位置是屏外坐标,禁止采样);hasShown=至少上屏过一次。
	winPosSet              bool
	winX, winY, winW, winH int
	saveWinPos             func(x, y, w, h int)
	offscreen              bool
	hasShown               bool
}

// New 创建桌面端应用。assets 为内嵌 Web UI 的文件系统(根含 index.html),
// api 为既有 HTTP 服务处理器;onCancel 在"关闭即退出"模式下于关窗时触发。
func New(assets fs.FS, api http.Handler, closeToQuit bool, onCancel func(), udfPath string) *App {
	a := &App{assets: assets, api: api, closeToQuit: closeToQuit, onCancel: onCancel, udfPath: udfPath}
	appMu.Lock()
	app = a
	appMu.Unlock()
	return a
}

// OnSystemTerminating macOS 原生层回调:系统即将关机/重启。Wails v2 的
// applicationShouldTerminate 恒返回 NSTerminateCancel(只把 "Q" 转给 Go 侧),
// 系统只能等应用自己退出;托盘常驻模式若不主动退出,宽限期到即被强杀并弹
// "ZDownloader 中断了关机"。此处走与 SIGTERM 完全相同的退出路径(取消根
// 上下文 → 退出 goroutine 依次收起托盘/悬浮窗/主窗、后台保存任务状态、退出
// 事件循环),绝不落入"隐藏到托盘继续常驻"分支。
func (a *App) OnSystemTerminating() {
	if !a.systemTerminating.CompareAndSwap(false, true) {
		return // 通知可能重复送达(关机/重启各一次,或系统重发)
	}
	log.Println("收到系统关机/重启通知,主动走优雅退出(避免被强杀弹\"中断了关机\")")
	if a.onCancel != nil {
		a.onCancel()
	}
}

// StartHidden 让窗口创建即隐藏(Wails StartHidden 选项),永不弹出。开机静默自启
// (命令行带 -no-browser)调用:登录后仅常驻托盘,点击托盘再唤起。
func (a *App) StartHidden() { a.startHidden = true }

// SetWindowPosMemory 注入主窗位置尺寸记忆:初始值来自配置,save 为落盘回调
// (main 注入 manager.UpdateWindowPos,经 m.cfg 落盘避免与设置保存互相覆盖)。
func (a *App) SetWindowPosMemory(x, y, w, h int, set bool, save func(x, y, w, h int)) {
	a.winX, a.winY, a.winW, a.winH, a.winPosSet, a.saveWinPos = x, y, w, h, set, save
}

// pollWinPos 周期采样主窗位置尺寸,变化即落盘(拖动中最多 2 秒一存,足够记忆)。
// 最大化/最小化/离屏到托盘/StartHidden 未上屏时不采样,避免记住非正常态。
func (a *App) pollWinPos() {
	if a.saveWinPos == nil {
		return
	}
	for {
		select {
		case <-a.wctx.Done():
			return
		default:
		}
		time.Sleep(2 * time.Second)
		if a.offscreen || !a.hasShown {
			continue
		}
		if wruntime.WindowIsMaximised(a.wctx) || wruntime.WindowIsMinimised(a.wctx) {
			continue
		}
		x, y := wruntime.WindowGetPosition(a.wctx)
		w, h := wruntime.WindowGetSize(a.wctx)
		if w <= 0 || h <= 0 {
			continue
		}
		if x == a.winX && y == a.winY && w == a.winW && h == a.winH {
			continue
		}
		a.winX, a.winY, a.winW, a.winH = x, y, w, h
		a.saveWinPos(x, y, w, h)
	}
}

// ShowUI 唤起主窗口(供托盘点击与 /api/ui/show 调用,任意 goroutine 安全);
// settings 为 true 时唤起后直达设置页。
func (a *App) ShowUI(settings bool) {
	if a.wctx == nil {
		return
	}
	a.showToScreen() // WDA_NONE 撤销隐藏(窗体从未 SW_HIDE/最小化,WebView2 全程存活,唤起即响应)
	a.hasShown = true
	// 撤销隐藏只让窗体重新"上屏",不保证提到前台:窗体已可见却被别的应用遮挡时仍沉
	// 在下面,识别到下载/新任务唤窗时用户第一眼看不到。短暂置「总是置顶」把窗口拉到
	// Z 序最前,再取消置顶:窗口立即提到最顶层,又不永久压住其它应用(之后点别处它会
	// 正常退下)。
	wruntime.WindowSetAlwaysOnTop(a.wctx, true)
	time.Sleep(120 * time.Millisecond)
	wruntime.WindowSetAlwaysOnTop(a.wctx, false)
	// ExecuteScriptAsync 直接驱动渲染进程执行脚本:全量唤醒 __zdWake——既消费「待弹
	// 创建窗」持久槽(关联启动转发的磁链/种子),又刷新任务快照触发 maybePromptChoose
	// (choosing 文件选择窗)。连发数次覆盖窗口唤起时的时序抖动,确保弹窗一定落地。
	go func() {
		for i := 0; i < 8; i++ {
			// window.focus() 先让 webview 拿回窗口级输入焦点;__zdWake 全量唤醒
			// (取槽弹窗 + 刷新任务触发 choosing 弹层);旧前端无 __zdWake 时回退
			// __wakeNewTask 仅取槽。
			wruntime.WindowExecJS(a.wctx, `window.focus();if(window.__zdWake)window.__zdWake();else if(window.__wakeNewTask)window.__wakeNewTask()`)
			time.Sleep(250 * time.Millisecond)
		}
	}()
	if settings {
		wruntime.WindowExecJS(a.wctx, `if(window.__zdOpenSettings)window.__zdOpenSettings()`)
	}
}

// Quit 结束 Wails 事件循环。先置 quitting 标记,使 OnBeforeClose 放行窗口直接消失
// (而非托盘模式下的最小化取消关闭——那会吞掉 wruntime.Quit,导致进程退出不了)。
// 时序关键:quitAssist 必须在触发 wails Quit 之前调用——wails 的 Quit 是异步的
// (goroutine 里 [NSApp stop:]),它跑得快时事件循环先退出,quitAssist 的
// dispatch_sync 排在已死的队列上永远无人执行,整个退出协程挂死(bt.191 实测,
// 症状:进程不退出、端口占用)。quitAssist 在循环存活期同步执行则无竞态:
// 全部窗口立即 orderOut + stop + 哑事件唤醒循环即刻退出;之后的 WindowHide/
// Quit 派发到已退出的循环,无害空转(Windows 的 Quit/PostQuitMessage 立即生效)。
func (a *App) Quit() {
	if a.wctx == nil {
		return
	}
	a.quitting = true
	quitAssist()
	wruntime.WindowHide(a.wctx)
	wruntime.Quit(a.wctx)
}

// SaveTorrent 弹原生 Windows"另存为"对话框保存种子文件(data 为 .torrent 字节,
// suggestedName 为默认文件名),返回用户选择后实际落盘的路径。用户在对话框点取消
// (返回空路径)时以 "已取消" 返回,不写文件。wctx 未就绪(窗口未打开)时直接报错。
// 用原生对话框而非 webview 内置下载:用户能自选保存位置,且不走浏览器下载流。
func (a *App) SaveTorrent(data []byte, suggestedName string) (string, error) {
	if a.wctx == nil {
		return "", errors.New("窗口尚未就绪,请打开后重试")
	}
	out, err := wruntime.SaveFileDialog(a.wctx, wruntime.SaveDialogOptions{
		Title:           "另存为种子文件",
		DefaultFilename: suggestedName,
		Filters: []wruntime.FileFilter{{
			DisplayName: "Torrent 文件 (*.torrent)",
			Pattern:     "*.torrent",
		}},
	})
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", errors.New("已取消")
	}
	if werr := os.WriteFile(out, data, 0o644); werr != nil {
		return "", werr
	}
	return out, nil
}

// PickDir 弹原生"选择文件夹"对话框(Wails OpenDirectoryDialog,跨平台,Windows 底层
// 为 IFileDialog;内部 invokeSync 到主线程,可从 HTTP handler goroutine 直接调用)。
// 返回所选路径;用户取消(空路径)时 ok=false。wctx 未就绪(窗口未打开)时直接报错。
func (a *App) PickDir(title string) (string, bool, error) {
	if a.wctx == nil {
		return "", false, errors.New("窗口尚未就绪,请打开后重试")
	}
	out, err := wruntime.OpenDirectoryDialog(a.wctx, wruntime.OpenDialogOptions{Title: title})
	if err != nil {
		return "", false, err
	}
	if out == "" {
		return "", false, nil // 用户取消
	}
	return out, true, nil
}

// SetOpenTargetHandler 注入 macOS 关联启动回调(magnet: URL / 双击 .torrent 的
// 文件路径):wails 经 Apple Event 管道(kAEGetURL / application:openFile:)送达,
// main 侧用它走与命令行 argv 相同的 bootstrap 槽与创建窗流程。其他平台不触发。
func (a *App) SetOpenTargetHandler(fn func(target string)) {
	a.openTarget = fn
}

// Run 阻塞运行 Wails 事件循环,必须在主 goroutine 调用。
func (a *App) Run(title string, width, height int) error {
	registerSystemShutdownObserver() // 主线程、事件循环启动前注册 macOS 关机/重启观察
	return wails.Run(&options.App{
		Title:     title,
		Width:     width,
		Height:    height,
		MinWidth:  900,
		MinHeight: 600,
		// 窗口底漆与 WebView2 默认背景设为 UI 主题底色(--bg #0d1117):
		// 任何 显示/隐藏/最小化/恢复 过渡中被清空的窗口表面都是深色,
		// 不再出现白色一闪(开、关都闪)。
		BackgroundColour: &options.RGBA{R: 0x0d, G: 0x11, B: 0x17, A: 255},
		// 静默自启(-no-browser):窗口创建即隐藏、永不弹出(Wails 在 navigationCompleted
		// 后跳过 ShowWindow)。OnStartup 时主窗尚未上屏,枚举不到可见主窗,离屏方案在此
		// 不适用;WebView2 控制器保持可见,内容照常合成,唤起即响应。
		StartHidden: a.startHidden,
		// 关闭统一走 OnBeforeClose(不再用 SW_HIDE):托盘模式点 X = 移出屏幕 + 改工具窗
		// (任务栏不留图标);无托盘模式点 X = 退出。窗口保持可见 WebView2 不冻结,
		// 唤起即响应;仅关闭到托盘才临时转工具窗,普通最小化按钮照常可用。
		HideWindowOnClose: false,
		Windows: &windows.Options{
			// 用户数据目录放在数据目录下:多实例互不锁 WebView2
			WebviewUserDataPath: a.udfPath,
		},
		Mac: &mac.Options{
			// macOS .app 关联启动:magnet: URL(kAEGetURL)与双击 .torrent
			// (application:openFile:)由 wails 内置 Apple Event 管道送达此处
			OnFileOpen: func(p string) {
				if a.openTarget != nil {
					a.openTarget(p)
				}
			},
			OnUrlOpen: func(u string) {
				if a.openTarget != nil {
					a.openTarget(u)
				}
			},
		},
		AssetServer: &assetserver.Options{
			Assets: a.assets,
			Middleware: func(next http.Handler) http.Handler {
				return &apiRouter{api: a.api, assets: a.assets, next: next}
			},
		},
		OnStartup: func(ctx context.Context) {
			a.wctx = ctx
			// 恢复上次的窗口位置与尺寸(Wails Run 内部会先 WindowCenter,此处覆盖之);
			// 记忆位置落在已拔出的显示器上(与虚拟桌面几乎无交集)时放弃,回退居中默认
			if a.winPosSet && a.winW > 0 && a.winH > 0 && winPosOnScreen(a.winX, a.winY, a.winW, a.winH) {
				wruntime.WindowSetPosition(ctx, a.winX, a.winY)
				wruntime.WindowSetSize(ctx, a.winW, a.winH)
			}
			a.hasShown = !a.startHidden
			go a.pollWinPos()
		},
		OnBeforeClose: func(ctx context.Context) bool {
			if a.quitting {
				return false // 真退出(托盘退出/SIGTERM):窗口直接消失,不最小化——否则 Quit 被吞,进程退不了
			}
			if a.systemTerminating.Load() {
				// 系统关机/重启中:取消本次关窗。返回 false 会让 wails 直接
				// [NSApp stop:],可能抢在退出 goroutine 的 dispatch_sync(托盘/
				// 悬浮窗收起)之前退出事件循环使其挂死;统一由 goroutine 的
				// quitAssist(orderOut + stop + 哑事件)停循环,构造上无死锁。
				return true
			}
			if a.closeToQuit {
				if a.onCancel != nil {
					a.onCancel() // 关闭即退出:取消根上下文,走统一保存流程
				}
				return false // 不拦截:正常退出
			}
			// 托盘模式:点 X = 最小化离场并改工具窗(任务栏不留图标、Alt+Tab 不再出现)。
			// 不用 SW_HIDE——那会让 DWM 判定遮挡而挂起 WebView2,唤起时冻结、第一次按键无反应;
			// 最小化是真实前台切换不冻结,工具窗去任务栏图标,且仅隐藏时转工具窗,最小化按钮照常。
			a.hideToTray()
			return true // 取消关闭:窗口保持(已隐藏于任务栏外)
		},
	})
}

// apiRouter 把 /api/* 转发回既有 HTTP 服务,其余路径交给 Wails 资产服务(next)。
// next 缺失时用本地文件服务兜底,保证 UI 一定能加载。
type apiRouter struct {
	api    http.Handler
	assets fs.FS
	next   http.Handler // Wails 若注入资产处理器则优先使用
}

func (h *apiRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.api.ServeHTTP(w, r)
		return
	}
	if h.next != nil {
		h.next.ServeHTTP(w, r)
		return
	}
	http.FileServerFS(h.assets).ServeHTTP(w, r)
}
