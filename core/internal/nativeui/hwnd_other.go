//go:build !windows

// 非 Windows 占位:Darwin 接 NSWindow 句柄时补齐
package nativeui

import "fyne.io/fyne/v2"

// NativeHWND 取窗口原生句柄(非 Windows 恒 0)
func NativeHWND(w fyne.Window) uintptr { return 0 }
