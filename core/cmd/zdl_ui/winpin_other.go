//go:build !windows

// 非 Windows 占位:位置钉位仅 Windows(Win32 线程钩子在 CreateWindowEx 前
// 改写 CREATESTRUCT 出生坐标,见 winpin_windows.go),Unix 构建保持可编译。
package main

func pinHookInstall()           {}
func pinHookUninstall()         {}
func pinTarget(hwnd uintptr)    {}
func pinBegin(x, y, w, h int32) {}
func pinEnd()                   {}
