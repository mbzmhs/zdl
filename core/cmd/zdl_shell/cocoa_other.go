//go:build !darwin

// 非 darwin 占位:runCocoaLoop 是 darwin 专属的 Cocoa 主线程循环(托盘/
// 悬浮窗的 dispatch_async 块全靠它落地,真实现见 cocoa_darwin.go)。main.go
// 用 runtime.GOOS 运行时分支调用它,符号却只存在于 darwin 构建——若其余
// 平台没有同名桩,Windows/Linux 上整个包编译不过(undefined symbol,
// 2026-11 实测合并后构建即在此失败)。本桩仅供编译,运行时不可达:
// main.go 的 darwin 分支在非 darwin 上不会进入,主 goroutine 直跑 runShell。
package main

func runCocoaLoop() {}
