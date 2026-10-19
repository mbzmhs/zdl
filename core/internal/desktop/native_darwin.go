//go:build darwin

// Wails v2.16.0 的 darwin 前端在 WailsContext.h 按 __has_include 条件引入
// UniformTypeIdentifiers(创建/另存对话框按扩展名过滤用 UTType),但其所有
// #cgo LDFLAGS 均未声明该 framework——macOS 11+ SDK 下编译期可见、链接期报
// undefined: _OBJC_CLASS_$_UTType。本文件唯一职责是借 cgo 链接参数跨包合并,
// 把该 framework 拖进最终链接命令,无任何可调用的代码。
package desktop

// #cgo CFLAGS: -x objective-c
// #cgo darwin LDFLAGS: -framework UniformTypeIdentifiers
// #include "float_darwin.h"
import "C"

import (
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// quitAssist darwin 退出辅助:同步 orderOut 全部窗口 + stop + 哑事件唤醒循环,
// 让主窗立即消失、事件循环立即退出(见 float_darwin.m zdAppQuitAssistC)。
func quitAssist() {
	C.zdAppQuitAssistC()
}

// registerSystemShutdownObserver 观察系统关机/重启通知(见 float_darwin.m
// zdAppRegisterShutdownObserverC):系统即将关机/重启时回调 Go 侧主动走优雅
// 退出——Wails v2 的 applicationShouldTerminate 恒返回 NSTerminateCancel,系统
// 只能等应用自己退出,不主动退出即被强杀并弹"中断了关机"。须在 Run 前于
// 主线程调用(事件循环启动前注册,通知送达时循环已存活)。
func registerSystemShutdownObserver() {
	C.zdAppRegisterShutdownObserverC()
}

//export zdAppOnSystemShutdown
func zdAppOnSystemShutdown() {
	appMu.Lock()
	a := app
	appMu.Unlock()
	if a != nil {
		a.OnSystemTerminating()
	}
}

// macOS 托盘隐藏 = WindowHide(orderOut):NSStatusItem 托盘(bt.184)承担唤起
// 职责;此前 WindowMinimise 缩 Dock 是无托盘期的过渡态。唤起侧 Unminimise+Show
// 对 orderOut 窗同样有效(StartHidden 实测)。
func (a *App) hideToTray() {
	if a.wctx != nil {
		wruntime.WindowHide(a.wctx)
	}
}

func (a *App) showToScreen() {
	if a.wctx != nil {
		wruntime.WindowUnminimise(a.wctx)
		wruntime.WindowShow(a.wctx)
	}
}

// winPosOnScreen darwin 暂信任记忆位置(与 Linux 同):拔屏后恢复到屏外坐标时,
// macOS 窗口管理会把窗口拉回可见区。精确校验可后续用 NSScreen(screens 的
// frame 带原点)实现——比 Windows 的 GetSystemMetrics 方案信息更全。
func winPosOnScreen(x, y, w, h int) bool { return true }
