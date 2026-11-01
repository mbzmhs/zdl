//go:build windows

// 窗口位置记忆的 Win32 实现:fyne v2.8 的 Window 接口未暴露 Move/Position,
// 但 driver.NativeWindow.RunNative 给出 HWND——位置读写直接走 GetWindowRect/
// SetWindowPos,线程安全且不依赖 Fyne 版本演进。
package main

import (
	"syscall"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")

type rect struct {
	Left, Top, Right, Bottom int32
}

// winGetRect 读窗口屏幕坐标与尺寸
func winGetRect(hwnd uintptr) (x, y, w, h int, ok bool) {
	if hwnd == 0 {
		return 0, 0, 0, 0, false
	}
	var r rect
	ret, _, _ := user32.NewProc("GetWindowRect").Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		return 0, 0, 0, 0, false
	}
	return int(r.Left), int(r.Top), int(r.Right - r.Left), int(r.Bottom - r.Top), true
}

// winSetRect 写窗口位置尺寸(SWP_NOZORDER|NOACTIVATE:静默应用,不抢焦点)
func winSetRect(hwnd uintptr, x, y, w, h int) {
	if hwnd == 0 || w <= 0 || h <= 0 {
		return
	}
	const swpNoZorder = 0x4
	const swpNoActivate = 0x10
	user32.NewProc("SetWindowPos").Call(
		hwnd, 0,
		uintptr(int32(x)), uintptr(int32(y)),
		uintptr(int32(w)), uintptr(int32(h)),
		swpNoZorder|swpNoActivate)
}

// winPosOnScreen 报告窗口矩形与虚拟桌面(所有显示器并集)是否有足够可见部分
// (可见宽≥100、高≥60,足够抓住标题栏拖回),防止记忆位置落在已拔出的显示器上
// (多屏布局变更,如上排屏被移除)时把窗口钉位/恢复到屏外。语义照搬旧 Wails 壳
// internal/desktop 的同名函数(多屏记忆位场景已验证多年);不满足时调用方应放弃
// 记忆位置,回退 OS 默认出生位(居中)。
func winPosOnScreen(x, y, w, h int) bool {
	const (
		smXVirtual  = 76 // SM_XVIRTUALSCREEN:虚拟桌面原点 X(多屏可为负)
		smYVirtual  = 77 // SM_YVIRTUALSCREEN:虚拟桌面原点 Y(多屏可为负)
		smCXVirtual = 78 // SM_CXVIRTUALSCREEN
		smCYVirtual = 79 // SM_CYVIRTUALSCREEN
	)
	gsm := user32.NewProc("GetSystemMetrics")
	r, _, _ := gsm.Call(smXVirtual)
	vx := int(int32(r)) // 返回值按 int32 解释:原点可为负,直接转 int 会丢符号
	r, _, _ = gsm.Call(smYVirtual)
	vy := int(int32(r))
	r, _, _ = gsm.Call(smCXVirtual)
	vw := int(int32(r))
	r, _, _ = gsm.Call(smCYVirtual)
	vh := int(int32(r))
	ix := max(min(x+w, vx+vw)-max(x, vx), 0)
	iy := max(min(y+h, vy+vh)-max(y, vy), 0)
	return ix >= 100 && iy >= 60
}

// winIsVisible 报告窗口当前是否可见(IsWindowVisible)。用于区分"隐藏唤起"
// (需钉位/校位)与"已可见时的再置前"(绝不能动位置——移动窗口不实时写配置,
// 若置前也钉位,窗口会被拽回上次隐藏/退出时的保存位)。
func winIsVisible(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := user32.NewProc("IsWindowVisible").Call(hwnd)
	return r != 0
}

// winFrameDelta 现存窗口的"外框-客户区"尺寸差(GetWindowRect − GetClientRect)。
// Fyne 的 SetSize 只定客户区,而记忆/钉位尺寸是外框尺寸,换算需要此差值;
// 仅对已存在的窗口可用(故复显路径用实测,首次出生路径用 winFrameEstimate)。
func winFrameDelta(hwnd uintptr) (dx, dy int, ok bool) {
	if hwnd == 0 {
		return 0, 0, false
	}
	var rc rect
	ret, _, _ := user32.NewProc("GetClientRect").Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	if ret == 0 {
		return 0, 0, false
	}
	_, _, w, h, ok2 := winGetRect(hwnd) // 只取尺寸,位置弃用
	if !ok2 {
		return 0, 0, false
	}
	return w - int(rc.Right), h - int(rc.Bottom), true
}

// winFrameEstimate 尚未建窗时按系统度量估算外框尺寸差。宁可略大——偏小会让
// 出生尺寸大于保存值;实测本机 SM_CXFRAME 不含 SM_CXPADDEDBORDER(DWM 不可见
// 边框),须显式相加,否则冷启动尺寸偏差 8px。
func winFrameEstimate() (dx, dy int) {
	gsm := user32.NewProc("GetSystemMetrics")
	const (
		smCXFrame = 32
		smCYFrame = 33
		smCYCap   = 4
		smCXPad   = 92 // SM_CXPADDEDBORDER:DWM 不可见边框,SM_CXFRAME 不含它
	)
	r, _, _ := gsm.Call(smCXFrame)
	fx := int(int32(r))
	r, _, _ = gsm.Call(smCYFrame)
	fy := int(int32(r))
	r, _, _ = gsm.Call(smCYCap)
	cap := int(int32(r))
	r, _, _ = gsm.Call(smCXPad)
	pad := int(int32(r))
	return 2 * (fx + pad), 2*(fy+pad) + cap
}

// winSetForeground 把窗口提到最前并激活。Fyne 的 RequestFocus 只给输入焦点
// (viewport.Focus→SetFocus),不足以把后台/刚从托盘唤起的窗口拉到前台,故补:
// ShowWindow(SW_RESTORE)兜底还原最小化 + BringWindowToTop + SetForegroundWindow。
func winSetForeground(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	const swRestore = 9 // SW_RESTORE
	user32.NewProc("ShowWindow").Call(hwnd, uintptr(swRestore))
	user32.NewProc("BringWindowToTop").Call(hwnd)
	user32.NewProc("SetForegroundWindow").Call(hwnd)
}
