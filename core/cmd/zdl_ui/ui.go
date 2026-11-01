//go:build !wails

// ZDownloader 桌面端窗口进程(zdl_ui,默认构建):Fyne 窗口 + 引擎 API 客户端。
// 与 ui_wails.go(-tags wails)互斥,二者实现同一入口 runUI,由构建参数选择
// 编译哪一套 UI(单进程只初始化一个 UI 栈,未选中的框架不驻留任何代码页)。
// 启动闭环:确保常驻组件在跑(不在则拉起 zdl_shell)→ 等引擎就绪 → 钉位首显。关窗默认隐藏驻留(SSE 仍连,托盘唤起经
// uiShow 帧秒开);设置页「关闭窗口时退出 UI 进程」打开后,关窗即整进程退出
// (内存归还 OS)——常驻组件与引擎不受影响,托盘随时重新拉起本进程。
package main

import (
	"log"
	"sync"
	"sync/atomic"

	"fyne.io/fyne/v2"

	"zdownloader/internal/nativeui"
)

func runUI(o uiOpts) {
	log.SetPrefix("[ui] ")

	client, ok := uiEnter(o)
	if !ok {
		return // 已唤起既有窗口实例,本进程静默退出
	}

	// 初始状态取引擎持久化配置:窗口位置/侧栏视图/主题跨会话恢复(位置钉位
	// 的屏外门控在保存侧已校验)。失败按零值兜底(NewStore 内归一化)。
	startCfg, err := client.Config()
	if err != nil {
		log.Printf("读取初始配置失败(按默认恢复): %v", err)
	}
	// ust 是旧 mgr.Config() 的分离版:位置/主题/视图的当前值副本,变更时同步
	// 本地副本 + 上报 /api/uiState;钉位/校位读本地副本(UI 进程没有 mgr)。
	ust := nativeui.UIState{
		WinX:      startCfg.WinX,
		WinY:      startCfg.WinY,
		WinW:      startCfg.WinW,
		WinH:      startCfg.WinH,
		WinPosSet: startCfg.WinPosSet,
		Theme:     startCfg.Theme,
		View:      startCfg.LastView,
	}
	var stMu sync.Mutex // onPreShow(主线程)与 savePos(收尾 goroutine)并发读改

	store := nativeui.NewStore(client, ust.Theme, ust.View)
	app := nativeui.NewApp(client, store)
	win := app.Window()

	// 主题切换:UI 上报引擎落盘;常驻组件经 SSE theme 帧跟随托盘菜单明暗
	app.SetOnThemePersist(func(t string) {
		stMu.Lock()
		ust.Theme = t
		stMu.Unlock()
		_ = client.UpdateUIState(nativeui.UIState{Theme: t})
	})
	// 侧栏视图切换 → 回写引擎 conf.LastView,新窗口进程恢复到上次所在页
	store.OnViewChange = func(v string) {
		stMu.Lock()
		ust.View = v
		stMu.Unlock()
		_ = client.UpdateUIState(nativeui.UIState{View: v})
	}
	// 引擎 uiShow 帧:托盘/单实例转交/扩展唤起已驻留的隐藏窗口
	store.OnShowRequest = func(settings bool) { app.ShowUI(settings) }

	// 窗口位置记忆 + 启动"钉位"钩子(WH_CBT 线程钩子在 CreateWindowEx 前改写
	// CREATESTRUCT 出生坐标,首显无闪帧)。位置持久化走 /api/uiState 写引擎。
	var winHWND atomic.Uintptr
	// 两条退出路径(关窗退出开关/引擎 uiExit)共用的窗口收尾:先还原窗口过程
	// 再关闭(销毁后 HWND 失效,无法再子类还原)
	exitUICloseWin := func() {
		fyne.Do(func() {
			if h := winHWND.Load(); h != 0 {
				minUnsubclass(h)
			}
			pinHookUninstall()
			win.Close()
		})
	}
	savePos := func() {
		if h := winHWND.Load(); h != 0 {
			if x, y, w, hh, ok := winGetRect(h); ok {
				stMu.Lock()
				ust.WinX, ust.WinY, ust.WinW, ust.WinH, ust.WinPosSet = x, y, w, hh, true
				stMu.Unlock()
				_ = client.UpdateUIState(nativeui.UIState{
					WinX: x, WinY: y, WinW: w, WinH: hh, WinPosSet: true,
				})
			}
		}
	}
	// 引擎退出前广播 uiExit:自行收尾退出(存位置/卸钩子),不留死接口孤儿窗口
	store.OnExitRequest = func() {
		go func() {
			savePos()
			exitUICloseWin()
		}()
	}

	// 显示前把位置+尺寸全部落定到"可见之前",消除首帧可见后的事后校正闪变:
	// ①首次建窗(h==0):Fyne create() 收尾会按内部宽高(此时恒为 0)强制
	//   SetSize 成内容最小尺寸——任何 Win32 手段的"出生尺寸"都会被它覆盖。
	//   正确注入点是 win.Resize:建窗前排进 runOnMainWhenCreated 队列,create()
	//   末尾 drainPendingEvents 执行后 w.width/shouldWidth 即保存值,收尾的
	//   SetSize 与首帧循环的 fitContent 复算都按保存值落定(它自带
	//   max(保存, 内容最小) 夹紧)。
	//   单位换算:保存值是外框像素(GetWindowRect),Resize 吃 Fyne 单位(客户
	//   区)——建窗前 glCanvas.scale 初值恒 1.0(create 内才重检 DPI),故直接
	//   扣外框差即可。
	// ②复显(h!=0):窗口存在且隐藏,直接 SetWindowPos 套保存矩形;隐藏态无
	//   渲染,Fyne 经 WM_SIZE 同步内部宽高,显示即终态(实测 Fyne 不会弹回)。
	// 位置仍由 CBT 钩子在出生前改写(见 winpin_windows.go);屏外记忆位放弃
	// 钉位回退 OS 默认,已可见的置前绝不动位置。
	app.SetOnPreShow(func() {
		h := nativeui.NativeHWND(win)
		if h != 0 {
			winHWND.Store(h)
		}
		if h != 0 && winIsVisible(h) {
			return
		}
		stMu.Lock()
		c := ust
		stMu.Unlock()
		if !c.WinPosSet || c.WinW <= 0 || c.WinH <= 0 || !winPosOnScreen(c.WinX, c.WinY, c.WinW, c.WinH) {
			return
		}
		if h == 0 {
			fd, fe := winFrameEstimate()
			win.Resize(fyne.NewSize(float32(c.WinW-fd), float32(c.WinH-fe)))
		} else {
			winSetRect(h, c.WinX, c.WinY, c.WinW, c.WinH)
		}
		pinBegin(int32(c.WinX), int32(c.WinY), int32(c.WinW), int32(c.WinH))
	})

	// 显示后:关钉位 + 补 Fyne 缺的"提到最前"(RequestFocus 只给输入焦点、
	// 不拉前台)+ 安装最小化占位消息隔离(副屏 DPI 钳缩修复,见 winmin_windows.go)
	app.SetOnShown(func() {
		pinEnd()
		if h := nativeui.NativeHWND(win); h != 0 {
			winHWND.Store(h)
			minSubclass(h) // C 侧幂等
			winSetForeground(h)
		}
	})

	// 钩子就绪后首显(钉位生效);常驻组件以 -settings 拉起时直达设置页
	go func() {
		fyne.Do(func() { pinHookInstall() })
		fyne.Do(func() { app.ShowUI(o.settings) })
	}()

	// 关窗语义:默认隐藏驻留(常驻组件持续跟踪,托盘唤起秒开);设置开关
	// 「关闭窗口时退出 UI 进程」打开 → 存位置后整进程退出。开关读 store 配置
	// 快照(设置保存后 SetCfg 刷新),引擎侧经 /api/uiState 同步落盘。
	win.SetCloseIntercept(func() {
		exitOnClose := false
		if c := store.Cfg(); c != nil {
			exitOnClose = c.UIExitOnClose
		}
		savePos()
		if exitOnClose {
			exitUICloseWin()
			return
		}
		fyne.Do(func() { win.Hide() })
	})

	// 进入 Fyne 事件循环(阻塞)。引擎 uiExit / 关窗退出路径都汇到 win.Close。
	app.RunLoop()
	log.Println("窗口进程已退出")
}
