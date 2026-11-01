//go:build !windows

// 非 Windows 占位:最小化占位消息隔离仅 Windows(Fyne/GLFW 的窗口过程
// 子类化,修副屏最小化钳缩,见 winmin_windows.go)。macOS 的 NSWindow
// 最小化不经 WM_* 占位消息路径, GLFW 报给 Fyne 的就是真实尺寸,无此
// 问题;空实现保持非 Windows 构建可编译。
package main

func minSubclass(_ uintptr)   {}
func minUnsubclass(_ uintptr) {}
