//go:build windows

// Fyne 窗口原生句柄获取(driver.NativeWindow.RunNative)。
// 回调在 Fyne 主线程执行;窗口未创建前调用会等到创建后回调,
// 调用方以 channel 同步取回。
package nativeui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver"
)

// NativeHWND 取窗口原生 HWND(0 = 不可用)
func NativeHWND(w fyne.Window) uintptr {
	ch := make(chan uintptr, 1)
	nw, ok := w.(driver.NativeWindow)
	if !ok {
		return 0
	}
	nw.RunNative(func(ctx any) {
		if wc, okc := ctx.(driver.WindowsWindowContext); okc {
			ch <- wc.HWND
			return
		}
		ch <- 0
	})
	return <-ch
}
