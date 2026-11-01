//go:build darwin

// 常驻组件的 Cocoa 主线程循环。托盘(tray_darwin)与悬浮窗(floatwin_darwin)
// 的 UI 块都经 dispatch_async 派发到主线程主队列——1.8 时代由 Wails 的
// NSApplication 事件循环清队列,独立进程无循环,块会永远排队(实测托盘不
// 上屏)。本进程由主 goroutine 锁主线程专职跑 [NSApp run]:既服务主队列,
// 又处理托盘菜单的 NSEvent。Accessory 激活策略 = 无 Dock 图标、不出现在
// 应用切换器(纯托盘常驻语义;窗口在独立的 zdl_ui 进程,自带 Dock 存在)。
package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa

#import <Cocoa/Cocoa.h>

static void zdShellCocoaRun(void) {
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    [NSApp run]; // 阻塞至进程退出,永不返回
}
*/
import "C"

func runCocoaLoop() { C.zdShellCocoaRun() }
