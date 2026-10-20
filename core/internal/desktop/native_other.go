//go:build !windows && !darwin

// 非 Windows/macOS 平台(Linux 等)占位:无 SetWindowDisplayAffinity,主窗隐藏/
// 唤起回退到最小化/显示(最小化窗由窗口管理器任务栏承载,唤起可点任务栏);
// 悬浮窗仅空占位(darwin 真实现见 float_darwin.go)。注意 hideToTray 不能学
// macOS 用 WindowHide——本平台无托盘,orderOut 后窗口无处唤回。
package desktop

import (
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

func (a *App) hideToTray() {
	if a.wctx != nil {
		wruntime.WindowMinimise(a.wctx)
	}
}

func (a *App) showToScreen() {
	if a.wctx != nil {
		wruntime.WindowUnminimise(a.wctx)
		wruntime.WindowShow(a.wctx)
	}
}

// winPosOnScreen 非 Windows 无统一多屏信息:信任记忆位置,交由窗口管理器自行摆位。
func winPosOnScreen(x, y, w, h int) bool { return true }

// FloatWindow 置顶悬浮进度小窗占位:真实现为 Windows 的 Win32+GDI 自绘与
// macOS 的 NSPanel+WKWebView(float_darwin.go),本平台( Linux 等)暂无。
type FloatWindow struct {
	baseURL    string
	onShow     func()
	onSettings func()
	onExit     func()
}

func NewFloatWindow(baseURL string, onShow, onSettings, onExit func()) *FloatWindow {
	return &FloatWindow{baseURL: baseURL, onShow: onShow, onSettings: onSettings, onExit: onExit}
}

// Start 占位:空操作。
func (f *FloatWindow) Start() {}

// CloseFloatWindow Linux 等平台悬浮窗为占位实现,无需收起。
func CloseFloatWindow() {}

// quitAssist Linux 无需退出辅助。
func quitAssist() {}

// registerSystemShutdownObserver Linux 无 NSWorkspace 关机通知占位(进程退出
// 由 SIGTERM/SIGHUP 驱动,systemd 关机时经登录会话送达)。
func registerSystemShutdownObserver() {}

// setActivationPolicy Linux 无 Dock/激活策略概念,空实现(跨平台 OnStartup 引用)。
func setActivationPolicy(regular bool) {}
