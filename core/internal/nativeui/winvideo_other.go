//go:build !windows

// 非 Windows 占位:Darwin 接 NSView addSubview、Linux 接 X11/portal 时补齐
package nativeui

func makeVideoChild(parent uintptr) uintptr       { return 0 }
func moveVideoChild(hwnd uintptr, x, y, w, h int) {}
func destroyVideoChild(hwnd uintptr)              {}
