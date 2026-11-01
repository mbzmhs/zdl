//go:build !darwin

// 非 darwin 无需窗级置前兜底:Windows 侧 ui.go 的 SetOnShown 已走
// winSetForeground(HWND 置前);Linux 窗口管理器自行处理。占位保持跨平台
// 可编译(macOS 真实现见 activate_darwin.go)。
package nativeui

func activateApp()         {}
func raiseWindowsToFront() {}
