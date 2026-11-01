//go:build !windows

// 非 Windows 占位:原生壳当前仅 Windows 交付,Unix 构建保持可编译
package main

import "fyne.io/fyne/v2"

func nativeHWND(w fyne.Window) uintptr                   { return 0 }
func winGetRect(hwnd uintptr) (int, int, int, int, bool) { return 0, 0, 0, 0, false }
func winSetRect(hwnd uintptr, x, y, w, h int)            {}
func winSetForeground(hwnd uintptr)                      {}
func winIsVisible(hwnd uintptr) bool                     { return hwnd != 0 }

// winPosOnScreen 非 Windows 无统一多屏信息:信任记忆位置,交由窗口管理器自行
// 摆位(与旧 Wails 壳 native_other.go 同语义)。
func winPosOnScreen(x, y, w, h int) bool { return true }

// winFrameEstimate 首次出生路径的外框尺寸差估算(outer − client)。macOS 标准
// 标题栏约 28pt、左右无边框;WinW/WinH 记忆值在 mac 上按同单位近似扣减,
// 尽力而为(复显路径 winGetRect 返回 ok=false,首次出生路径独走本估算)。
func winFrameEstimate() (dx, dy int) { return 0, 28 }
