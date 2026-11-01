//go:build windows

// 视频子窗口 glue:libvlc 渲染面的宿主(STATIC 子窗口)。
// 线程契约(spike 实测沉淀):
//   - 子窗口必须在"持续泵消息的线程"(Fyne 主线程)创建——VLC vout 线程
//     会向它发同步消息,创建线程不泵消息=窗口未响应、Stop 死锁;
//   - 创建/移动/销毁全部经 fyne.Do(AndWait) 派发到主线程;
//   - 生命周期:播放中 Create → MoveWindow 跟随布局 →
//     Stop(vlc) → Release(vlc) → DestroyWindow(顺序不可倒)。
package nativeui

import (
	"log"
	"syscall"
	"unsafe"
)

const (
	wsChildStatic = 0x40000000 | 0x10000000 // WS_CHILD|WS_VISIBLE
)

var (
	user32V            = syscall.NewLazyDLL("user32.dll")
	procCreateWindowEx = user32V.NewProc("CreateWindowExW")
	procMoveWindow     = user32V.NewProc("MoveWindow")
	procDestroyWindow  = user32V.NewProc("DestroyWindow")
)

func utf16pV(s string) uintptr {
	p, _ := syscall.UTF16PtrFromString(s)
	return uintptr(unsafe.Pointer(p))
}

// makeVideoChild 在父窗口内创建 STATIC 子窗口,返回 HWND(0 = 失败)
func makeVideoChild(parent uintptr) uintptr {
	h, _, err := procCreateWindowEx.Call(
		0, utf16pV("STATIC"), utf16pV(""),
		wsChildStatic, 0, 0, 320, 180,
		parent, 0, 0, 0)
	if h == 0 {
		log.Println("[nativeui] 创建视频子窗口失败:", err)
	}
	return h
}

// moveVideoChild 移动/缩放子窗口(坐标相对父窗口客户区)
func moveVideoChild(hwnd uintptr, x, y, w, h int) {
	if hwnd == 0 || w <= 0 || h <= 0 {
		return
	}
	procMoveWindow.Call(hwnd, uintptr(int32(x)), uintptr(int32(y)),
		uintptr(int32(w)), uintptr(int32(h)), 1)
}

// destroyVideoChild 销毁子窗口(须在 vlc Stop 之后)
func destroyVideoChild(hwnd uintptr) {
	if hwnd != 0 {
		procDestroyWindow.Call(hwnd)
	}
}
